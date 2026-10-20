//go:build integration

package httpapi_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

func TestReadyzFollowsMigrations(t *testing.T) {
	db := postgrestest.NewEmpty(t)
	srv := httptest.NewServer(httpapi.New(httpapi.Options{DB: db.Pool, Migrations: migrations.FS}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz before migrations = %d", resp.StatusCode)
	}

	if _, err := database.Migrate(context.Background(), db.Pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body gen.HealthStatus
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || body.Status != gen.HealthStatusStatusReady || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("readyz after migrations = %d %+v", resp.StatusCode, body)
	}
}

func TestListAuditEventsEndToEnd(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	staff := newStaffFixture(t, db)
	mine := scope.Scope{Brand: staff.brand, Tenant: postgrestest.NewScope(t).Tenant}
	other := scope.Scope{Brand: postgrestest.NewScope(t).Brand, Tenant: mine.Tenant}
	base := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		for _, s := range []scope.Scope{mine, other} {
			err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
				_, err := audit.Write(ctx, tx, s, audit.Entry{
					Actor:         events.Actor{Type: events.ActorAPIKey, ID: "key_test"},
					Action:        "wallet.grant.created",
					Outcome:       audit.OutcomeSuccess,
					CorrelationID: "corr",
					OccurredAt:    base.Add(time.Duration(i) * time.Second),
				})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	srv := httptest.NewServer(httpapi.New(httpapi.Options{DB: db.Pool, Migrations: migrations.FS, Verifier: staff.verifier}))
	defer srv.Close()

	// Without an operator grant the route does not exist for the caller.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/audit/events?tenant_id="+mine.TenantID(), nil)
	req.Header.Set("Authorization", "Bearer "+staff.token(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("without a grant: %d", resp.StatusCode)
	}
	staff.grant(t, db, identity.StaffOperator)

	get := func(query url.Values) (*http.Response, gen.AuditEventPage) {
		query.Set("tenant_id", mine.TenantID())
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/audit/events?"+query.Encode(), nil)
		req.Header.Set("Authorization", "Bearer "+staff.token(t))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var page gen.AuditEventPage
		dec := json.NewDecoder(resp.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&page); err != nil {
			t.Fatal(err)
		}
		return resp, page
	}

	var ids []string
	query := url.Values{"limit": {"2"}}
	for {
		resp, page := get(query)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Request-ID") == "" {
			t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
		}
		for _, e := range page.Data {
			if e.BrandId != mine.BrandID() || e.TenantId == nil || *e.TenantId != mine.TenantID() {
				t.Fatalf("event outside scope: %+v", e)
			}
			ids = append(ids, e.Id)
		}
		if page.NextCursor == nil {
			break
		}
		query.Set("cursor", *page.NextCursor)
	}
	if len(ids) != 5 {
		t.Fatalf("got %d events, want 5", len(ids))
	}

	_, filtered := get(url.Values{
		"occurred_after":  {base.Add(time.Second).Format(time.RFC3339)},
		"occurred_before": {base.Add(3 * time.Second).Format(time.RFC3339)},
		"action":          {"wallet.grant.created"},
		"actor_id":        {"key_test"},
	})
	if len(filtered.Data) != 2 || filtered.NextCursor != nil {
		t.Fatalf("filtered = %+v", filtered)
	}
}

// staffFixture is an applied brand, a staff principal of it and a verifier
// that trusts a test signing key in place of Keycloak's.
type staffFixture struct {
	brand    idgen.UUID
	key      *rsa.PrivateKey
	verifier *identity.Verifier
}

const fixtureIssuer = "http://127.0.0.1:8180/realms/zs-reference"

type fixtureKeys struct{ key *rsa.PublicKey }

func (k fixtureKeys) Issuer(context.Context, string) (string, error) { return fixtureIssuer, nil }
func (k fixtureKeys) PublicKey(_ context.Context, _, kid string) (*rsa.PublicKey, error) {
	if kid != "k1" {
		return nil, identity.ErrUnknownKey
	}
	return k.key, nil
}

func newStaffFixture(t *testing.T, db *postgrestest.DB) *staffFixture {
	t.Helper()
	m, err := brand.Load(filepath.Join("..", "..", "brands", "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(m)
	id := idgen.MustNewV7()
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO brands (id, slug, name, keycloak_realm, killbill_api_key,
    killbill_api_secret_ref, manifest, manifest_digest, applied_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`, id, m.Slug, m.Name, m.Identity.Realm, m.Billing.KillBillTenantKey,
		m.Billing.KillBillAPISecretRef, manifest, m.Digest()); err != nil {
		t.Fatal(err)
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	return &staffFixture{brand: id, key: key, verifier: &identity.Verifier{
		Brands: brand.NewRegistry(db.Pool, time.Minute, nil),
		Keys:   fixtureKeys{key: &key.PublicKey},
	}}
}

func (f *staffFixture) token(t *testing.T) string {
	t.Helper()
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	input := enc(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + enc(map[string]any{
		"iss": fixtureIssuer, "sub": "staff-1", "aud": "zs-staff", "azp": "zs-staff", "typ": "Bearer",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	})
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *staffFixture) grant(t *testing.T, db *postgrestest.DB, s identity.StaffScope) {
	t.Helper()
	err := pgx.BeginFunc(context.Background(), db.Pool, func(tx pgx.Tx) error {
		_, err := identity.AddStaffGrant(context.Background(), tx, identity.StaffGrant{
			BrandID: f.brand, Issuer: fixtureIssuer, Subject: "staff-1", Scope: s, GrantedBy: "test", Reason: "test",
		}, events.Actor{Type: events.ActorService, ID: "test"}, "corr")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
