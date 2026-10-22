//go:build integration

package notifications_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/notifications"
	"github.com/zsoftly/zs-core/pkg/outbox"
)

type refs struct{}

func (refs) Check(slug, ref string) error                            { return nil }
func (refs) Resolve(context.Context, string, string) (string, error) { return "test-secret", nil }

type preflightRefs struct {
	checked  []string
	resolves int
}

func (r *preflightRefs) Check(slug, ref string) error {
	r.checked = append(r.checked, slug+":"+ref)
	return nil
}

func (r *preflightRefs) Resolve(context.Context, string, string) (string, error) {
	r.resolves++
	return "", errors.New("preflight must not resolve webhook secrets")
}

type blockingRefs struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingRefs) Check(string, string) error { return nil }

func (r *blockingRefs) Resolve(_ context.Context, _ string, ref string) (string, error) {
	if strings.HasSuffix(ref, "__B") {
		r.once.Do(func() { close(r.started) })
		<-r.release
	}
	return "test-secret", nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFanoutRecordsZeroEndpointReceiptBeforeEndpointsExist(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	brandID := idgen.MustNewV7()
	initial := &brand.Manifest{}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO brands (id,slug,name,keycloak_realm,killbill_api_key,killbill_api_secret_ref,manifest,manifest_digest,applied_at) VALUES ($1,'zs-zero','zero','zero','zero','env://BRAND_ZS_ZERO__KB',$2,'sha256:' || repeat('0',64),now())`, brandID, mustJSON(t, initial)); err != nil {
		t.Fatal(err)
	}
	event, err := events.New(ctx, events.Params{Type: "tenant.created", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: brandID, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "one"}, CorrelationID: "zero", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if err := outbox.Write(ctx, tx, event); err != nil {
			return err
		}
		return (&notifications.Service{}).Fanout(ctx, tx, event)
	}); err != nil {
		t.Fatal(err)
	}
	updated := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{{URL: "https://receiver.example/new", SecretRef: "env://BRAND_ZS_ZERO__NEW"}}}}
	if _, err := db.Pool.Exec(ctx, `UPDATE brands SET manifest=$2 WHERE id=$1`, brandID, mustJSON(t, updated)); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return (&notifications.Service{Secrets: refs{}}).Fanout(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("historical delivery count=%d err=%v", count, err)
	}
}

func TestFirstFanoutUsesEndpointCurrentWhenQueuedSourceIsFirstProcessed(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	brandID := idgen.MustNewV7()
	initial := &brand.Manifest{}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO brands (id,slug,name,keycloak_realm,killbill_api_key,killbill_api_secret_ref,manifest,manifest_digest,applied_at) VALUES ($1,'zs-first','first','first','first','env://BRAND_ZS_FIRST__KB',$2,'sha256:' || repeat('0',64),now())`, brandID, mustJSON(t, initial)); err != nil {
		t.Fatal(err)
	}
	event, err := events.New(ctx, events.Params{Type: "tenant.created", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: brandID, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "one"}, CorrelationID: "first", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
	first := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{
		{URL: "https://receiver.example/first", SecretRef: "env://BRAND_ZS_FIRST__FIRST", Events: []string{"tenant.created"}},
		{URL: "https://receiver.example/filtered", SecretRef: "env://BRAND_ZS_FIRST__FILTERED", Events: []string{"tenant.deleted"}},
	}}}
	if _, err := db.Pool.Exec(ctx, `UPDATE brands SET manifest=$2 WHERE id=$1`, brandID, mustJSON(t, first)); err != nil {
		t.Fatal(err)
	}
	service := &notifications.Service{Secrets: refs{}}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return service.Fanout(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
	var deliveries int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("first fanout deliveries=%d err=%v", deliveries, err)
	}
	widened := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{
		{URL: "https://receiver.example/first", SecretRef: "env://BRAND_ZS_FIRST__FIRST", Events: []string{"tenant.created"}},
		{URL: "https://receiver.example/filtered", SecretRef: "env://BRAND_ZS_FIRST__FILTERED"},
		{URL: "https://receiver.example/added", SecretRef: "env://BRAND_ZS_FIRST__ADDED"},
	}}}
	if _, err := db.Pool.Exec(ctx, `UPDATE brands SET manifest=$2 WHERE id=$1`, brandID, mustJSON(t, widened)); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return service.Fanout(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("receipt replay deliveries=%d err=%v", deliveries, err)
	}
}

