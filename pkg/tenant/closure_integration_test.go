//go:build integration

package tenant_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	// lets a truncate through to a guard when both are named. The first table
	// named is the one whose guard answers.
	for _, tc := range []struct{ statement, message string }{
		{`TRUNCATE tenant_closure_invoice_attempts, tenant_closure_invoice_outcomes`, "closure invoice attempt is immutable"},
		{`TRUNCATE tenant_closure_invoice_outcomes, tenant_closure_invoice_attempts`, "closure invoice outcome is immutable"},
		{`TRUNCATE tenant_closure_invoice_outcomes`, "closure invoice outcome is immutable"},
	} {
		_, err := e.db.Pool.Exec(ctx, tc.statement)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.Message != tc.message {
			t.Fatalf("%s = %v, want %q", tc.statement, err, tc.message)
		}
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
		if answerPreflight(w, r, tn, 0, 0) {
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
		if answerPreflight(w, r, tn, 0, 0) {
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
		if answerPreflight(w, r, tn, 0, 0) {
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
				if answerPreflight(w, r, tn, 0, 0) {
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

// answerPreflight answers what ExecuteClosureInvoiceOnce reads from Kill Bill
// before it starts: the tenant lookup, the account and the invoice dry run.
// dry: 0 something to invoice, 1 nothing (204), 2 unavailable. account: 0 the
// tenant's account in CAD, 1 unavailable, 2 in a currency the brand does not bill in, 3 another account.
func answerPreflight(w http.ResponseWriter, r *http.Request, tn tenant.Tenant, dry, account int32) bool {
	if answerTenantLookup(w, r, tn.BrandID) {
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/1.0/kb/accounts/"+tn.KillBillAccountID {
		id, currency := tn.KillBillAccountID, "CAD"
		switch account {
		case 1:
			http.Error(w, "account unavailable", http.StatusServiceUnavailable)
			return true
		case 2:
			currency = "ZZZ"
		case 3:
			id = idgen.MustNewV7().String()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accountId":"` + id + `","currency":"` + currency + `"}`))
		return true
	}
	if r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices/dryRun" {
		switch dry {
		case 1:
			w.WriteHeader(http.StatusNoContent)
		case 2:
			http.Error(w, "dry run unavailable", http.StatusServiceUnavailable)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"amount":1,"currency":"CAD"}`))
		}
		return true
	}
	return false
}

// kbMode says how the Kill Bill of claimedClosure answers.
// lookup: 0 the stored tenant, 1 another tenant, 2 unavailable.
// post: 0 created, 2 slow, 3 a bare 404. dry and account are as for answerPreflight.
type kbMode struct {
	lookup, post, dry, account atomic.Int32
	// holdFirstDry, when set, makes the first dry run say it signalled on
	// dryHeld and then wait for dryRelease before it answers 204.
	holdFirstDry bool
	dryHeld      chan struct{}
	dryRelease   chan struct{}
	dryOnce      sync.Once
	// noClaim leaves the tenant ready to be claimed, instead of claimed.
	noClaim bool
	// startCancelling starts the settlement at cancelling, with none of the
	// steps before finance done, and implies noClaim.
	startCancelling bool
	// inFlight, when set, runs while a dry run or a POST is in flight.
	inFlight func()
	// readFail makes the invoice readback answer 503.
	readFail atomic.Int32
}

// claimedClosure is a closing tenant with a claimed, not yet executed,
// closure invoice, and a Kill Bill whose tenant lookup answers as lookup
// says: 0 the stored tenant, 1 another tenant, 2 unavailable.
func claimedClosure(t *testing.T, m *kbMode, posts, lookups *atomic.Int64) (*env, tenant.Tenant) {
	t.Helper()
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if m.startCancelling {
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status) VALUES ($1, $2, now(), 'cancelling')`, tn.BrandID, tn.ID); err != nil {
			t.Fatal(err)
		}
	} else if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements
    (brand_id, tenant_id, cutoff_at, status, cancellation_completed_at, finance_target_date, drain_completed_at)
VALUES ($1, $2, now(), 'finance_pending', now(), current_date, now())`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if !m.noClaim && !m.startCancelling {
		if claim, err := tenant.ProcessClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err != nil || !claim.Claimed {
			t.Fatalf("claim = %+v %v", claim, err)
		}
	}
	var mu sync.Mutex
	var invoiceID, targetDate string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/1.0/kb/tenants" {
			lookups.Add(1)
			switch m.lookup.Load() {
			case 1:
				answerTenantLookup(w, r, idgen.MustNewV7())
			case 2:
				http.Error(w, "tenant lookup unavailable", http.StatusServiceUnavailable)
			default:
				answerTenantLookup(w, r, tn.BrandID)
			}
			return
		}
		if m.inFlight != nil && r.Method == http.MethodPost && (r.URL.Path == "/1.0/kb/invoices/dryRun" || r.URL.Path == "/1.0/kb/invoices") {
			m.inFlight()
		}
		if m.holdFirstDry && r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices/dryRun" {
			first := false
			m.dryOnce.Do(func() { first = true })
			if first {
				close(m.dryHeld)
				<-m.dryRelease
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if answerPreflight(w, r, tn, m.dry.Load(), m.account.Load()) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/invoices" {
			posts.Add(1)
			switch m.post.Load() {
			case 2:
				time.Sleep(800 * time.Millisecond)
			case 3:
				w.Header().Set("Content-Type", "text/html;charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("<!doctype html><html><body><h3>Apache Tomcat/9.0.122</h3></body></html>"))
				return
			}
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
			if m.readFail.Load() == 1 {
				http.Error(w, "readback unavailable", http.StatusServiceUnavailable)
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
	return e, tn
}

// A Kill Bill tenant rebuilt under the same API key is not the tenant the
// brand stores. The live lookup catches it before anything is started.
func TestClosureInvoiceExecutionRefusesARebuiltKillBillTenant(t *testing.T) {
	var m kbMode
	var posts, lookups atomic.Int64
	m.lookup.Store(1)
	e, tn := claimedClosure(t, &m, &posts, &lookups)
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
	var m kbMode
	var posts, lookups atomic.Int64
	m.lookup.Store(2)
	e, tn := claimedClosure(t, &m, &posts, &lookups)
	ctx := context.Background()
	if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 0 {
		t.Fatalf("failed lookup = %v posts=%d", err, posts.Load())
	}
	var outcomes int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes`).Scan(&outcomes); err != nil || outcomes != 0 {
		t.Fatalf("outcomes after a failed lookup = %d, %v", outcomes, err)
	}
	m.lookup.Store(0)
	result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !result.Posted || !result.Evidenced || posts.Load() != 1 {
		t.Fatalf("after the lookup recovered = %+v %v posts=%d", result, err, posts.Load())
	}
}

// The invoice dry run answers 204 when there is nothing to invoice. That is
// definite: it is recorded as no_invoice, nothing is started and no real POST
// is made. A repeat returns it without calling Kill Bill.
func TestClosureInvoiceExecutionRecordsNothingToInvoiceFromTheDryRun(t *testing.T) {
	var m kbMode
	var posts, lookups atomic.Int64
	m.dry.Store(1)
	e, tn := claimedClosure(t, &m, &posts, &lookups)
	ctx := context.Background()
	result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || result.Posted || !result.NoInvoice || result.Evidenced || posts.Load() != 0 {
		t.Fatalf("nothing to invoice = %+v %v posts=%d", result, err, posts.Load())
	}
	var noInvoice, started int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'no_invoice' AND invoice_id IS NULL), count(*) FILTER (WHERE kind = 'started')
FROM tenant_closure_invoice_outcomes`).Scan(&noInvoice, &started); err != nil || noInvoice != 1 || started != 0 {
		t.Fatalf("no_invoice outcomes = %d started = %d, %v", noInvoice, started, err)
	}
	lookupsBefore := lookups.Load()
	again, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !again.NoInvoice || again.Posted || posts.Load() != 0 || lookups.Load() != lookupsBefore {
		t.Fatalf("repeat = %+v %v posts=%d lookups %d -> %d", again, err, posts.Load(), lookupsBefore, lookups.Load())
	}
}

// A real POST that answers a bare 404 is not proof of anything. It is an
// unknown outcome for manual review, never nothing to invoice, and is not
// posted again.
func TestClosureInvoiceExecutionTreatsABareNotFoundPostAsUnknown(t *testing.T) {
	var m kbMode
	var posts, lookups atomic.Int64
	m.post.Store(3)
	e, tn := claimedClosure(t, &m, &posts, &lookups)
	ctx := context.Background()
	if result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || result.NoInvoice || posts.Load() != 1 {
		t.Fatalf("bare 404 = %+v %v posts=%d", result, err, posts.Load())
	}
	var unknown, noInvoice int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'post_unknown'), count(*) FILTER (WHERE kind = 'no_invoice')
FROM tenant_closure_invoice_outcomes`).Scan(&unknown, &noInvoice); err != nil || unknown != 1 || noInvoice != 0 {
		t.Fatalf("post_unknown = %d no_invoice = %d, %v", unknown, noInvoice, err)
	}
	if result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || result.NoInvoice || posts.Load() != 1 {
		t.Fatalf("repeat = %+v %v posts=%d", result, err, posts.Load())
	}
}

// The account is read with the brand credentials before anything is started.
// An unreadable account, a currency the brand does not bill in and another
// account all leave nothing started; the recorded currency is the account's.
func TestClosureInvoiceExecutionReadsTheAccountBeforeStarting(t *testing.T) {
	var m kbMode
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, &m, &posts, &lookups)
	ctx := context.Background()
	for name, mode := range map[string]int32{"unavailable": 1, "currency the brand does not bill in": 2, "another account": 3} {
		m.account.Store(mode)
		if _, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err == nil || posts.Load() != 0 {
			t.Fatalf("%s: err = %v posts=%d", name, err, posts.Load())
		}
		var outcomes int
		if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes`).Scan(&outcomes); err != nil || outcomes != 0 {
			t.Fatalf("%s: outcomes = %d, %v", name, outcomes, err)
		}
	}
	m.account.Store(0)
	if result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err != nil || !result.Posted || !result.Evidenced {
		t.Fatalf("execution = %+v %v", result, err)
	}
	var currency string
	if err := e.db.Pool.QueryRow(ctx, `SELECT expected_currency FROM tenant_closure_invoice_outcomes WHERE kind = 'started'`).Scan(&currency); err != nil || currency != "CAD" {
		t.Fatalf("recorded currency = %q, %v", currency, err)
	}
}

// An evidenced or read_mismatch outcome names the invoice ID that was
// recorded, and no other.
func TestClosureInvoiceOutcomeMustNameTheRecordedInvoice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status)
VALUES ($1, $2, now(), 'finance_pending')`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	attemptID, recorded := idgen.MustNewV7(), idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $2, $4::uuid, current_date)`, attemptID, tn.BrandID, tn.ID, tn.KillBillAccountID); err != nil {
		t.Fatal(err)
	}
	insert := func(kind string, invoice any, currency any) error {
		_, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, invoice_id, expected_currency) VALUES ($1, $2, $3, $4, $5)`,
			idgen.MustNewV7(), attemptID, kind, invoice, currency)
		return err
	}
	if err := insert("started", nil, "CAD"); err != nil {
		t.Fatal(err)
	}
	refused := func(kind string, invoice idgen.UUID, why string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if err := insert(kind, invoice, nil); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s %s = %v, want a check violation", kind, why, err)
		}
	}
	// With no invoice_id outcome recorded there is no invoice to name.
	refused("evidenced", recorded, "before any invoice_id outcome")
	if err := insert("invoice_id", recorded, nil); err != nil {
		t.Fatal(err)
	}
	refused("evidenced", idgen.MustNewV7(), "for another invoice")
	refused("read_mismatch", idgen.MustNewV7(), "for another invoice")
	if err := insert("evidenced", recorded, nil); err != nil {
		t.Fatalf("evidenced for the recorded invoice = %v", err)
	}
}

