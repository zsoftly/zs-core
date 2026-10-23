//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/keycloakaudit"
	"github.com/zsoftly/zs-core/pkg/scope"
)

const keycloakAuditPGTimestamp = int64(1760000000)

var errKeycloakAuditPGInjected = errors.New("injected audit insert failure")

// keycloakAuditPGDB keeps the production receiver on a real PostgreSQL
// transaction while exposing two test-only synchronization points. The
// receiver inserts a receipt through QueryRow before it writes the audit row.
type keycloakAuditPGDB struct {
	httpapi.DB
	failAudit  atomic.Bool
	sawReceipt atomic.Bool
	barrier    *keycloakAuditPGBarrier
}

func (db *keycloakAuditPGDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := db.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &keycloakAuditPGTx{Tx: tx, db: db}, nil
}

type keycloakAuditPGTx struct {
	pgx.Tx
	db *keycloakAuditPGDB
}

func (tx *keycloakAuditPGTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO keycloak_audit_receipts") && tx.db.barrier != nil {
		if err := tx.db.barrier.wait(ctx); err != nil {
			return keycloakAuditPGErrorRow{err: err}
		}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *keycloakAuditPGTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !tx.db.failAudit.Load() || !strings.Contains(sql, "INSERT INTO audit_events") {
		return tx.Tx.Exec(ctx, sql, args...)
	}
	var receipts int
	if err := tx.Tx.QueryRow(ctx, `SELECT count(*) FROM keycloak_audit_receipts`).Scan(&receipts); err != nil {
		return pgconn.CommandTag{}, err
	}
	tx.db.sawReceipt.Store(receipts == 1)
	return pgconn.CommandTag{}, errKeycloakAuditPGInjected
}

type keycloakAuditPGErrorRow struct{ err error }

func (r keycloakAuditPGErrorRow) Scan(...any) error { return r.err }

type keycloakAuditPGBarrier struct {
	arrived chan struct{}
	release chan struct{}
}

