//go:build integration

package tenant_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func TestClosureInvoiceProcessorRefusesInvalidClaims(t *testing.T) {
	for _, tc := range []struct {
		name         string
		closing      bool
		drained      bool
		foreignScope bool
	}{
		{name: "active tenant", drained: true},
		{name: "missing drain", closing: true},
		{name: "foreign scope", closing: true, drained: true, foreignScope: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			p := e.person("owner@example.test")
			tn := e.mustOrg(p, "Tenant A")
			if tc.closing {
				if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID); err != nil {
					t.Fatal(err)
				}
			}
			drain := "NULL"
			if tc.drained {
				drain = "now()"
			}
			if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements
    (brand_id, tenant_id, cutoff_at, status, cancellation_completed_at, finance_target_date, drain_completed_at)
VALUES ($1, $2, now(), 'finance_pending', now(), current_date, `+drain+`)`, tn.BrandID, tn.ID); err != nil {
				t.Fatal(err)
			}
			sc := tn.Scope()
			if tc.foreignScope {
				sc = scope.Scope{Brand: idgen.MustNewV7(), Tenant: tn.ID}
			}
			result, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, sc)
			if err == nil || result.Claimed || result.Attempted {
				t.Fatalf("invalid claim = %+v %v", result, err)
			}
			var n int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_attempts WHERE brand_id = $1 AND tenant_id = $2`, tn.BrandID, tn.ID).Scan(&n); err != nil || n != 0 {
				t.Fatalf("invalid claim created %d attempts, %v", n, err)
			}
		})
	}
}

func TestClosureInvoiceProcessorClaimsExactlyOnce(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements
    (brand_id, tenant_id, cutoff_at, status, cancellation_completed_at, finance_target_date, drain_completed_at)
VALUES ($1, $2, now(), 'finance_pending', now(), current_date, now())`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "D1a must not call Kill Bill", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.KillBill = kb
	observer, err := pgxpool.New(ctx, e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Close)
	start := make(chan struct{})
	results := make(chan tenant.ClosureProcessResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
			results <- result
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	claimed := 0
	for result := range results {
		if result.Claimed {
			claimed++
		}
		if result.Attempted {
			t.Fatal("claim-only processor attempted external work")
		}
	}
	if claimed != 1 {
		t.Fatalf("claimed = %d, want 1", claimed)
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := observer.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_attempts WHERE brand_id = $1 AND tenant_id = $2`, tn.BrandID, tn.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("attempts = %d, %v", n, err)
	}
	if requests.Load() != 0 {
		t.Fatalf("claim-only processor made %d Kill Bill requests", requests.Load())
	}
}

func TestClosureInvoiceProcessorRefusesCallerTransaction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements
    (brand_id, tenant_id, cutoff_at, status, cancellation_completed_at, finance_target_date, drain_completed_at)
VALUES ($1, $2, now(), 'finance_pending', now(), current_date, now())`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	result, err := tenant.ProcessClosureInvoiceOnce(ctx, tx, e.eng, tn.Scope())
	if err == nil || result.Claimed || result.Attempted {
		t.Fatalf("caller transaction = %+v %v", result, err)
	}
	var n int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_attempts WHERE brand_id = $1 AND tenant_id = $2`, tn.BrandID, tn.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("caller transaction created %d attempts, %v", n, err)
	}
}

func TestClosureInvoiceAttemptRejectsEveryMutation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status)
VALUES ($1, $2, now(), 'finance_pending')`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	id := idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts
    (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $2, $4::uuid, current_date)`, id, tn.BrandID, tn.ID, tn.KillBillAccountID); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"noop":      `UPDATE tenant_closure_invoice_attempts SET target_date = target_date WHERE id = '` + id.String() + `'`,
		"id":        `UPDATE tenant_closure_invoice_attempts SET id = '` + idgen.MustNewV7().String() + `' WHERE id = '` + id.String() + `'`,
		"brand":     `UPDATE tenant_closure_invoice_attempts SET brand_id = '` + idgen.MustNewV7().String() + `' WHERE id = '` + id.String() + `'`,
		"tenant":    `UPDATE tenant_closure_invoice_attempts SET tenant_id = '` + idgen.MustNewV7().String() + `' WHERE id = '` + id.String() + `'`,
		"kb tenant": `UPDATE tenant_closure_invoice_attempts SET killbill_tenant_id = '` + idgen.MustNewV7().String() + `' WHERE id = '` + id.String() + `'`,
		"account":   `UPDATE tenant_closure_invoice_attempts SET killbill_account_id = '` + idgen.MustNewV7().String() + `' WHERE id = '` + id.String() + `'`,
		"date":      `UPDATE tenant_closure_invoice_attempts SET target_date = current_date + 1 WHERE id = '` + id.String() + `'`,
		"status":    `UPDATE tenant_closure_invoice_attempts SET status = status WHERE id = '` + id.String() + `'`,
		"created":   `UPDATE tenant_closure_invoice_attempts SET created_at = now() WHERE id = '` + id.String() + `'`,
		"delete":    `DELETE FROM tenant_closure_invoice_attempts WHERE id = '` + id.String() + `'`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.db.Pool.Exec(ctx, statement)
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.Message != "closure invoice attempt is immutable" {
				t.Fatalf("%s mutation error = %v", name, err)
			}
		})
	}
	_, err := e.db.Pool.Exec(ctx, `TRUNCATE tenant_closure_invoice_attempts`)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.Message != "closure invoice attempt is immutable" {
		t.Fatalf("truncate error = %v", err)
	}
	var n int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_attempts WHERE brand_id = $1 AND tenant_id = $2`, tn.BrandID, tn.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("immutable count = %d, %v", n, err)
	}
	var status string
	var storedID idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id, status FROM tenant_closure_invoice_attempts WHERE brand_id = $1 AND tenant_id = $2`, tn.BrandID, tn.ID).Scan(&storedID, &status); err != nil || storedID != id || status != "intent" {
		t.Fatalf("immutable row = count %d id %v status %q err %v", n, storedID, status, err)
	}
}