// A stop while the POST is in flight still records the invoice ID the answer
// carries, so the attempt is recovered by its read and never posted again.
func TestClosureInvoiceExecutionRecordsTheInvoiceIDWhenStoppedDuringThePost(t *testing.T) {
	var m kbMode
	var posts, lookups atomic.Int64
	m.post.Store(2)
	e, tn := claimedClosure(t, &m, &posts, &lookups)
	ctx := context.Background()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for cctx.Err() == nil {
			var started int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'started'`).Scan(&started); err == nil && started == 1 {
				time.Sleep(150 * time.Millisecond)
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	_, _ = tenant.ExecuteClosureInvoiceOnce(cctx, e.db.Pool, e.eng, tn.Scope())
	var invoiceOutcomes, unknown int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'invoice_id'), count(*) FILTER (WHERE kind = 'post_unknown')
FROM tenant_closure_invoice_outcomes`).Scan(&invoiceOutcomes, &unknown); err != nil || invoiceOutcomes != 1 || unknown != 0 || posts.Load() != 1 {
		t.Fatalf("invoice_id outcomes = %d post_unknown = %d posts = %d, %v", invoiceOutcomes, unknown, posts.Load(), err)
	}
}

// A database failure while the invoice ID is recorded, after Kill Bill has
// created the invoice, is retried instead of leaving the attempt unrecorded.
func TestClosureInvoiceExecutionRetriesRecordingTheInvoiceID(t *testing.T) {
	var m kbMode
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, &m, &posts, &lookups)
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE SEQUENCE closure_outcome_fail_seq`,
		`CREATE FUNCTION closure_outcome_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF nextval('closure_outcome_fail_seq') <= 2 THEN RAISE EXCEPTION 'injected failure'; END IF;
  RETURN NEW; END; $$`,
		`CREATE TRIGGER a_closure_outcome_fail BEFORE INSERT ON tenant_closure_invoice_outcomes
