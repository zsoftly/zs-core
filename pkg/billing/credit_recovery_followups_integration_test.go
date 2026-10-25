//go:build integration

package billing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/idempotency"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

const manualReviewMessage = "Credit recovery requires manual review."

// seedPending puts a pending credit row of the route into tenant a with its
// original target stored, as a request that lost its answer leaves it, and
// returns its ID, the description of its credit and the retry that resumes it.
func seedPending(t *testing.T, e *env, a tenant.Tenant, route, key string) (id idgen.UUID, description string, retry func(*billing.Service) (billing.Credit, bool, error)) {
	t.Helper()
	ctx := context.Background()
	id = idgen.MustNewV7()
	description = "zs-core credit " + idgen.Format(idgen.Credit, id)
	if route == "redeem" {
		code := e.code(billing.NewCreditCode{Code: key, AmountMinor: 700, Currency: "CAD", Reason: "x", CreatedBy: "ops"})
		codeID, err := idgen.ParseAs(idgen.CreditCode, code.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO credit_code_redemptions
    (id, brand_id, tenant_id, credit_code_id, amount_minor, currency, actor_type, actor_id, correlation_id,
     original_endpoint_kind, original_killbill_tenant_id, original_killbill_account_id, original_currency, original_amount_minor, original_description)
VALUES ($1, $2, $3, $4, 700, 'CAD', 'user', $5, 'receipt', 'redeem', $2, $6::uuid, 'CAD', 700, $7)`,
			id, a.BrandID, a.ID, codeID, e.owner.Identity.PublicID(), a.KillBillAccountID, description); err != nil {
			t.Fatal(err)
		}
		return id, description, func(svc *billing.Service) (billing.Credit, bool, error) { return svc.Redeem(ctx, a.Scope(), e.call(), key) }
	}
	req := billing.StaffCredit{AmountMinor: 700, Currency: "CAD", IdempotencyKey: key}
	payload, err := json.Marshal(map[string]any{"amount_minor": req.AmountMinor, "currency": req.Currency, "reason": "goodwill", "expires_at": req.ExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := idempotency.Fingerprint("billing.credit.grant", payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO staff_credits
    (id, brand_id, tenant_id, idempotency_key, fingerprint, amount_minor, currency, reason, actor_type, actor_id, correlation_id,
     original_endpoint_kind, original_killbill_tenant_id, original_killbill_account_id, original_currency, original_amount_minor, original_description)
VALUES ($1, $2, $3, $4, $5, 700, 'CAD', 'goodwill', 'staff', 'sub:staff-1', 'receipt', 'grant', $2, $6::uuid, 'CAD', 700, $7)`,
		id, a.BrandID, a.ID, req.IdempotencyKey, fingerprint, a.KillBillAccountID, description); err != nil {
		t.Fatal(err)
	}
	return id, description, func(svc *billing.Service) (billing.Credit, bool, error) {
		return svc.GrantCredit(ctx, a.Scope(), e.staff("goodwill"), req)
	}
}

// historyStub is a Kill Bill whose invoice history is what the test says, whose
// credit POSTs are counted and refused, and which answers history with status
// when it is not zero.
func historyStub(t *testing.T, a tenant.Tenant, items []killbill.InvoiceItem, invoiceStatus string, status int) (*killbill.Client, *atomic.Int64) {
	t.Helper()
	var posts atomic.Int64
	invoiceID := idgen.MustNewV7().String()
	for i := range items {
		items[i].InvoiceID = invoiceID
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/credits":
			posts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/1.0/kb/accounts/") && strings.HasSuffix(r.URL.Path, "/invoices"):
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]killbill.Invoice{{InvoiceID: invoiceID, Status: invoiceStatus, AccountID: a.KillBillAccountID, Currency: "CAD", Items: items}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return kb, &posts
}

func receipt(account, itemID, description string) killbill.InvoiceItem {
	return killbill.InvoiceItem{InvoiceItemID: itemID, AccountID: account, ItemType: "CREDIT_ADJ", Description: description, Amount: json.Number("-7.00"), Currency: "CAD"}
}

func withKB(e *env, kb *killbill.Client, log *slog.Logger) *billing.Service {
	svc := *e.svc
	svc.KillBill = kb
	svc.Log = log
	return &svc
}

func setBrandTenant(t *testing.T, e *env) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE brands SET killbill_tenant_id = $2 WHERE id = $1`, e.brand.ID, e.brand.ID); err != nil {
		t.Fatal(err)
	}
}

func rowStatus(t *testing.T, e *env, table string, id idgen.UUID) string {
	t.Helper()
	var s string
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT status FROM `+table+` WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A Kill Bill receipt belongs to one credit, whichever source made it. When two
// credits, a redemption and a staff grant, would take the same receipt, the
// second is left pending for a person and nothing is posted.
func TestCreditReceiptHasOneOwnerAcrossSources(t *testing.T) {
	e := newEnv(t)
	setBrandTenant(t, e)
	a := e.tenant("Tenant A")
	redeemID, redeemDesc, redeem := seedPending(t, e, a, "redeem", "OWNER-REDEEM")
	grantID, grantDesc, grant := seedPending(t, e, a, "grant", "owner-grant")
	shared := idgen.MustNewV7().String()
	kb, posts := historyStub(t, a, []killbill.InvoiceItem{receipt(a.KillBillAccountID, shared, redeemDesc), receipt(a.KillBillAccountID, shared, grantDesc)}, "COMMITTED", 0)
	svc := withKB(e, kb, nil)
	if _, created, err := redeem(svc); err != nil || !created {
		t.Fatalf("redemption recovery = created %v, err %v", created, err)
	}
	if _, created, err := grant(svc); err == nil || created || !strings.Contains(err.Error(), manualReviewMessage) {
		t.Fatalf("grant recovery on a receipt another credit owns = created %v, err %v, want manual review", created, err)
	}
	if posts.Load() != 0 {
		t.Fatalf("credit POSTs = %d, want 0", posts.Load())
	}
	if rowStatus(t, e, "credit_code_redemptions", redeemID) != "completed" || rowStatus(t, e, "staff_credits", grantID) != "pending" {
		t.Fatal("the redemption must be completed and the grant left pending")
	}
	// The database says the same: the receipt of the redemption cannot be put on
	// the grant by any statement.
	_, err := e.db.Pool.Exec(context.Background(), `UPDATE staff_credits SET killbill_credit_id = $2::uuid WHERE id = $1`, grantID, shared)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("a receipt put on a second credit = %v, want a check violation", err)
	}
}

// Two credits that race for one receipt: exactly one gets it.
func TestCreditReceiptOwnershipIsSerialized(t *testing.T) {
	e := newEnv(t)
	setBrandTenant(t, e)
	a := e.tenant("Tenant A")
	redeemID, _, _ := seedPending(t, e, a, "redeem", "RACE-REDEEM")
	grantID, _, _ := seedPending(t, e, a, "grant", "race-grant")
	receiptID := idgen.MustNewV7().String()
	var wg sync.WaitGroup
	var won atomic.Int64
	for _, stmt := range []struct{ table string; id idgen.UUID }{{"credit_code_redemptions", redeemID}, {"staff_credits", grantID}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.db.Pool.Exec(context.Background(), `UPDATE `+stmt.table+` SET killbill_credit_id = $2::uuid WHERE id = $1`, stmt.id, receiptID); err == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d credits took the same receipt, want exactly 1", won.Load())
	}
}

// Every way the invoice history can fail to show the one receipt leaves the
// credit pending with the same manual-review answer and one log line that names
// the reason, whatever the route, and posts nothing.
func TestCreditRecoveryDiagnosticsAreUniform(t *testing.T) {
	for _, route := range []string{"redeem", "grant"} {
		for _, tc := range []struct {
			name, reason, amount, invoiceStatus string
			count, status                       int
			foreign                             bool
		}{
			{name: "history unavailable", reason: "history_unavailable", status: http.StatusServiceUnavailable},
			{name: "history not found", reason: "history_unavailable", status: http.StatusNotFound},
			{name: "receipt absent", reason: "receipt_absent"},
			{name: "receipt duplicated", reason: "receipt_ambiguous", amount: "-7.00", count: 2},
			{name: "receipt not committed", reason: "receipt_not_committed", amount: "-7.00", count: 1, invoiceStatus: "DRAFT"},
			{name: "receipt of another account", reason: "history_foreign_account", amount: "-7.00", count: 1, foreign: true},
			{name: "receipt of another amount", reason: "receipt_mismatch", amount: "7.00", count: 1},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				e := newEnv(t)
				setBrandTenant(t, e)
				a := e.tenant("Tenant A")
				id, description, retry := seedPending(t, e, a, route, "DIAG-"+strings.ToUpper(route))
				account := a.KillBillAccountID
				if tc.foreign {
					account = idgen.MustNewV7().String()
				}
				var items []killbill.InvoiceItem
				for range tc.count {
					it := receipt(account, idgen.MustNewV7().String(), description)
					it.Amount = json.Number(tc.amount)
					items = append(items, it)
				}
				status := tc.invoiceStatus
				if status == "" {
					status = "COMMITTED"
				}
				kb, posts := historyStub(t, a, items, status, tc.status)
				if tc.foreign {
					// The history names another account on its invoice.
					kb = foreignAccountKB(t, items, a)
				}
				var logs bytes.Buffer
				svc := withKB(e, kb, slog.New(slog.NewTextHandler(&logs, nil)))
				_, created, err := retry(svc)
				table := map[string]string{"redeem": "credit_code_redemptions", "grant": "staff_credits"}[route]
				if err == nil || created || !strings.Contains(err.Error(), manualReviewMessage) {
					t.Fatalf("retry = created %v, err %v, want the manual-review answer", created, err)
				}
				if rowStatus(t, e, table, id) != "pending" || posts.Load() != 0 {
					t.Fatalf("status %q, credit POSTs %d, want pending and 0", rowStatus(t, e, table, id), posts.Load())
				}
				if !strings.Contains(logs.String(), "credit recovery needs review") || !strings.Contains(logs.String(), "reason="+tc.reason) {
					t.Fatalf("log = %q, want one line that says credit recovery needs review with reason=%s", logs.String(), tc.reason)
				}
			})
		}
	}
}

// foreignAccountKB answers the history with an invoice of another account.
func foreignAccountKB(t *testing.T, items []killbill.InvoiceItem, a tenant.Tenant) *killbill.Client {
	t.Helper()
	other := idgen.MustNewV7().String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/invoices") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]killbill.Invoice{{InvoiceID: idgen.MustNewV7().String(), Status: "COMMITTED", AccountID: other, Currency: "CAD", Items: items}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_ = a
	return kb
}

// The recovery of one tenant's credit never touches another tenant's, a retry
// that races itself completes it once, and the completion has its side effects
// exactly once: the receipt on the row, one audit entry, one wallet version.
func TestCreditRecoveryIdentityConcurrencyAndCompletionEffects(t *testing.T) {
	e := newEnv(t)
	setBrandTenant(t, e)
	ctx := context.Background()
	a, b := e.tenant("Tenant A"), e.tenant("Tenant B")
	id, description, retry := seedPending(t, e, a, "redeem", "EFFECTS-REDEEM")
	otherID, otherDescription, otherRetry := seedPending(t, e, b, "redeem", "EFFECTS-OTHER")
	item := receipt(a.KillBillAccountID, idgen.MustNewV7().String(), description)
	kb, posts := historyStub(t, a, []killbill.InvoiceItem{item}, "COMMITTED", 0)
	svc := withKB(e, kb, nil)

	// Tenant B's retry, with a history that holds only tenant A's receipt, does
	// not complete A's credit or its own.
	if _, created, err := otherRetry(svc); err == nil || created {
		t.Fatalf("tenant B retry = created %v, err %v, want it refused", created, err)
	}
	if rowStatus(t, e, "credit_code_redemptions", id) != "pending" || rowStatus(t, e, "credit_code_redemptions", otherID) != "pending" {
		t.Fatal("a retry of tenant B changed a credit")
	}
	_ = otherDescription

	// Tenant A's retry, five at once.
	var wg sync.WaitGroup
	var created, replayed, failed atomic.Int64
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, c, err := retry(svc)
			switch {
			case err != nil:
				failed.Add(1)
			case c:
				created.Add(1)
			default:
				replayed.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 || failed.Load() != 0 || created.Load()+replayed.Load() != 5 || posts.Load() != 0 {
		t.Fatalf("created %d replayed %d failed %d, credit POSTs %d, want exactly one completion and no POST", created.Load(), replayed.Load(), failed.Load(), posts.Load())
	}
	var status, creditID, invoiceID string
	if err := e.db.Pool.QueryRow(ctx, `SELECT status, killbill_credit_id::text, killbill_invoice_id::text FROM credit_code_redemptions WHERE id = $1`, id).Scan(&status, &creditID, &invoiceID); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || creditID != item.InvoiceItemID || invoiceID == "" {
		t.Fatalf("row = %q %q %q, want completed with the receipt %s and its invoice", status, creditID, invoiceID, item.InvoiceItemID)
	}
	var audits, version int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'billing.credit.redeemed'`, a.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("redeemed audits = %d, %v, want 1", audits, err)
	}
	if err := e.db.Pool.QueryRow(ctx, `SELECT version FROM wallet_versions WHERE tenant_id = $1`, a.ID).Scan(&version); err != nil || version != 1 {
		t.Fatalf("wallet version = %d, %v, want 1", version, err)
	}
}
