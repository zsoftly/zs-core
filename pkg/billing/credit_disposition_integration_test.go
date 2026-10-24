//go:build integration

package billing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/idempotency"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

func TestCreditRecoveryMatchesOnlyExactCommittedReceipt(t *testing.T) {
	ctx := context.Background()
	type receiptCase struct {
		name    string
		amount  string
		count   int
		match   bool
		status  int
		draft   bool
		foreign bool
	}
	for _, route := range []string{"redeem", "grant"} {
		for _, tc := range []receiptCase{
			{name: "matching committed receipt", amount: "-7.00", count: 1, match: true},
			{name: "wrong positive amount", amount: "7.00", count: 1},
			{name: "near rounding amount", amount: "-7.004", count: 1},
			{name: "exponent beyond bound", amount: "-7e513", count: 1},
			{name: "missing receipt", count: 0},
			{name: "duplicate receipt", amount: "-7.00", count: 2},
			{name: "draft receipt", amount: "-7.00", count: 1, draft: true},
			{name: "foreign receipt", amount: "-7.00", count: 1, foreign: true},
			{name: "history not found", status: http.StatusNotFound},
			{name: "history unavailable", status: http.StatusServiceUnavailable},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				e := newEnv(t)
				a := e.tenant("Tenant A")
				if _, err := e.db.Pool.Exec(ctx, `UPDATE brands SET killbill_tenant_id = $2 WHERE id = $1`, e.brand.ID, e.brand.ID); err != nil {
					t.Fatal(err)
				}
				id := idgen.MustNewV7()
				description := "zs-core credit " + idgen.Format(idgen.Credit, id)
				var retry func() (billing.Credit, bool, error)
				if route == "redeem" {
					code := e.code(billing.NewCreditCode{Code: "RECEIPT-REDEEM", AmountMinor: 700, Currency: "CAD", Reason: "x", CreatedBy: "ops"})
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
					retry = func() (billing.Credit, bool, error) { return e.svc.Redeem(ctx, a.Scope(), e.call(), "RECEIPT-REDEEM") }
				} else {
					req := billing.StaffCredit{AmountMinor: 700, Currency: "CAD", IdempotencyKey: "receipt-grant"}
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
					retry = func() (billing.Credit, bool, error) {
						return e.svc.GrantCredit(ctx, a.Scope(), e.staff("goodwill"), req)
					}
				}

				var posts atomic.Int64
				invoiceID := idgen.MustNewV7().String()
				itemAccountID := a.KillBillAccountID
				if tc.foreign {
					itemAccountID = idgen.MustNewV7().String()
				}
				items := make([]killbill.InvoiceItem, 0, tc.count)
				for range tc.count {
					items = append(items, killbill.InvoiceItem{InvoiceItemID: idgen.MustNewV7().String(), InvoiceID: invoiceID,
						AccountID: itemAccountID, ItemType: "CREDIT_ADJ", Description: description, Amount: json.Number(tc.amount), Currency: "CAD"})
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/credits" {
						posts.Add(1)
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/1.0/kb/accounts/") && strings.HasSuffix(r.URL.Path, "/invoices") {
						if tc.status != 0 {
							w.WriteHeader(tc.status)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						invoiceStatus := "COMMITTED"
						if tc.draft {
							invoiceStatus = "DRAFT"
						}
						if err := json.NewEncoder(w).Encode([]killbill.Invoice{{InvoiceID: invoiceID, Status: invoiceStatus, AccountID: a.KillBillAccountID, Currency: "CAD", Items: items}}); err != nil {
							t.Error(err)
						}
						return
					}
					http.NotFound(w, r)
				}))
				t.Cleanup(server.Close)
				kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
				if err != nil {
					t.Fatal(err)
				}
				svc := *e.svc
				svc.KillBill = kb
				if route == "redeem" {
					retry = func() (billing.Credit, bool, error) { return svc.Redeem(ctx, a.Scope(), e.call(), "RECEIPT-REDEEM") }
				} else {
					req := billing.StaffCredit{AmountMinor: 700, Currency: "CAD", IdempotencyKey: "receipt-grant"}
					retry = func() (billing.Credit, bool, error) { return svc.GrantCredit(ctx, a.Scope(), e.staff("goodwill"), req) }
				}

				_, created, err := retry()
				if tc.match {
					if err != nil || !created {
						t.Fatalf("matching retry = created %v, err %v", created, err)
					}
				} else if created || err == nil || strings.HasPrefix(codeOf(err), "untyped:") {
					t.Fatalf("nonmatching retry = created %v, err %v", created, err)
				}
				if posts.Load() != 0 {
					t.Fatalf("credit POSTs = %d, want 0", posts.Load())
				}
				table := "credit_code_redemptions"
				if route == "grant" {
					table = "staff_credits"
				}
				var status string
				if err := e.db.Pool.QueryRow(ctx, `SELECT status FROM `+table+` WHERE id = $1`, id).Scan(&status); err != nil {
					t.Fatal(err)
				}
				wantStatus := "pending"
				if tc.match {
					wantStatus = "completed"
				}
				if status != wantStatus {
					t.Fatalf("status = %q, want %q", status, wantStatus)
				}
			})
		}
	}
}