FOR EACH ROW WHEN (NEW.kind = 'invoice_id') EXECUTE FUNCTION closure_outcome_fail()`,
	} {
		if _, err := e.db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err != nil || !result.Posted || !result.Evidenced || posts.Load() != 1 {
		t.Fatalf("execution = %+v %v posts=%d", result, err, posts.Load())
	}
}

// An invoice ID or an unknown outcome and a no_invoice outcome exclude each
// other, so a real invoice can never be hidden by an answer of nothing. A start
// alone does not: a trigger that answers nothing to invoice is recorded after
// its start, as the test of the executor shows.
func TestClosureInvoiceNoInvoiceAndStartExcludeEachOther(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status)
VALUES ($1, $2, now(), 'finance_pending')`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	newAttempt := func(date string) idgen.UUID {
		id := idgen.MustNewV7()
		// An attempt per target date lets one tenant hold two attempts in the test only if the
		// table allows it; it holds one, so the second case below uses a fresh tenant.
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $2, $4::uuid, $5::date)`, id, tn.BrandID, tn.ID, tn.KillBillAccountID, date); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert := func(attempt idgen.UUID, kind string, invoice, currency any) error {
		_, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, invoice_id, expected_currency) VALUES ($1, $2, $3, $4, $5)`,
			idgen.MustNewV7(), attempt, kind, invoice, currency)
		return err
	}
	violation := func(err error, what string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s = %v, want a check violation", what, err)
		}
	}
	attempt := newAttempt("2026-12-31")
	if err := insert(attempt, "started", nil, "CAD"); err != nil {
		t.Fatal(err)
	}
	if err := insert(attempt, "post_unknown", nil, nil); err != nil {
		t.Fatal(err)
	}
	violation(insert(attempt, "no_invoice", nil, nil), "no_invoice after post_unknown")
	recorded := idgen.MustNewV7()
	if err := insert(attempt, "invoice_id", recorded, nil); err != nil {
		t.Fatal(err)
	}
	violation(insert(attempt, "no_invoice", nil, nil), "no_invoice after invoice_id")
}