func TestValidateEndpointsRejectsInvalidStoredConfigurationWithoutMutation(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	validID, invalidID := idgen.MustNewV7(), idgen.MustNewV7()
	valid := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{{URL: "https://receiver.example/valid", SecretRef: "env://BRAND_ZS_VALID__HOOK"}}}}
	invalidURL := "http://user:credential@receiver.example/unsafe?secret=query"
	invalid := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{{URL: invalidURL, SecretRef: "env://BRAND_ZS_INVALID__HOOK"}}}}
	for _, row := range []struct {
		id       idgen.UUID
		slug     string
		manifest *brand.Manifest
	}{{validID, "zs-valid", valid}, {invalidID, "zs-z-invalid", invalid}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO brands (id,slug,name,keycloak_realm,killbill_api_key,killbill_api_secret_ref,manifest,manifest_digest,applied_at) VALUES ($1,$2,$2,$2,$2,'env://BRAND_TEST__KB',$3,'sha256:' || repeat('0',64),now())`, row.id, row.slug, mustJSON(t, row.manifest)); err != nil {
			t.Fatal(err)
		}
	}
	event, err := events.New(ctx, events.Params{Type: "tenant.created", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: invalidID, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "one"}, CorrelationID: "preflight", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
	resolver := &preflightRefs{}
	err = (&notifications.Service{DB: db.Pool, Secrets: resolver}).ValidateEndpoints(ctx)
	if err == nil || !strings.Contains(err.Error(), "zs-z-invalid") || strings.Contains(err.Error(), invalidURL) || strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "query") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe validation error: %v", err)
	}
	var attempts int
	var published, tokenUnset, leaseUnset bool
	if err := db.Pool.QueryRow(ctx, `SELECT dispatch_attempts, published_at IS NOT NULL, claim_token IS NULL, claimed_until IS NULL FROM outbox WHERE id=$1`, event.StorageID()).Scan(&attempts, &published, &tokenUnset, &leaseUnset); err != nil || attempts != 0 || published || !tokenUnset || !leaseUnset {
		t.Fatalf("preflight changed source event: attempts=%d published=%v token_unset=%v lease_unset=%v err=%v", attempts, published, tokenUnset, leaseUnset, err)
	}
	var deliveries int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("preflight deliveries=%d err=%v", deliveries, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE brands SET manifest=$2 WHERE id=$1`, invalidID, mustJSON(t, &brand.Manifest{})); err != nil {
		t.Fatal(err)
	}
	if err := (&notifications.Service{DB: db.Pool, Secrets: resolver}).ValidateEndpoints(ctx); err != nil {
		t.Fatalf("validation after manifest repair: %v", err)
	}
	if len(resolver.checked) != 2 || resolver.checked[0] != "zs-valid:env://BRAND_ZS_VALID__HOOK" || resolver.checked[1] != "zs-valid:env://BRAND_ZS_VALID__HOOK" {
		t.Fatalf("preflight checks=%v", resolver.checked)
	}
	if resolver.resolves != 0 {
		t.Fatalf("preflight resolved %d secret values", resolver.resolves)
	}
}