func (b *keycloakAuditPGBarrier) wait(ctx context.Context) error {
	select {
	case b.arrived <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type keycloakAuditPGFixture struct {
	t      *testing.T
	ctx    context.Context
	cancel context.CancelFunc
	db     *postgrestest.DB
	store  *keycloakAuditPGDB
	srv    *httptest.Server
	client *http.Client
	now    time.Time
	secret map[string][]byte
	brands map[string]idgen.UUID
}

func newKeycloakAuditPGFixture(t *testing.T) *keycloakAuditPGFixture {
	t.Helper()
	db := postgrestest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	f := &keycloakAuditPGFixture{
		t: t, ctx: ctx, cancel: cancel, db: db, now: time.Unix(keycloakAuditPGTimestamp, 0).UTC(),
		store:  &keycloakAuditPGDB{DB: db.Pool},
		client: &http.Client{Timeout: 5 * time.Second},
		secret: map[string][]byte{}, brands: map[string]idgen.UUID{},
	}
	t.Cleanup(cancel)

	// Every case has two active, applied realms. The disabled and unapplied
	// rows exercise the receiver's realm eligibility lookup independently of
	// HMAC selection.
	f.insertBrand("realm-alpha", "active", true)
	f.insertBrand("realm-beta", "active", true)
	f.insertBrand("realm-disabled", "disabled", true)
	f.insertBrand("realm-unapplied", "active", false)
	f.secret["realm-unknown"] = []byte("unknown-realm-test-secret-012345678")
	f.srv = httptest.NewServer(httpapi.New(httpapi.Options{
		DB: f.store, Migrations: migrations.FS, KeycloakAuditSecrets: f.secret,
		KeycloakAuditSourceID: "keycloak", Now: func() time.Time { return f.now },
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *keycloakAuditPGFixture) insertBrand(realm, status string, applied bool) idgen.UUID {
	f.t.Helper()
	id := idgen.MustNewV7()
	slug := "audit-" + strings.TrimPrefix(realm, "realm-")
	var appliedAt any
	if applied {
		appliedAt = f.now
	}
	if _, err := f.db.Pool.Exec(f.ctx, `INSERT INTO brands (id, slug, name, status, keycloak_realm, killbill_api_key,
    killbill_api_secret_ref, manifest, manifest_digest, applied_at)
VALUES ($1, $2, $3, $4, $5, $6, 'env://BRAND_AUDIT_KB', '{}'::jsonb, 'sha256:' || repeat('0', 64), $7)`,
		id, slug, "Audit "+realm, status, realm, "kb-"+slug, appliedAt); err != nil {
		f.t.Fatal(err)
	}
	f.brands[realm] = id
	f.secret[realm] = []byte("keycloak-audit-test-secret-for-" + realm)
	return id
}

func (f *keycloakAuditPGFixture) body(realm, kind, eventID string) []byte {
	f.t.Helper()
	e := keycloakaudit.Envelope{
		Version: 1, SourceID: "keycloak", EventID: eventID, RealmID: "opaque-realm-id-" + realm,
		RealmName: realm, Kind: kind, OccurredAtMS: f.now.Add(-time.Minute).UnixMilli(),
	}
	switch kind {
	case "user":
		e.EventType, e.SubjectID, e.ClientID = "LOGIN", "opaque-subject", "opaque-client"
	case "admin":
		e.Operation, e.ResourceType = "CREATE", "USER"
		e.AdminActorRealmID, e.AdminActorUserID, e.AdminActorClientID = "opaque-admin-realm", "opaque-admin-user", "opaque-admin-client"
	default:
		f.t.Fatalf("unsupported test kind %q", kind)
	}
	body, err := json.Marshal(e)
	if err != nil {
		f.t.Fatal(err)
	}
	return body
}

func (f *keycloakAuditPGFixture) post(ctx context.Context, client *http.Client, realm string, body []byte) (int, error) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+keycloakaudit.Path, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	timestamp := strconv.FormatInt(f.now.Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ZS-Keycloak-Realm", realm)
	req.Header.Set("X-ZS-Keycloak-Timestamp", timestamp)
	req.Header.Set("X-ZS-Keycloak-Signature", keycloakaudit.DeliverySignature(f.secret[realm], timestamp, body))
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

func (f *keycloakAuditPGFixture) requireStatus(realm string, body []byte, want int) {
	f.t.Helper()
	status, err := f.post(f.ctx, f.client, realm, body)
	if err != nil || status != want {
		f.t.Fatalf("POST %s = %d %v, want %d", realm, status, err, want)
	}
}

func (f *keycloakAuditPGFixture) counts(realm, kind, eventID string) (int, int) {
	f.t.Helper()
	brand := f.brands[realm]
	var receipts, events int
	if err := f.db.Pool.QueryRow(f.ctx, `SELECT count(*) FROM keycloak_audit_receipts WHERE brand_id=$1 AND realm_name=$2 AND kind=$3 AND event_id=$4`, brand, realm, kind, eventID).Scan(&receipts); err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.Pool.QueryRow(f.ctx, `SELECT count(*) FROM audit_events WHERE brand_id=$1 AND tenant_id IS NULL AND action=$2 AND correlation_id=$3`, brand, "identity.keycloak."+kind, eventID).Scan(&events); err != nil {
		f.t.Fatal(err)
	}
	return receipts, events
}

func (f *keycloakAuditPGFixture) receipt(realm, kind, eventID string) ([]byte, time.Time) {
	f.t.Helper()
	var digest []byte
	var receivedAt time.Time
	if err := f.db.Pool.QueryRow(f.ctx, `SELECT body_digest, received_at FROM keycloak_audit_receipts WHERE brand_id=$1 AND realm_name=$2 AND kind=$3 AND event_id=$4`, f.brands[realm], realm, kind, eventID).Scan(&digest, &receivedAt); err != nil {
		f.t.Fatal(err)
	}
	return digest, receivedAt
}

func (f *keycloakAuditPGFixture) requireReceiptDigest(realm, kind, eventID string, body []byte) ([]byte, time.Time) {
	f.t.Helper()
	digest, receivedAt := f.receipt(realm, kind, eventID)
	want := sha256.Sum256(body)
	if !bytes.Equal(digest, want[:]) {
		f.t.Fatalf("receipt digest %s = %x, want %x", eventID, digest, want)
	}
	return digest, receivedAt
}

func TestKeycloakAuditReceiverStoresSignedBrandEvents(t *testing.T) {
	f := newKeycloakAuditPGFixture(t)
	userID, adminID := "evt-user-alpha", "evt-admin-beta"
	userBody := f.body("realm-alpha", "user", userID)
	adminBody := f.body("realm-beta", "admin", adminID)
	var failedAdmin keycloakaudit.Envelope
	if err := json.Unmarshal(adminBody, &failedAdmin); err != nil {
		t.Fatal(err)
	}
	failedAdmin.Failed = true
	adminBody, err := json.Marshal(failedAdmin)
	if err != nil {
		t.Fatal(err)
	}
	f.requireStatus("realm-alpha", userBody, http.StatusNoContent)
	f.requireStatus("realm-beta", adminBody, http.StatusNoContent)

	for _, c := range []struct {
		realm, kind, eventID string
		wantDetails          map[string]any
		body                 []byte
		outcome              audit.Outcome
	}{
		{"realm-alpha", "user", userID, map[string]any{"realm_id": "opaque-realm-id-realm-alpha", "event_type": "LOGIN", "subject_id": "opaque-subject", "client_id": "opaque-client"}, userBody, audit.OutcomeSuccess},
		{"realm-beta", "admin", adminID, map[string]any{"realm_id": "opaque-realm-id-realm-beta", "operation": "CREATE", "resource_type": "USER", "admin_actor_realm_id": "opaque-admin-realm", "admin_actor_user_id": "opaque-admin-user", "admin_actor_client_id": "opaque-admin-client"}, adminBody, audit.OutcomeFailure},
	} {
		receipts, auditCount := f.counts(c.realm, c.kind, c.eventID)
		if receipts != 1 || auditCount != 1 {
			t.Fatalf("%s counts = receipts:%d events:%d", c.eventID, receipts, auditCount)
		}
		f.requireReceiptDigest(c.realm, c.kind, c.eventID, c.body)
		page, err := audit.QueryBrand(f.ctx, f.db.Pool, f.brands[c.realm], audit.Filter{})
		if err != nil || len(page.Events) != 1 {
			t.Fatalf("brand query %s = %+v %v", c.realm, page, err)
		}
		e := page.Events[0]
		if e.TenantID != "" || e.Action != "identity.keycloak."+c.kind || e.CorrelationID != c.eventID || e.Actor.Type != events.ActorProvider || e.Actor.ID != "keycloak:"+c.realm || e.Outcome != c.outcome || !e.OccurredAt.Equal(f.now.Add(-time.Minute)) {
			t.Fatalf("brand event %s = %+v", c.eventID, e)
		}
		if !equalKeycloakAuditPGDetails(e.Details, c.wantDetails) {
			t.Fatalf("details %s = %#v, want %#v", c.eventID, e.Details, c.wantDetails)
		}
		tenantScope := scope.Scope{Brand: f.brands[c.realm], Tenant: idgen.MustNewV7()}
		tenantPage, err := audit.Query(f.ctx, f.db.Pool, tenantScope, audit.Filter{})
		if err != nil || len(tenantPage.Events) != 0 {
			t.Fatalf("tenant query included brand event: %+v %v", tenantPage, err)
		}
	}

	alpha, err := audit.QueryBrand(f.ctx, f.db.Pool, f.brands["realm-alpha"], audit.Filter{})
	if err != nil || len(alpha.Events) != 1 || alpha.Events[0].CorrelationID != userID {
		t.Fatalf("cross-brand query = %+v %v", alpha, err)
	}
}

func TestKeycloakAuditReceiverDeniesUnknownDisabledAndUnappliedRealms(t *testing.T) {
	f := newKeycloakAuditPGFixture(t)
	for _, realm := range []string{"realm-unknown", "realm-disabled", "realm-unapplied"} {
		f.requireStatus(realm, f.body(realm, "user", "evt-denied-"+realm), http.StatusNotFound)
	}
	var receipts, events int
	if err := f.db.Pool.QueryRow(f.ctx, `SELECT count(*) FROM keycloak_audit_receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Pool.QueryRow(f.ctx, `SELECT count(*) FROM audit_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 || events != 0 {
		t.Fatalf("denied realms persisted receipts:%d events:%d", receipts, events)
	}
}

func TestKeycloakAuditReceiverRetriesAndConflicts(t *testing.T) {
	f := newKeycloakAuditPGFixture(t)
	body := f.body("realm-alpha", "user", "evt-retry")
	f.requireStatus("realm-alpha", body, http.StatusNoContent)
	firstDigest, firstReceivedAt := f.requireReceiptDigest("realm-alpha", "user", "evt-retry", body)
	f.requireStatus("realm-alpha", body, http.StatusNoContent)
	secondDigest, secondReceivedAt := f.requireReceiptDigest("realm-alpha", "user", "evt-retry", body)
	if !bytes.Equal(firstDigest, secondDigest) || !firstReceivedAt.Equal(secondReceivedAt) {
		t.Fatalf("same-body retry changed receipt: digest %x/%x received_at %s/%s", firstDigest, secondDigest, firstReceivedAt, secondReceivedAt)
	}
	if receipts, events := f.counts("realm-alpha", "user", "evt-retry"); receipts != 1 || events != 1 {
		t.Fatalf("same-body retry counts = receipts:%d events:%d", receipts, events)
	}

	conflict := f.body("realm-alpha", "user", "evt-conflict")
	f.requireStatus("realm-alpha", conflict, http.StatusNoContent)
	conflictDigest, conflictReceivedAt := f.requireReceiptDigest("realm-alpha", "user", "evt-conflict", conflict)
	var changed keycloakaudit.Envelope
	if err := json.Unmarshal(conflict, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Failed = true
	different, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	f.requireStatus("realm-alpha", different, http.StatusConflict)
	afterConflictDigest, afterConflictReceivedAt := f.requireReceiptDigest("realm-alpha", "user", "evt-conflict", conflict)
	if !bytes.Equal(conflictDigest, afterConflictDigest) || !conflictReceivedAt.Equal(afterConflictReceivedAt) {
		t.Fatalf("conflicting retry changed receipt: digest %x/%x received_at %s/%s", conflictDigest, afterConflictDigest, conflictReceivedAt, afterConflictReceivedAt)
	}
	if receipts, events := f.counts("realm-alpha", "user", "evt-conflict"); receipts != 1 || events != 1 {
		t.Fatalf("conflict counts = receipts:%d events:%d", receipts, events)
	}
}

type keycloakAuditPGLostResponseTransport struct {
	base http.RoundTripper
	once sync.Once
}

func (t *keycloakAuditPGLostResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusNoContent {
		return resp, nil
	}
	drop := false
	t.once.Do(func() { drop = true })
	if !drop {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return nil, errors.New("deliberately discarded successful response")
}

func TestKeycloakAuditReceiverLostResponseRetryIsIdempotent(t *testing.T) {
	f := newKeycloakAuditPGFixture(t)
	body := f.body("realm-alpha", "user", "evt-lost-response")
	lost := &http.Client{Timeout: 5 * time.Second, Transport: &keycloakAuditPGLostResponseTransport{base: http.DefaultTransport}}
	if _, err := f.post(f.ctx, lost, "realm-alpha", body); err == nil {
		t.Fatal("lost response client call succeeded")
	}
	if receipts, events := f.counts("realm-alpha", "user", "evt-lost-response"); receipts != 1 || events != 1 {
		t.Fatalf("lost response first delivery counts = receipts:%d events:%d", receipts, events)
	}
	firstDigest, firstReceivedAt := f.requireReceiptDigest("realm-alpha", "user", "evt-lost-response", body)
	f.requireStatus("realm-alpha", body, http.StatusNoContent)
	secondDigest, secondReceivedAt := f.requireReceiptDigest("realm-alpha", "user", "evt-lost-response", body)
	if !bytes.Equal(firstDigest, secondDigest) || !firstReceivedAt.Equal(secondReceivedAt) {
		t.Fatalf("lost-response retry changed receipt: digest %x/%x received_at %s/%s", firstDigest, secondDigest, firstReceivedAt, secondReceivedAt)
	}
	if receipts, events := f.counts("realm-alpha", "user", "evt-lost-response"); receipts != 1 || events != 1 {
		t.Fatalf("lost-response retry counts = receipts:%d events:%d", receipts, events)
	}
}

func TestKeycloakAuditReceiverConcurrentSameIDConverges(t *testing.T) {
	f := newKeycloakAuditPGFixture(t)
	body := f.body("realm-alpha", "user", "evt-concurrent")
	barrier := &keycloakAuditPGBarrier{arrived: make(chan struct{}, 4), release: make(chan struct{})}
	f.store.barrier = barrier
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	var wg sync.WaitGroup
	results := make(chan struct {
		status int
		err    error
	}, 4)
	wg.Add(4)
	for range 4 {
		go func() {
			defer wg.Done()
			status, err := f.post(ctx, f.client, "realm-alpha", body)
			results <- struct {
				status int
				err    error
			}{status, err}
		}()
	}
	t.Cleanup(func() {
		release()
		cancel()
		wg.Wait()
	})
	for range 4 {
		select {
		case <-barrier.arrived:
		case <-ctx.Done():
			t.Fatalf("same-ID requests did not all reach receipt barrier: %v", ctx.Err())
		}
	}
	release()
	wg.Wait()
	cancel()
	for range 4 {
		result := <-results
		if result.err != nil || result.status != http.StatusNoContent {
			t.Fatalf("concurrent delivery = %d %v", result.status, result.err)
		}
	}
	if receipts, events := f.counts("realm-alpha", "user", "evt-concurrent"); receipts != 1 || events != 1 {
		t.Fatalf("concurrent counts = receipts:%d events:%d", receipts, events)
	}
	f.requireReceiptDigest("realm-alpha", "user", "evt-concurrent", body)
}

func TestKeycloakAuditReceiverRollsBackReceiptWhenAuditWriteFails(t *testing.T) {
	f := newKeycloakAuditPGFixture(t)
	body := f.body("realm-alpha", "user", "evt-rollback")
	f.store.failAudit.Store(true)
	f.requireStatus("realm-alpha", body, http.StatusInternalServerError)
	if !f.store.sawReceipt.Load() {
		t.Fatal("audit failure did not observe the receipt inside the receiver transaction")
	}
	if receipts, events := f.counts("realm-alpha", "user", "evt-rollback"); receipts != 0 || events != 0 {
		t.Fatalf("failed audit write retained receipts:%d events:%d", receipts, events)
	}
	f.store.failAudit.Store(false)
	f.requireStatus("realm-alpha", body, http.StatusNoContent)
	f.requireReceiptDigest("realm-alpha", "user", "evt-rollback", body)
	if receipts, events := f.counts("realm-alpha", "user", "evt-rollback"); receipts != 1 || events != 1 {
		t.Fatalf("retry after rollback counts = receipts:%d events:%d", receipts, events)
	}
}

func equalKeycloakAuditPGDetails(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			return false
		}
	}
	return true
}