// The other direction, with its own tenant: no_invoice first, then started.
func TestClosureInvoiceStartIsRefusedAfterNoInvoice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status)
VALUES ($1, $2, now(), 'finance_pending')`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	attempt := idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $2, $4::uuid, current_date)`, attempt, tn.BrandID, tn.ID, tn.KillBillAccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind) VALUES ($1, $2, 'no_invoice')`, idgen.MustNewV7(), attempt); err != nil {
		t.Fatal(err)
	}
	_, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, expected_currency) VALUES ($1, $2, 'started', 'CAD')`, idgen.MustNewV7(), attempt)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("started after no_invoice = %v, want a check violation", err)
	}
	for kind, invoice := range map[string]any{"post_unknown": nil, "invoice_id": idgen.MustNewV7()} {
		_, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, invoice_id) VALUES ($1, $2, $3, $4)`, idgen.MustNewV7(), attempt, kind, invoice)
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s after no_invoice = %v, want a check violation", kind, err)
		}
	}
}

// The timeline that hid a real invoice: executor Y reads that nothing is
// recorded, executor X starts and posts, and only then Y's dry run answers
// 204. Y must not record no_invoice.
func TestClosureInvoiceStaleDryRunCannotHideARealInvoice(t *testing.T) {
	m := &kbMode{holdFirstDry: true, dryHeld: make(chan struct{}), dryRelease: make(chan struct{})}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	ctx := context.Background()
	type outcome struct {
		result tenant.ClosureExecutionResult
		err    error
	}
	yDone := make(chan outcome, 1)
	go func() {
		result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
		yDone <- outcome{result, err}
	}()
	<-m.dryHeld
	// Executor X runs to the end while Y waits in its dry run.
	if result, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope()); err != nil || !result.Posted || !result.Evidenced {
		t.Fatalf("executor X = %+v %v", result, err)
	}
	close(m.dryRelease)
	y := <-yDone
	if y.err == nil || y.result.NoInvoice {
		t.Fatalf("executor Y = %+v %v, want a refusal", y.result, y.err)
	}
	var noInvoice, started, evidenced int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'no_invoice'), count(*) FILTER (WHERE kind = 'started'), count(*) FILTER (WHERE kind = 'evidenced')
FROM tenant_closure_invoice_outcomes`).Scan(&noInvoice, &started, &evidenced); err != nil || noInvoice != 0 || started != 1 || evidenced != 1 || posts.Load() != 1 {
		t.Fatalf("no_invoice = %d started = %d evidenced = %d posts = %d, %v", noInvoice, started, evidenced, posts.Load(), err)
	}
}