func TestValidateEndpointsRejectsCanonicalDuplicateStoredDestinations(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	brandID := idgen.MustNewV7()
	manifest := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{
		{URL: "https://EXAMPLE.test./%68ook", SecretRef: "env://BRAND_ZS_DUPLICATE__ONE"},
		{URL: "https://example.test/hook", SecretRef: "env://BRAND_ZS_DUPLICATE__TWO"},
	}}}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO brands (id,slug,name,keycloak_realm,killbill_api_key,killbill_api_secret_ref,manifest,manifest_digest,applied_at) VALUES ($1,'zs-duplicate','duplicate','duplicate','duplicate','env://BRAND_ZS_DUPLICATE__KB',$2,'sha256:' || repeat('0',64),now())`, brandID, mustJSON(t, manifest)); err != nil {
		t.Fatal(err)
	}
	event, err := events.New(ctx, events.Params{Type: "tenant.created", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: brandID, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "one"}, CorrelationID: "duplicate", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
	err = (&notifications.Service{DB: db.Pool, Secrets: refs{}}).ValidateEndpoints(ctx)
	if err == nil || !strings.Contains(err.Error(), "zs-duplicate") || strings.Contains(err.Error(), "EXAMPLE") || strings.Contains(err.Error(), "%68") {
		t.Fatalf("unsafe duplicate validation error: %v", err)
	}
	var attempts, receipts int
	var published, tokenUnset, leaseUnset bool
	if err := db.Pool.QueryRow(ctx, `SELECT dispatch_attempts, published_at IS NOT NULL, claim_token IS NULL, claimed_until IS NULL FROM outbox WHERE id=$1`, event.StorageID()).Scan(&attempts, &published, &tokenUnset, &leaseUnset); err != nil || attempts != 0 || published || !tokenUnset || !leaseUnset {
		t.Fatalf("duplicate preflight changed source event: attempts=%d published=%v token_unset=%v lease_unset=%v err=%v", attempts, published, tokenUnset, leaseUnset, err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_fanouts WHERE event_id=$1`, event.StorageID()).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("duplicate preflight receipts=%d err=%v", receipts, err)
	}
}

