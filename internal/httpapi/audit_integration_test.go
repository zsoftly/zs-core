//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/events"
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
	mine := postgrestest.NewScope(t)
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

	srv := httptest.NewServer(httpapi.New(httpapi.Options{DB: db.Pool, Migrations: migrations.FS, TrustedScopeHeaders: true}))
	defer srv.Close()

	get := func(query url.Values) (*http.Response, gen.AuditEventPage) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/audit/events?"+query.Encode(), nil)
		req.Header.Set("X-ZS-Brand", mine.BrandID())
		req.Header.Set("X-ZS-Tenant", mine.TenantID())
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
			if e.BrandId != mine.BrandID() || e.TenantId != mine.TenantID() {
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