// When every try to record the invoice ID fails, the error names the
// invoice, so staff can find it in Kill Bill.
func TestClosureInvoiceFinalizeFailureNamesTheInvoice(t *testing.T) {
	var m kbMode
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, &m, &posts, &lookups)
	ctx := context.Background()
	for _, sql := range []string{
		`CREATE FUNCTION closure_outcome_always_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END; $$`,
		`CREATE TRIGGER a_closure_outcome_always_fail BEFORE INSERT ON tenant_closure_invoice_outcomes
FOR EACH ROW WHEN (NEW.kind = 'invoice_id') EXECUTE FUNCTION closure_outcome_always_fail()`,
	} {
		if _, err := e.db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	_, err := tenant.ExecuteClosureInvoiceOnce(ctx, e.db.Pool, e.eng, tn.Scope())
	if err == nil || !regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`).MatchString(err.Error()) {
		t.Fatalf("error = %v, want it to name the invoice ID", err)
	}
}

// The database alone keeps an unknown outcome and no_invoice from both being
// recorded, even when two transactions insert them at the same time: the
// second waits for the first and then finds it.
func TestClosureInvoiceExclusionHoldsForConcurrentInserts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status)
VALUES ($1, $2, now(), 'finance_pending')`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	attempt := idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $2, $4::uuid, current_date)`, attempt, tn.BrandID, tn.ID, tn.KillBillAccountID); err != nil {
		t.Fatal(err)
	}
	first, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	second, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(ctx)
	if _, err := first.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind) VALUES ($1, $2, 'post_unknown')`, idgen.MustNewV7(), attempt); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := second.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind) VALUES ($1, $2, 'no_invoice')`, idgen.MustNewV7(), attempt)
		done <- err
	}()
	// The second insert must wait for the first transaction.
	select {
	case err := <-done:
		t.Fatalf("no_invoice was not held back by a concurrent unknown outcome: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if err := <-done; err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("no_invoice after the concurrent unknown outcome = %v, want a check violation", err)
	}
	var unknown, noInvoice int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind = 'post_unknown'), count(*) FILTER (WHERE kind = 'no_invoice') FROM tenant_closure_invoice_outcomes`).Scan(&unknown, &noInvoice); err != nil || unknown != 1 || noInvoice != 0 {
		t.Fatalf("post_unknown = %d no_invoice = %d, %v", unknown, noInvoice, err)
	}
}