func TestFanoutDoesNotDeliverInternalEvent(t *testing.T) {
	db, event := webhookFixture(t, []brand.Webhook{{URL: "https://receiver.example/internal", SecretRef: "env://BRAND_ZS_FIXTURE__INTERNAL"}})
	for _, eventType := range outbox.InternalEventTypes {
		internal := event
		internal.Type = eventType
		internal.ID = idgen.Format(idgen.Event, idgen.MustNewV7())
		if err := internal.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := pgx.BeginFunc(context.Background(), db.Pool, func(tx pgx.Tx) error {
			if err := outbox.Write(context.Background(), tx, internal); err != nil {
				return err
			}
			return (&notifications.Service{Secrets: refs{}}).Fanout(context.Background(), tx, internal)
		}); err != nil {
			t.Fatal(err)
		}
		var receipts, deliveries int
		if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_fanouts WHERE event_id=$1`, internal.StorageID()).Scan(&receipts); err != nil || receipts != 1 {
			t.Fatalf("%s receipts=%d err=%v", eventType, receipts, err)
		}
		if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, internal.StorageID()).Scan(&deliveries); err != nil || deliveries != 0 {
			t.Fatalf("%s deliveries=%d err=%v", eventType, deliveries, err)
		}
	}
}

func TestGenericWebhookRelayLeavesInternalEventsForTypedConsumers(t *testing.T) {
	db, public := webhookFixture(t, []brand.Webhook{{URL: "https://receiver.example/public", SecretRef: "env://BRAND_ZS_FIXTURE__PUBLIC"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	internals := make([]events.Envelope, 0, len(outbox.InternalEventTypes))
	if err := pgx.BeginFunc(context.Background(), db.Pool, func(tx pgx.Tx) error {
		for _, eventType := range outbox.InternalEventTypes {
			internal := public
			internal.Type = eventType
			internal.ID = idgen.Format(idgen.Event, idgen.MustNewV7())
			if err := internal.Validate(); err != nil {
				return err
			}
			if err := outbox.Write(ctx, tx, internal); err != nil {
				return err
			}
			internals = append(internals, internal)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := &notifications.Service{DB: db.Pool, Secrets: refs{}}
	if count, err := outbox.NewRelay("webhook-test", outbox.NewPostgresReader(db.Pool), service.FanoutHandler(), 10, 3, nil).RunOnce(ctx); err != nil || count != 1 {
		t.Fatalf("generic webhook relay = %d, %v", count, err)
	}
	for _, internal := range internals {
		var attempts int
		var published, tokenUnset, leaseUnset bool
		if err := db.Pool.QueryRow(ctx, `SELECT dispatch_attempts, published_at IS NOT NULL, claim_token IS NULL, claimed_until IS NULL FROM outbox WHERE id=$1`, internal.StorageID()).Scan(&attempts, &published, &tokenUnset, &leaseUnset); err != nil || attempts != 0 || published || !tokenUnset || !leaseUnset {
			t.Fatalf("internal %s changed by generic relay: attempts=%d published=%v token_unset=%v lease_unset=%v err=%v", internal.Type, attempts, published, tokenUnset, leaseUnset, err)
		}
		consumed := 0
		typed := outbox.NewRelay("typed-test", outbox.NewPostgresReader(db.Pool).WithEventTypes(internal.Type), outbox.HandlerFunc(func(_ context.Context, got events.Envelope) error {
			if got.StorageID() != internal.StorageID() {
				return fmt.Errorf("typed consumer received %s, want %s", got.ID, internal.ID)
			}
			consumed++
			return nil
		}), 10, 3, nil)
		if count, err := typed.RunOnce(ctx); err != nil || count != 1 || consumed != 1 {
			t.Fatalf("typed consumer for %s = %d, %v, consumed=%d", internal.Type, count, err, consumed)
		}
	}
}

func TestFanoutSnapshotsMatchesAndReconcileCancelsRemovedEndpoints(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	brandID := idgen.MustNewV7()
	manifest := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{
		{URL: "https://receiver.example/a", SecretRef: "env://BRAND_ZS_HOOK__A", Events: []string{"tenant.created"}},
		{URL: "https://receiver.example/b", SecretRef: "env://BRAND_ZS_HOOK__B", Events: []string{"tenant.deleted"}},
	}}}
	encoded, _ := json.Marshal(manifest)
	if _, err := db.Pool.Exec(ctx, `INSERT INTO brands (id,slug,name,keycloak_realm,killbill_api_key,killbill_api_secret_ref,manifest,manifest_digest,applied_at) VALUES ($1,'zs-hook','hook','hook','hook','env://BRAND_ZS_HOOK__KB',$2,'sha256:' || repeat('0',64),now())`, brandID, encoded); err != nil {
		t.Fatal(err)
	}
	event, err := events.New(ctx, events.Params{Type: "tenant.created", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: brandID, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "one"}, CorrelationID: "test", Payload: map[string]string{"x": "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if err := outbox.Write(ctx, tx, event); err != nil {
			return err
		}
		return (&notifications.Service{Secrets: refs{}}).Fanout(ctx, tx, event)
	}); err != nil {
		t.Fatal(err)
	}
	var deliveries int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("deliveries = %d, %v", deliveries, err)
	}
	var receipts int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_fanouts WHERE event_id=$1`, event.StorageID()).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("fanout receipts = %d, %v", receipts, err)
	}
	record, err := brand.Get(ctx, db.Pool, brandID)
	if err != nil {
		t.Fatal(err)
	}
	record.Manifest.Notifications.Webhooks = nil
	if _, err := db.Pool.Exec(ctx, `UPDATE brands SET manifest=$2 WHERE id=$1`, brandID, mustJSON(t, record.Manifest)); err != nil {
		t.Fatal(err)
	}
	// A source outbox retry after a manifest change must not fan out to the
	// new endpoint set. The receipt makes the second fanout a no-op.
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		return (&notifications.Service{Secrets: refs{}}).Fanout(ctx, tx, event)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("retry expanded deliveries = %d, %v", deliveries, err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		current, err := brand.Get(ctx, tx, brandID)
		if err != nil {
			return err
		}
		_, err = (&notifications.Service{}).Reconcile(ctx, tx, current, audit.Entry{Actor: events.Actor{Type: events.ActorSystem}, Reason: "manifest update", Outcome: audit.OutcomeSuccess, CorrelationID: "test"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var canceled bool
	if err := db.Pool.QueryRow(ctx, `SELECT canceled_at IS NOT NULL FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&canceled); err != nil || !canceled {
		t.Fatalf("canceled = %v, %v", canceled, err)
	}
}

// A claim may contain several rows. B blocks while resolving its secret, while
// A proceeds. Apply then removes B, reconciles it, and re-adds B. The old
// claimed B row must still not send: re-adding config never resurrects it.
func TestClaimedEndpointRemovedAndReaddedDoesNotSend(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	brandID := idgen.MustNewV7()
	manifest := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{
		{URL: "https://receiver.example/a", SecretRef: "env://BRAND_ZS_TRACE__A"},
		{URL: "https://receiver.example/b", SecretRef: "env://BRAND_ZS_TRACE__B"},
	}}}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO brands (id,slug,name,keycloak_realm,killbill_api_key,killbill_api_secret_ref,manifest,manifest_digest,applied_at) VALUES ($1,'zs-trace','trace','trace','trace','env://BRAND_ZS_TRACE__KB',$2,'sha256:' || repeat('0',64),now())`, brandID, mustJSON(t, manifest)); err != nil {
		t.Fatal(err)
	}
	event, err := events.New(ctx, events.Params{Type: "tenant.created", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: brandID, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "one"}, CorrelationID: "trace", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if err := outbox.Write(ctx, tx, event); err != nil {
			return err
		}
		return (&notifications.Service{Secrets: refs{}}).Fanout(ctx, tx, event)
	}); err != nil {
		t.Fatal(err)
	}
	blockedB, releaseB := make(chan struct{}), make(chan struct{})
	aDelivered := make(chan struct{})
	var mu sync.Mutex
	var requests []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/a" {
			close(aDelivered)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	resolver := &blockingRefs{started: blockedB, release: releaseB}
	svc := &notifications.Service{DB: db.Pool, Secrets: resolver, HTTP: client}
	done := make(chan error, 1)
	go func() { _, err := svc.RunOnce(ctx, 10); done <- err }()
	select {
	case <-blockedB:
	case <-time.After(2 * time.Second):
		t.Fatal("endpoint B did not block in secret resolution")
	}
	select {
	case <-aDelivered:
	case <-time.After(2 * time.Second):
		t.Fatal("healthy endpoint A did not progress while B was blocked")
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		removed := *manifest
		removed.Notifications = &brand.Notifications{Webhooks: []brand.Webhook{{URL: "https://receiver.example/a", SecretRef: "env://BRAND_ZS_TRACE__A"}}}
		if _, err := tx.Exec(ctx, `UPDATE brands SET manifest=$2 WHERE id=$1`, brandID, mustJSON(t, &removed)); err != nil {
			return err
		}
		rec, err := brand.Get(ctx, tx, brandID)
		if err != nil {
			return err
		}
		_, err = svc.Reconcile(ctx, tx, rec, audit.Entry{Actor: events.Actor{Type: events.ActorSystem}, Reason: "remove", Outcome: audit.OutcomeSuccess, CorrelationID: "trace"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var action, count string
	var auditedBrand idgen.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT action, brand_id, details->>'count' FROM audit_events WHERE action='notification.webhook_deliveries.cancelled' ORDER BY occurred_at DESC, id DESC LIMIT 1`).Scan(&action, &auditedBrand, &count); err != nil {
		t.Fatal(err)
	}
	if action != "notification.webhook_deliveries.cancelled" || auditedBrand != brandID || count != "1" {
		t.Fatalf("cancellation audit action=%q brand=%s count=%q", action, auditedBrand, count)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE brands SET manifest=$2 WHERE id=$1`, brandID, mustJSON(t, manifest)); err != nil {
		t.Fatal(err)
	}
	close(releaseB)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range requests {
		if path == "/b" {
			t.Fatal("canceled and re-added endpoint B was sent")
		}
	}
}

func TestDeliveryRejectsCrossBrandSourceEvent(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	first, second := idgen.MustNewV7(), idgen.MustNewV7()
	for _, pair := range []struct {
		id   idgen.UUID
		slug string
	}{{first, "zs-one"}, {second, "zs-two"}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO brands (id,slug,name,keycloak_realm,killbill_api_key,killbill_api_secret_ref,manifest,manifest_digest,applied_at) VALUES ($1,$2,$2,$2,$2,'env://BRAND_X__KB','{}','sha256:' || repeat('0',64),now())`, pair.id, pair.slug); err != nil {
			t.Fatal(err)
		}
	}
	event, err := events.New(ctx, events.Params{Type: "tenant.created", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: first, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "one"}, CorrelationID: "cross", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, event) }); err != nil {
		t.Fatal(err)
	}
	_, err = db.Pool.Exec(ctx, `INSERT INTO webhook_fanouts (brand_id,event_id,endpoint_count) VALUES ($1,$2,0)`, second, event.StorageID())
	if err == nil {
		t.Fatal("cross-brand event reference accepted")
	}
}

func TestIndependentEndpointsRetryAndQuarantine(t *testing.T) {
	db, event := webhookFixture(t, []brand.Webhook{{URL: "https://receiver.example/ok", SecretRef: "env://BRAND_ZS_FIXTURE__OK"}, {URL: "https://receiver.example/fail", SecretRef: "env://BRAND_ZS_FIXTURE__FAIL"}})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusNoContent
		if r.URL.Path == "/fail" {
			status = http.StatusBadGateway
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	svc := &notifications.Service{DB: db.Pool, Secrets: refs{}, HTTP: client}
	if _, err := svc.RunOnce(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	var delivered, attempts int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE delivered_at IS NOT NULL), max(attempts) FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&delivered, &attempts); err != nil || delivered != 1 || attempts != 1 {
		t.Fatalf("independent results delivered=%d attempts=%d err=%v", delivered, attempts, err)
	}
	for range 9 {
		if _, err := db.Pool.Exec(context.Background(), `UPDATE webhook_deliveries SET next_attempt_at=now() WHERE event_id=$1 AND delivered_at IS NULL`, event.StorageID()); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.RunOnce(context.Background(), 10); err != nil {
			t.Fatal(err)
		}
	}
	var quarantined bool
	if err := db.Pool.QueryRow(context.Background(), `SELECT quarantined_at IS NOT NULL FROM webhook_deliveries WHERE event_id=$1 AND endpoint_url='https://receiver.example/fail'`, event.StorageID()).Scan(&quarantined); err != nil || !quarantined {
		t.Fatalf("quarantined=%v err=%v", quarantined, err)
	}
}

type rotatingRefs struct{ resolved []string }

func (r *rotatingRefs) Check(string, string) error { return nil }

func (r *rotatingRefs) Resolve(_ context.Context, _ string, ref string) (string, error) {
	r.resolved = append(r.resolved, ref)
	switch ref {
	case "env://BRAND_ZS_FIXTURE__NEW":
		return "new-secret", nil
	case "env://BRAND_ZS_FIXTURE__OLD":
		return "", errors.New("stale webhook secret reference")
	default:
		return "", errors.New("unexpected webhook secret reference")
	}
}

func TestPendingDeliveryUsesCurrentSecretReference(t *testing.T) {
	db, event := webhookFixture(t, []brand.Webhook{{URL: "https://receiver.example/hook", SecretRef: "env://BRAND_ZS_FIXTURE__OLD"}})
	refs := &rotatingRefs{}
	received := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		received = notifications.Verify([]byte("new-secret"), body, r.Header.Get(notifications.SignatureHeader), time.Now()) == nil
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	var brandID idgen.UUID
	if err := db.Pool.QueryRow(context.Background(), `SELECT brand_id FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&brandID); err != nil {
		t.Fatal(err)
	}
	m := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: []brand.Webhook{{URL: "https://receiver.example/hook", SecretRef: "env://BRAND_ZS_FIXTURE__NEW"}}}}
	if _, err := db.Pool.Exec(context.Background(), `UPDATE brands SET manifest=$2 WHERE id=$1`, brandID, mustJSON(t, m)); err != nil {
		t.Fatal(err)
	}
	if _, err := (&notifications.Service{DB: db.Pool, Secrets: refs, HTTP: client}).RunOnce(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if !received {
		t.Fatal("delivery did not use the current secret")
	}
	if len(refs.resolved) != 1 || refs.resolved[0] != "env://BRAND_ZS_FIXTURE__NEW" {
		t.Fatalf("resolved secret references = %v, want current reference only", refs.resolved)
	}
}

func TestStaleClaimCannotMarkDeliveryComplete(t *testing.T) {
	db, event := webhookFixture(t, []brand.Webhook{{URL: "https://receiver.example/block", SecretRef: "env://BRAND_ZS_FIXTURE__BLOCK"}})
	started, release := make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	svc := &notifications.Service{DB: db.Pool, Secrets: refs{}, HTTP: client}
	done := make(chan error, 1)
	go func() { _, err := svc.RunOnce(context.Background(), 1); done <- err }()
	<-started
	if _, err := db.Pool.Exec(context.Background(), `UPDATE webhook_deliveries SET claim_token=$2, claimed_until=now()+interval '1 minute' WHERE event_id=$1`, event.StorageID(), idgen.MustNewV7()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var delivered bool
	if err := db.Pool.QueryRow(context.Background(), `SELECT delivered_at IS NOT NULL FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&delivered); err != nil || delivered {
		t.Fatalf("stale claim completed delivery=%v err=%v", delivered, err)
	}
}

func TestLostResponseRetriesAfterServiceRestartWithStableEnvelopeID(t *testing.T) {
	db, event := webhookFixture(t, []brand.Webhook{{URL: "https://receiver.example/lost-response", SecretRef: "env://BRAND_ZS_FIXTURE__LOST_RESPONSE"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var firstID, secondID string
	var firstCallbackErr error
	first := &notifications.Service{DB: db.Pool, Secrets: refs{}, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var received events.Envelope
		body, err := io.ReadAll(r.Body)
		if err != nil {
			firstCallbackErr = fmt.Errorf("read first webhook request: %w", err)
			return nil, firstCallbackErr
		}
		if err := json.Unmarshal(body, &received); err != nil {
			firstCallbackErr = fmt.Errorf("decode first webhook request: %w", err)
			return nil, firstCallbackErr
		}
		firstID = received.ID
		return nil, io.ErrUnexpectedEOF
	})}}
	if count, err := first.RunOnce(ctx, 1); err != nil || count != 1 {
		t.Fatalf("first delivery = %d, %v", count, err)
	}
	if firstCallbackErr != nil {
		t.Fatal(firstCallbackErr)
	}
	if _, err := db.Pool.Exec(context.Background(), `UPDATE webhook_deliveries SET next_attempt_at=now() WHERE event_id=$1`, event.StorageID()); err != nil {
		t.Fatal(err)
	}
	var secondCallbackErr error
	second := &notifications.Service{DB: db.Pool, Secrets: refs{}, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var received events.Envelope
		body, err := io.ReadAll(r.Body)
		if err != nil {
			secondCallbackErr = fmt.Errorf("read restarted webhook request: %w", err)
			return nil, secondCallbackErr
		}
		if err := json.Unmarshal(body, &received); err != nil {
			secondCallbackErr = fmt.Errorf("decode restarted webhook request: %w", err)
			return nil, secondCallbackErr
		}
		secondID = received.ID
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}}
	if count, err := second.RunOnce(ctx, 1); err != nil || count != 1 {
		t.Fatalf("restarted delivery = %d, %v", count, err)
	}
	if secondCallbackErr != nil {
		t.Fatal(secondCallbackErr)
	}
	if firstID != event.ID || secondID != event.ID {
		t.Fatalf("receiver IDs = %q, %q, want %q", firstID, secondID, event.ID)
	}
	var attempts int
	var delivered bool
	if err := db.Pool.QueryRow(context.Background(), `SELECT attempts, delivered_at IS NOT NULL FROM webhook_deliveries WHERE event_id=$1`, event.StorageID()).Scan(&attempts, &delivered); err != nil || attempts != 1 || !delivered {
		t.Fatalf("lost response state: attempts=%d delivered=%v err=%v", attempts, delivered, err)
	}
}

func TestConcurrentWorkersClaimAtMostEightDeliveriesEach(t *testing.T) {
	hooks := make([]brand.Webhook, 16)
	for i := range hooks {
		hooks[i] = brand.Webhook{URL: fmt.Sprintf("https://receiver.example/concurrent/%d", i), SecretRef: fmt.Sprintf("env://BRAND_ZS_FIXTURE__CONCURRENT_%d", i)}
	}
	db, _ := webhookFixture(t, hooks)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var mu sync.Mutex
	seen := make(map[string]int, len(hooks))
	entered := make(chan string, len(hooks))
	release := make(chan struct{})
	released := false
	var workers sync.WaitGroup
	defer func() {
		if !released {
			close(release)
		}
		cancel()
		workers.Wait()
	}()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen[r.URL.String()]++
		mu.Unlock()
		select {
		case entered <- r.URL.String():
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	service := &notifications.Service{DB: db.Pool, Secrets: refs{}, HTTP: client}
	type result struct {
		count int
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			count, err := service.RunOnce(ctx, 100)
			results <- result{count: count, err: err}
		}()
	}
	enteredURLs := make(map[string]bool, len(hooks))
	for range hooks {
		select {
		case endpoint := <-entered:
			if enteredURLs[endpoint] {
				t.Fatalf("endpoint %s entered more than once", endpoint)
			}
			enteredURLs[endpoint] = true
		case <-ctx.Done():
			t.Fatalf("workers did not overlap on 16 requests: %v", ctx.Err())
		}
	}
	close(release)
	released = true
	for range 2 {
		select {
		case got := <-results:
			if got.err != nil || got.count != notifications.MaxConcurrentDeliveries {
				t.Fatalf("worker result = %d, %v; want %d, nil", got.count, got.err, notifications.MaxConcurrentDeliveries)
			}
		case <-ctx.Done():
			t.Fatalf("worker did not finish: %v", ctx.Err())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(hooks) {
		t.Fatalf("unique requests=%d, want %d", len(seen), len(hooks))
	}
	for endpoint, count := range seen {
		if count != 1 {
			t.Fatalf("endpoint %s requested %d times", endpoint, count)
		}
	}
}

func TestDeliveryLogDoesNotExposeEndpointPath(t *testing.T) {
	db, _ := webhookFixture(t, []brand.Webhook{{URL: "https://receiver.example/vendor-capability-token", SecretRef: "env://BRAND_ZS_FIXTURE__LOG"}})
	var logs bytes.Buffer
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })}
	if _, err := (&notifications.Service{DB: db.Pool, Secrets: refs{}, HTTP: client, Log: slog.New(slog.NewTextHandler(&logs, nil))}).RunOnce(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "vendor-capability-token") || strings.Contains(logs.String(), "UnexpectedEOF") {
		t.Fatalf("unsafe delivery log: %s", logs.String())
	}
}

func TestFanoutBrandLockSerializesManifestUpdate(t *testing.T) {
	db, old := webhookFixture(t, []brand.Webhook{{URL: "https://receiver.example/one", SecretRef: "env://BRAND_ZS_FIXTURE__ONE"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var brandID idgen.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT brand_id FROM webhook_deliveries WHERE event_id=$1`, old.StorageID()).Scan(&brandID); err != nil {
		t.Fatal(err)
	}
	next, err := events.New(ctx, events.Params{Type: "tenant.deleted", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: brandID, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "two"}, CorrelationID: "lock", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := outbox.Write(ctx, tx, next); err != nil {
		t.Fatal(err)
	}
	if err := (&notifications.Service{Secrets: refs{}}).Fanout(ctx, tx, next); err != nil {
		t.Fatal(err)
	}
	probe, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Rollback(ctx)
	if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = probe.Exec(ctx, `UPDATE brands SET name='fixture updated' WHERE id=$1`, brandID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("brand update while fanout holds FOR SHARE = %v, want SQLSTATE 55P03", err)
	}
	if err := probe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE brands SET name='fixture updated' WHERE id=$1`, brandID); err != nil {
		t.Fatal(err)
	}
}

func webhookFixture(t *testing.T, hooks []brand.Webhook) (*postgrestest.DB, events.Envelope) {
	t.Helper()
	db := postgrestest.New(t)
	ctx := context.Background()
	brandID := idgen.MustNewV7()
	m := &brand.Manifest{Notifications: &brand.Notifications{Webhooks: hooks}}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO brands (id,slug,name,keycloak_realm,killbill_api_key,killbill_api_secret_ref,manifest,manifest_digest,applied_at) VALUES ($1,'zs-fixture','fixture','fixture','fixture','env://BRAND_ZS_FIXTURE__KB',$2,'sha256:' || repeat('0',64),now())`, brandID, mustJSON(t, m)); err != nil {
		t.Fatal(err)
	}
	event, err := events.New(ctx, events.Params{Type: "tenant.created", SchemaVersion: 1, Producer: events.Producer{Service: "zs-core", Instance: "test"}, Actor: events.Actor{Type: events.ActorSystem}, Brand: brandID, Tenant: idgen.MustNewV7(), Aggregate: events.Aggregate{Type: "tenant", ID: "one"}, CorrelationID: "fixture", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if err := outbox.Write(ctx, tx, event); err != nil {
			return err
		}
		return (&notifications.Service{Secrets: refs{}}).Fanout(ctx, tx, event)
	}); err != nil {
		t.Fatal(err)
	}
	return db, event
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
