//go:build integration

package tenant_test

import (
	"context"
	"encoding/json"
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
	// The outcomes table references the attempts table, so Postgres only
	// lets a truncate through to the guard when both are named.
	_, err := e.db.Pool.Exec(ctx, `TRUNCATE tenant_closure_invoice_attempts, tenant_closure_invoice_outcomes`)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || (pgErr.Message != "closure invoice attempt is immutable" && pgErr.Message != "closure invoice outcome is immutable") {
		t.Fatalf("truncate error = %v", err)
	}
	_, err = e.db.Pool.Exec(ctx, `TRUNCATE tenant_closure_invoice_outcomes`)
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.Message != "closure invoice outcome is immutable" {
		t.Fatalf("outcomes truncate error = %v", err)
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

func TestClosureInvoiceExecutionStartsBeforeSingleInvoicePost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const productApplicationName = "closure-invoice-execution-test"
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
	claim, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !claim.Claimed {
		t.Fatalf("claim = %+v %v", claim, err)
	}
	var posts atomic.Int64
	var responseMu sync.Mutex
	var invoiceID, targetDate string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerTenantLookup(w, r, tn.BrandID) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices" {
			posts.Add(1)
			var openTransactions int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND xact_start IS NOT NULL`, productApplicationName).Scan(&openTransactions); err != nil || openTransactions != 0 {
				t.Errorf("product transaction at POST = %d, %v", openTransactions, err)
				http.Error(w, "product transaction open", http.StatusInternalServerError)
				return
			}
			var started int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'started'`).Scan(&started); err != nil || started != 1 {
				t.Errorf("started outcome before POST = %d, %v", started, err)
				http.Error(w, "outcome missing", http.StatusInternalServerError)
				return
			}
			responseMu.Lock()
			invoiceID = idgen.MustNewV7().String()
			targetDate = r.URL.Query().Get("targetDate")
			responseMu.Unlock()
			w.Header().Set("Location", "/1.0/kb/invoices/"+invoiceID)
			w.WriteHeader(http.StatusCreated)
			return
		}
		responseMu.Lock()
		id, date := invoiceID, targetDate
		responseMu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/1.0/kb/invoices/"+id {
			var openTransactions int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND xact_start IS NOT NULL`, productApplicationName).Scan(&openTransactions); err != nil || openTransactions != 0 {
				t.Errorf("product transaction at GET = %d, %v", openTransactions, err)
				http.Error(w, "product transaction open", http.StatusInternalServerError)
				return
			}
			var recorded int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'invoice_id' AND invoice_id = $1::uuid`, id).Scan(&recorded); err != nil || recorded != 1 {
				t.Errorf("invoice ID before GET = %d, %v", recorded, err)
				http.Error(w, "invoice ID missing", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"invoiceId":"` + id + `","accountId":"` + tn.KillBillAccountID + `","status":"COMMITTED","currency":"CAD","targetDate":"` + date + `"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.KillBill = kb
	productConfig, err := pgxpool.ParseConfig(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	productConfig.ConnConfig.RuntimeParams["application_name"] = productApplicationName
	product, err := pgxpool.NewWithConfig(ctx, productConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(product.Close)
	result, err := tenant.ExecuteClosureInvoiceOnce(ctx, product, e.eng, tn.Scope())
	if err != nil || !result.Posted || posts.Load() != 1 {
		t.Fatalf("execute = %+v %v posts=%d", result, err, posts.Load())
	}
}

func TestClosureInvoiceExecutionNeverRepostsUnknownTrigger(t *testing.T) {
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
	claim, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !claim.Claimed {
		t.Fatalf("claim = %+v %v", claim, err)
	}
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerTenantLookup(w, r, tn.BrandID) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices" {
			posts.Add(1)
			http.Error(w, "lost reply", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.KillBill = kb
	if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 1 {
		t.Fatalf("unknown trigger = %v posts=%d", err, posts.Load())
	}
	if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 1 {
		t.Fatalf("unknown trigger retry = %v posts=%d", err, posts.Load())
	}
	var pending int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'post_unknown'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("post unknown outcomes = %d, %v", pending, err)
	}
}

func TestClosureInvoiceExecutionNeverRepostsLostTriggerReply(t *testing.T) {
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
	claim, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !claim.Claimed {
		t.Fatalf("claim = %+v %v", claim, err)
	}
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerTenantLookup(w, r, tn.BrandID) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices" {
			posts.Add(1)
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("response writer cannot hijack")
				http.Error(w, "cannot simulate lost reply", http.StatusInternalServerError)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack response: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.KillBill = kb
	if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 1 {
		t.Fatalf("lost trigger reply = %v posts=%d", err, posts.Load())
	}
	if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 1 {
		t.Fatalf("lost trigger retry = %v posts=%d", err, posts.Load())
	}
	var pending int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'post_unknown'`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("post unknown outcomes = %d, %v", pending, err)
	}
}

func TestClosureInvoiceExecutionRefusesCallerTransactionBeforePost(t *testing.T) {
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
	claim, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !claim.Claimed {
		t.Fatalf("claim = %+v %v", claim, err)
	}
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		http.Error(w, "caller transaction reached Kill Bill", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.KillBill = kb
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	result, err := tenant.ExecuteClosureInvoiceOnce(ctx, tx, e.eng, tn.Scope())
	if err == nil || result.Posted || posts.Load() != 0 {
		t.Fatalf("caller transaction execution = %+v %v posts=%d", result, err, posts.Load())
	}
	var outcomes int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes`).Scan(&outcomes); err != nil || outcomes != 0 {
		t.Fatalf("caller transaction outcomes = %d, %v", outcomes, err)
	}
}

func TestClosureInvoiceExecutionRefusesChangedKillBillTenant(t *testing.T) {
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
	claim, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !claim.Claimed {
		t.Fatalf("claim = %+v %v", claim, err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE brands SET killbill_tenant_id = $2 WHERE id = $1`, tn.BrandID, idgen.MustNewV7().String()); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		http.Error(w, "changed target reached Kill Bill", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.KillBill = kb
	if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 0 {
		t.Fatalf("changed Kill Bill target = %v posts=%d", err, posts.Load())
	}
}

func TestClosureInvoiceOutcomeRejectsNilStartedCurrency(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status)
VALUES ($1, $2, now(), 'finance_pending')`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	attemptID := idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts
    (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $2, $4::uuid, current_date)`, attemptID, tn.BrandID, tn.ID, tn.KillBillAccountID); err != nil {
		t.Fatal(err)
	}
	_, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, expected_currency)
VALUES ($1, $2, 'started', NULL)`, idgen.MustNewV7(), attemptID)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("nil expected currency error = %v", err)
	}
}

func TestClosureInvoiceExecutionRecoversPersistedInvoiceIDWithoutReposting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		invoiceID  bool
		accountID  string
		currency   string
		targetDate string
		status     string
		wantErr    bool
		outcome    string
	}{
		{name: "matching invoice", accountID: "expected", currency: "CAD", targetDate: "expected", status: "COMMITTED", outcome: "evidenced"},
		{name: "wrong invoice ID", invoiceID: true, accountID: "expected", currency: "CAD", targetDate: "expected", status: "COMMITTED", wantErr: true, outcome: "read_mismatch"},
		{name: "wrong account", accountID: "foreign", currency: "CAD", targetDate: "expected", status: "COMMITTED", wantErr: true, outcome: "read_mismatch"},
		{name: "wrong currency", accountID: "expected", currency: "USD", targetDate: "expected", status: "COMMITTED", wantErr: true, outcome: "read_mismatch"},
		{name: "wrong target date", accountID: "expected", currency: "CAD", targetDate: "wrong", status: "COMMITTED", wantErr: true, outcome: "read_mismatch"},
		{name: "wrong status", accountID: "expected", currency: "CAD", targetDate: "expected", status: "DRAFT", wantErr: true, outcome: "read_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			claim, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
			if err != nil || !claim.Claimed {
				t.Fatalf("claim = %+v %v", claim, err)
			}

			var posts atomic.Int64
			var reads atomic.Int64
			var responseMu sync.Mutex
			invoiceCurrency := tc.currency
			invoiceStatus := tc.status
			wrongInvoiceID := tc.invoiceID
			foreignAccount := tc.accountID == "foreign"
			wrongTargetDate := tc.targetDate == "wrong"
			var invoiceID, targetDate string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if answerTenantLookup(w, r, tn.BrandID) {
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices" {
					posts.Add(1)
					responseMu.Lock()
					invoiceID = idgen.MustNewV7().String()
					targetDate = r.URL.Query().Get("targetDate")
					responseMu.Unlock()
					w.Header().Set("Location", "/1.0/kb/invoices/"+invoiceID)
					w.WriteHeader(http.StatusCreated)
					return
				}
				responseMu.Lock()
				id, date, currency := invoiceID, targetDate, invoiceCurrency
				responseID := id
				account := tn.KillBillAccountID
				responseDate := date
				if wrongInvoiceID {
					responseID = idgen.MustNewV7().String()
				}
				if foreignAccount {
					account = idgen.MustNewV7().String()
				}
				if wrongTargetDate {
					responseDate = "2099-01-01"
				}
				status := invoiceStatus
				responseMu.Unlock()
				if r.Method == http.MethodGet && r.URL.Path == "/1.0/kb/invoices/"+id {
					if reads.Add(1) == 1 {
						http.Error(w, "read unavailable", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"invoiceId":"` + responseID + `","accountId":"` + account + `","status":"` + status + `","currency":"` + currency + `","targetDate":"` + responseDate + `"}`))
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(server.Close)
			kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			e.eng.KillBill = kb

			if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 1 || reads.Load() != 1 {
				t.Fatalf("initial POST/read = %v posts=%d reads=%d", err, posts.Load(), reads.Load())
			}
			var recorded int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'invoice_id' AND invoice_id = $1::uuid`, invoiceID).Scan(&recorded); err != nil || recorded != 1 {
				t.Fatalf("persisted invoice ID = %d, %v", recorded, err)
			}

			result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
			if tc.wantErr && err == nil {
				t.Fatalf("mismatched read succeeded: %+v", result)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("matching read = %+v %v", result, err)
			}
			if posts.Load() != 1 || reads.Load() != 2 {
				t.Fatalf("recovery POST/read = posts=%d reads=%d", posts.Load(), reads.Load())
			}
			var outcomes int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = $1`, tc.outcome).Scan(&outcomes); err != nil || outcomes != 1 {
				t.Fatalf("%s outcomes = %d, %v", tc.outcome, outcomes, err)
			}
			if tc.wantErr {
				responseMu.Lock()
				invoiceCurrency = "CAD"
				invoiceStatus = "COMMITTED"
				wrongInvoiceID = false
				foreignAccount = false
				wrongTargetDate = false
				responseMu.Unlock()
				result, err = tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
				if err != nil || posts.Load() != 1 || reads.Load() != 3 {
					t.Fatalf("matching retry = %+v %v posts=%d reads=%d", result, err, posts.Load(), reads.Load())
				}
				if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'evidenced'`).Scan(&outcomes); err != nil || outcomes != 1 {
					t.Fatalf("evidenced outcomes = %d, %v", outcomes, err)
				}
			}
			readsBeforeRepeat := reads.Load()
			result, err = tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
			if err != nil || !result.Evidenced || posts.Load() != 1 || reads.Load() != readsBeforeRepeat+1 {
				t.Fatalf("repeated matching read = %+v %v posts=%d reads=%d", result, err, posts.Load(), reads.Load())
			}
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'evidenced'`).Scan(&outcomes); err != nil || outcomes != 1 {
				t.Fatalf("repeated evidenced outcomes = %d, %v", outcomes, err)
			}
		})
	}
}

// answerTenantLookup answers Kill Bill's tenant lookup with the given tenant.
func answerTenantLookup(w http.ResponseWriter, r *http.Request, tenantID idgen.UUID) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/1.0/kb/tenants" {
		return false
	}
	body, _ := json.Marshal(killbill.Tenant{TenantID: tenantID.String(), ExternalKey: "closure", APIKey: r.URL.Query().Get("apiKey")})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
	return true
}

// claimedClosure is a closing tenant with a claimed, not yet executed,
// closure invoice, and a Kill Bill whose tenant lookup answers as lookup
// says: 0 the stored tenant, 1 another tenant, 2 unavailable.
func claimedClosure(t *testing.T, lookup *atomic.Int32, posts, lookups *atomic.Int64) (*env, tenant.Tenant) {
	t.Helper()
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
	if claim, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err != nil || !claim.Claimed {
		t.Fatalf("claim = %+v %v", claim, err)
	}
	var mu sync.Mutex
	var invoiceID, targetDate string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/1.0/kb/tenants" {
			lookups.Add(1)
			switch lookup.Load() {
			case 1:
				answerTenantLookup(w, r, idgen.MustNewV7())
			case 2:
				http.Error(w, "tenant lookup unavailable", http.StatusServiceUnavailable)
			default:
				answerTenantLookup(w, r, tn.BrandID)
			}
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices" {
			posts.Add(1)
			mu.Lock()
			invoiceID = idgen.MustNewV7().String()
			targetDate = r.URL.Query().Get("targetDate")
			mu.Unlock()
			w.Header().Set("Location", "/1.0/kb/invoices/"+invoiceID)
			w.WriteHeader(http.StatusCreated)
			return
		}
		mu.Lock()
		id, date := invoiceID, targetDate
		mu.Unlock()
		if r.Method == http.MethodGet && id != "" && r.URL.Path == "/1.0/kb/invoices/"+id {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"invoiceId":"` + id + `","accountId":"` + tn.KillBillAccountID + `","status":"COMMITTED","currency":"CAD","targetDate":"` + date + `"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.KillBill = kb
	return e, tn
}

// A Kill Bill tenant rebuilt under the same API key is not the tenant the
// brand stores. The live lookup catches it before anything is started.
func TestClosureInvoiceExecutionRefusesARebuiltKillBillTenant(t *testing.T) {
	var lookup atomic.Int32
	var posts, lookups atomic.Int64
	lookup.Store(1)
	e, tn := claimedClosure(t, &lookup, &posts, &lookups)
	ctx := context.Background()
	if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 0 {
		t.Fatalf("rebuilt tenant = %v posts=%d", err, posts.Load())
	}
	var outcomes int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes`).Scan(&outcomes); err != nil || outcomes != 0 {
		t.Fatalf("outcomes after a refused tenant = %d, %v", outcomes, err)
	}
}

// A lookup that fails leaves nothing started, so the attempt is not stuck
// for manual review: once Kill Bill answers, it starts and posts once.
func TestClosureInvoiceExecutionLeavesNothingStartedWhenTheLookupFails(t *testing.T) {
	var lookup atomic.Int32
	var posts, lookups atomic.Int64
	lookup.Store(2)
	e, tn := claimedClosure(t, &lookup, &posts, &lookups)
	ctx := context.Background()
	if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 0 {
		t.Fatalf("failed lookup = %v posts=%d", err, posts.Load())
	}
	var outcomes int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes`).Scan(&outcomes); err != nil || outcomes != 0 {
		t.Fatalf("outcomes after a failed lookup = %d, %v", outcomes, err)
	}
	lookup.Store(0)
	result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !result.Posted || !result.Evidenced || posts.Load() != 1 {
		t.Fatalf("after the lookup recovered = %+v %v posts=%d", result, err, posts.Load())
	}
}
