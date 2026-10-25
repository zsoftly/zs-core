//go:build integration

package metering_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

var staffCall = metering.StaffCall{
	Call:   metering.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:op"}, CorrelationID: "corr-resolve"},
	Reason: "checked against Kill Bill",
}

// unconfirmedRollup leaves one rollup whose send Kill Bill took and whose answer
// was lost, and returns its public ID, its scope and the account.
func unconfirmedRollup(t *testing.T, e *env, b enginetest.Brand, name string) (rollupID string, sc scope.Scope, account string) {
	t.Helper()
	ctx := context.Background()
	tn := e.tenant(b, name)
	e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "u-1", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	e.fake.LoseAnswers("/1.0/kb/usages", 1)
	if report, err := e.svc.Forward(ctx); err != nil || report.Unconfirmed != 1 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var id idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return idgen.Format(idgen.Rollup, id), tn.Scope(), tn.KillBillAccountID
}

// closingAt makes the tenant closing with a settlement at status, the
// cancellation done on cancelDay, and, when the settlement is past finance,
// the finance step done too.
func (e *env) closingAt(sc scope.Scope, status, cancelDay string) {
	e.t.Helper()
	ctx := context.Background()
	financed := status == "blocking"
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status, cancellation_completed_at, drain_completed_at, finance_target_date, finance_completed_at)
VALUES ($1, $2, now(), $3, $4::timestamptz, now(), current_date, CASE WHEN $5 THEN now() END)`, sc.Brand, sc.Tenant, status, cancelDay+"T12:00:00Z", financed); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE id = $1`, sc.Tenant); err != nil {
		e.t.Fatal(err)
	}
}

// noInvoiceOutcome records that the closure invoice found nothing to invoice.
func (e *env) noInvoiceOutcome(sc scope.Scope, account string) {
	e.t.Helper()
	ctx := context.Background()
	attempt := idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date) VALUES ($1, $2, $3, $2, $4::uuid, current_date)`, attempt, sc.Brand, sc.Tenant, account); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind) VALUES ($1, $2, 'no_invoice')`, idgen.MustNewV7(), attempt); err != nil {
		e.t.Fatal(err)
	}
}

// withInvoices is the metering service with a Kill Bill that answers the
// account's invoice history with the invoices given.
func withInvoices(t *testing.T, e *env, invoices []killbill.Invoice) *metering.Service {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/invoices") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(invoices)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	bill := *e.svc.Billing
	bill.KillBill = kb
	return &metering.Service{DB: e.db.Pool, Billing: &bill, Now: e.clock.Now}
}

func rollupSubscription(t *testing.T, e *env, sc scope.Scope) string {
	t.Helper()
	var sub string
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT subscription_id::text FROM usage_rollups WHERE tenant_id = $1`, sc.Tenant).Scan(&sub); err != nil {
		t.Fatal(err)
	}
	return sub
}

// A send that started and was never confirmed does not hold the drain: nothing
// more will be sent for it, so waiting gains nothing. A claim that never started
// a send does hold it.
func TestDrainedDoesNotWaitForAStartedSendButWaitsForANeverStartedClaim(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-drain-unconfirmed")
	_, sc, _ := unconfirmedRollup(t, e, b, "Started Co")
	e.closing(tenant.Tenant{BrandID: sc.Brand, ID: sc.Tenant})
	if done, err := e.svc.Drained(ctx, sc); err != nil || !done {
		t.Fatalf("drained with a started, unconfirmed send = %v %v, want true", done, err)
	}

	other := e.tenant(b, "Claim Co")
	e.subscribe(b, other, enginetest.PlanMetered)
	if _, _, err := e.record(other.Scope(), metering.Event{Meter: "api-calls", EventID: "c-1", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	e.svc.ClaimTTL = time.Second
	osc := other.Scope()
	if n, err := e.svc.RollUp(ctx, osc); err != nil || n != 1 {
		t.Fatalf("rollup = %d %v", n, err)
	}
	if due, err := e.svc.ClaimDue(ctx, &osc); err != nil || len(due) != 1 {
		t.Fatalf("claim = %d %v", len(due), err)
	}
	time.Sleep(1500 * time.Millisecond)
	if n, err := e.svc.ResolveStranded(ctx); err != nil || n != 1 {
		t.Fatalf("resolve stranded = %d %v", n, err)
	}
	e.closing(other)
	if done, err := e.svc.Drained(ctx, osc); err != nil || done {
		t.Fatalf("drained with a claim that never started a send = %v %v, want false", done, err)
	}
}

// Staff confirm an unconfirmed rollup with Kill Bill's invoice as the evidence,
// and the confirmation is kept, audited and idempotent.
func TestStaffResolveAnUnconfirmedRollupWithInvoiceEvidence(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve")
	rollup, sc, account := unconfirmedRollup(t, e, b, "Resolve Co")
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, metering.ResolveDecisionForwarded); codeOf(err) != "conflict" {
		t.Fatalf("resolve without evidence = %v, want conflict", err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollup_resolutions WHERE tenant_id = $1`, sc.Tenant); n != 0 {
		t.Fatalf("resolutions after a refused resolve = %d", n)
	}
	invoice := e.fake.InvoiceUsage(b.Slug, account, e.clock.Now().AddDate(0, 0, -2), e.clock.Now().AddDate(0, 0, 2))
	got, replayed, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, metering.ResolveDecisionForwarded)
	if err != nil || replayed || got.InvoiceID != invoice || got.Decision != "forwarded" {
		t.Fatalf("resolve = %+v replayed %v %v, want forwarded with invoice %s", got, replayed, err, invoice)
	}
	if again, replayed, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, metering.ResolveDecisionForwarded); err != nil || !replayed || again.InvoiceID != invoice {
		t.Fatalf("repeat = %+v replayed %v %v", again, replayed, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'metering.rollup_resolved' AND reason = $2`, sc.Tenant, staffCall.Reason); n != 1 {
		t.Fatalf("resolution audits = %d, want 1", n)
	}
	fs, err := doctor.ClosureFindings(ctx, e.db.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Check == "usage_unconfirmed" {
			t.Fatalf("a resolved rollup is still reported: %+v", f)
		}
	}
}

// An invoice that is not committed is no evidence that Kill Bill bills the
// record.
func TestStaffResolveNeedsACommittedInvoice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve-draft")
	rollup, sc, account := unconfirmedRollup(t, e, b, "Draft Co")
	var tracking string
	if err := e.db.Pool.QueryRow(ctx, `SELECT tracking_id FROM usage_rollups WHERE tenant_id = $1`, sc.Tenant).Scan(&tracking); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/invoices") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]killbill.Invoice{{InvoiceID: idgen.MustNewV7().String(), Status: "DRAFT", AccountID: account, Currency: "CAD", TrackingIDs: []string{tracking}}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	kb, err := killbill.New(killbill.Config{URL: server.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	bill := *e.svc.Billing
	bill.KillBill = kb
	svc := &metering.Service{DB: e.db.Pool, Billing: &bill, Now: e.clock.Now}
	if _, _, err := svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, metering.ResolveDecisionForwarded); codeOf(err) != "conflict" {
		t.Fatalf("resolve on a draft invoice = %v, want conflict", err)
	}
}

// Not billed says Kill Bill has not billed the record and will not. It needs the
// cancel's own invoice, because that invoice bills the arrears and Kill Bill makes
// it a little after the cancel, so an invoice of that day that is another one proves
// nothing: a committed invoice dated on or after the cancellation that carries a
// repair or usage item of the subscription of the rollup, or a finance step that
// found nothing to invoice. It also needs the settlement past finance, the
// cancel's own invoice trigger having run.
func TestStaffResolveAsNotBilledNeedsTheCancelsOwnInvoice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve-lost")
	invoice := func(status, date string, items ...killbill.InvoiceItem) killbill.Invoice {
		return killbill.Invoice{InvoiceID: idgen.MustNewV7().String(), Status: status, InvoiceDate: date, Items: items}
	}
	type scenario struct {
		name     string
		settle   string
		invoices func(sub string) []killbill.Invoice
		noInv    bool
		want     string
	}
	for _, tc := range []scenario{
		{name: "the finance step has not run", settle: "finance_pending", want: "conflict",
			invoices: func(sub string) []killbill.Invoice {
				return []killbill.Invoice{invoice("COMMITTED", "2026-10-20", killbill.InvoiceItem{ItemType: "REPAIR_ADJ", SubscriptionID: sub})}
			}},
		{name: "an invoice of the day that has no item of the subscription", settle: "blocking", want: "conflict",
			invoices: func(string) []killbill.Invoice {
				return []killbill.Invoice{invoice("COMMITTED", "2026-10-20", killbill.InvoiceItem{ItemType: "RECURRING", SubscriptionID: idgen.MustNewV7().String()})}
			}},
		{name: "an invoice of the day with a recurring item of the subscription", settle: "blocking", want: "conflict",
			invoices: func(sub string) []killbill.Invoice {
				return []killbill.Invoice{invoice("COMMITTED", "2026-10-20", killbill.InvoiceItem{ItemType: "RECURRING", SubscriptionID: sub})}
			}},
		{name: "a repair item of the subscription on an invoice before the cancellation", settle: "blocking", want: "conflict",
			invoices: func(sub string) []killbill.Invoice {
				return []killbill.Invoice{invoice("COMMITTED", "2026-10-10", killbill.InvoiceItem{ItemType: "REPAIR_ADJ", SubscriptionID: sub})}
			}},
		{name: "a repair item of the subscription on an invoice that is not committed", settle: "blocking", want: "conflict",
			invoices: func(sub string) []killbill.Invoice {
				return []killbill.Invoice{invoice("DRAFT", "2026-10-20", killbill.InvoiceItem{ItemType: "REPAIR_ADJ", SubscriptionID: sub})}
			}},
		{name: "no invoice at all", settle: "blocking", want: "conflict",
			invoices: func(string) []killbill.Invoice { return nil }},
		{name: "the cancel's own invoice with a repair item", settle: "blocking", want: "ok",
			invoices: func(sub string) []killbill.Invoice {
				return []killbill.Invoice{invoice("COMMITTED", "2026-10-20", killbill.InvoiceItem{ItemType: "REPAIR_ADJ", SubscriptionID: sub})}
			}},
		{name: "the cancel's own invoice with a usage item", settle: "blocking", want: "ok",
			invoices: func(sub string) []killbill.Invoice {
				return []killbill.Invoice{invoice("COMMITTED", "2026-10-20", killbill.InvoiceItem{ItemType: "USAGE", SubscriptionID: sub})}
			}},
		{name: "a finance step that found nothing to invoice", settle: "blocking", noInv: true, want: "ok",
			invoices: func(string) []killbill.Invoice { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rollup, sc, account := unconfirmedRollup(t, e, b, "Lost "+tc.name)
			e.closingAt(sc, tc.settle, "2026-10-19")
			if tc.noInv {
				e.noInvoiceOutcome(sc, account)
			}
			svc := withInvoices(t, e, tc.invoices(rollupSubscription(t, e, sc)))
			got, _, err := svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, metering.ResolveDecisionNotBilled)
			if tc.want == "conflict" {
				if codeOf(err) != "conflict" {
					t.Fatalf("not_billed = %+v %v, want conflict", got, err)
				}
				return
			}
			if err != nil || got.Decision != "not_billed" || got.UnbilledQuantityMicro != 2_000_000 || got.InvoiceID != "" {
				t.Fatalf("not_billed = %+v %v, want the quantity 2000000 and no invoice", got, err)
			}
			if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'metering.rollup_resolved' AND details->>'decision' = 'not_billed' AND details->>'unbilled_quantity_micro' = '2000000'`, sc.Tenant); n != 1 {
				t.Fatalf("not_billed audits = %d, want 1 with the quantity", n)
			}
			if blocked := count(t, e.db, `SELECT count(*) FROM (SELECT tenant_closure_review_blocker($1, $2) AS b) q WHERE b IS NOT NULL`, sc.Brand, sc.Tenant); blocked != 0 {
				t.Fatal("the closure is still blocked after the usage was recorded as not billed")
			}
		})
	}
}

// Not billed is refused when a committed invoice does carry the record: that is
// a forwarded record, and has to be confirmed as that.
func TestStaffResolveNotBilledIsRefusedWhenAnInvoiceCarriesTheRecord(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve-billed")
	rollup, sc, account := unconfirmedRollup(t, e, b, "Billed Co")
	e.closingAt(sc, "blocking", "2026-10-19")
	e.fake.InvoiceUsage(b.Slug, account, e.clock.Now().AddDate(0, 0, -2), e.clock.Now().AddDate(0, 0, 2))
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, metering.ResolveDecisionNotBilled); codeOf(err) != "conflict" {
		t.Fatalf("not_billed when an invoice carries the record = %v, want conflict", err)
	}
}

// Only an unconfirmed rollup of the tenant can be resolved, with a reason and a
// known decision.
func TestStaffResolveRefusesWhatIsNotUnconfirmed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve-no")
	rollup, sc, _ := unconfirmedRollup(t, e, b, "Resolve No")
	other := e.tenant(b, "Pending Co")
	e.subscribe(b, other, enginetest.PlanMetered)
	e.fake.SetDown(false, true)
	if _, _, err := e.record(other.Scope(), metering.Event{Meter: "api-calls", EventID: "p-1", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Forward(ctx); err != nil {
		t.Fatal(err)
	}
	var pending idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM usage_rollups WHERE tenant_id = $1`, other.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, other.Scope(), staffCall, idgen.Format(idgen.Rollup, pending), metering.ResolveDecisionForwarded); codeOf(err) != "conflict" {
		t.Fatalf("resolve of a pending rollup = %v, want conflict", err)
	}
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, other.Scope(), staffCall, rollup, metering.ResolveDecisionForwarded); codeOf(err) != "not_found" {
		t.Fatalf("resolve through another tenant = %v, want not_found", err)
	}
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, metering.StaffCall{Call: staffCall.Call}, rollup, metering.ResolveDecisionForwarded); codeOf(err) != "reason_required" {
		t.Fatalf("resolve without a reason = %v, want reason_required", err)
	}
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, "forfeit"); codeOf(err) != "validation_failed" {
		t.Fatalf("resolve with an unknown decision = %v, want validation_failed", err)
	}
}

// The database refuses a resolution of a rollup that is not an unconfirmed
// started send, a not_billed before the cancellation, a decision whose shape is
// wrong, and any change or removal of a recorded one.
func TestRollupResolutionsAreGuardedByTheDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve-db")
	rollup, sc, account := unconfirmedRollup(t, e, b, "Resolve DB")
	id, _ := idgen.ParseAs(idgen.Rollup, rollup)
	invoice := e.fake.InvoiceUsage(b.Slug, account, e.clock.Now().AddDate(0, 0, -2), e.clock.Now().AddDate(0, 0, 2))
	isCheck := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23514"
	}
	e.closingAt(sc, "finance_pending", "2026-10-19")
	// not_billed before the finance step is done.
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollup_resolutions (brand_id, tenant_id, rollup_id, decision, unbilled_quantity_micro, resolved_by, note) VALUES ($1, $2, $3, 'not_billed', 2000000, 'sub:op', 'n')`, sc.Brand, sc.Tenant, id); !isCheck(err) {
		t.Fatalf("not_billed before the finance step = %v, want a check violation", err)
	}
	// A shape that is neither: forwarded with no invoice, not_billed with an invoice.
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollup_resolutions (brand_id, tenant_id, rollup_id, decision, resolved_by, note) VALUES ($1, $2, $3, 'forwarded', 'sub:op', 'n')`, sc.Brand, sc.Tenant, id); !isCheck(err) {
		t.Fatalf("forwarded without an invoice = %v, want a check violation", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollup_resolutions (brand_id, tenant_id, rollup_id, decision, invoice_id, unbilled_quantity_micro, resolved_by, note) VALUES ($1, $2, $3, 'forwarded', $4::uuid, 5, 'sub:op', 'n')`, sc.Brand, sc.Tenant, id, invoice); !isCheck(err) {
		t.Fatalf("forwarded with a quantity = %v, want a check violation", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollup_resolutions (brand_id, tenant_id, rollup_id, decision, invoice_id, resolved_by, note) VALUES ($1, $2, $3, 'forwarded', $4::uuid, 'sub:op', 'n')`, sc.Brand, sc.Tenant, id, invoice); err != nil {
		t.Fatalf("a resolution of an unconfirmed rollup = %v", err)
	}
	for _, stmt := range []string{`UPDATE usage_rollup_resolutions SET note = 'x'`, `DELETE FROM usage_rollup_resolutions`} {
		if _, err := e.db.Pool.Exec(ctx, stmt); !isCheck(err) {
			t.Errorf("%s = %v, want a check violation", stmt, err)
		}
	}
	// A rollup that is not unconfirmed.
	other := e.tenant(b, "Plain DB")
	e.subscribe(b, other, enginetest.PlanMetered)
	e.fake.SetDown(false, true)
	if _, _, err := e.record(other.Scope(), metering.Event{Meter: "api-calls", EventID: "q-1", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Forward(ctx); err != nil {
		t.Fatal(err)
	}
	var plain idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM usage_rollups WHERE tenant_id = $1`, other.ID).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollup_resolutions (brand_id, tenant_id, rollup_id, decision, invoice_id, resolved_by, note) VALUES ($1, $2, $3, 'forwarded', $4::uuid, 'sub:op', 'n')`, other.BrandID, other.ID, plain, invoice); !isCheck(err) {
		t.Fatalf("a resolution of a rollup that is not unconfirmed = %v, want a check violation", err)
	}
}

// The closure does not stall on usage Kill Bill may or may not hold. A send whose
// answer was lost during the drain does not hold the drain, so the closure
// cancels, finances and blocks the account, and then waits at completion for
// staff, who confirm the usage with the invoice Kill Bill made, after which it
// completes and the tenant is closed.
func TestClosureCompletesAfterStaffConfirmUnconfirmedUsage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-closure-lost")
	rollup, sc, account := unconfirmedRollup(t, e, b, "Closure Lost")
	if _, err := tenant.StartClosure(ctx, e.db.Pool, sc, tenant.SystemActor, tenant.ClosureRequest{Reason: "left", RequestID: "closure-lost-0001"}, "c"); err != nil {
		t.Fatal(err)
	}
	steps := tenant.EngineClosureSteps{Engines: e.eng, Drain: e.svc, Today: func() string { return enginetest.CancelDay }}
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}
	state := func() (status, settlement string, blocked bool) {
		if err := e.db.Pool.QueryRow(ctx, `SELECT t.status, s.status, s.blocked_at IS NOT NULL FROM tenants t JOIN tenant_closure_settlements s ON s.brand_id = t.brand_id AND s.tenant_id = t.id WHERE t.id = $1`, sc.Tenant).Scan(&status, &settlement, &blocked); err != nil {
			t.Fatal(err)
		}
		return
	}
	pass := func() {
		t.Helper()
		if _, err := sweeper.SweepOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET next_attempt_at = now() - interval '1 second' WHERE tenant_id = $1 AND status <> 'completed'`, sc.Tenant); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 6; i++ {
		pass()
		if _, settlement, blocked := state(); settlement == "blocking" && blocked {
			break
		}
	}
	status, settlement, blocked := state()
	if status != "closing" || settlement != "blocking" || !blocked {
		t.Fatalf("tenant %q settlement %q blocked %v, want closing at blocking with the account blocked, waiting for staff", status, settlement, blocked)
	}
	pass()
	if status, _, _ := state(); status != "closing" {
		t.Fatalf("the closure completed with usage nobody confirmed: tenant %q", status)
	}
	e.fake.InvoiceUsage(b.Slug, account, e.clock.Now().AddDate(0, 0, -2), e.clock.Now().AddDate(0, 0, 2))
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, metering.ResolveDecisionForwarded); err != nil {
		t.Fatalf("resolve = %v", err)
	}
	pass()
	if status, settlement, _ := state(); status != "closed" || settlement != "completed" {
		t.Fatalf("after the confirmation tenant %q settlement %q, want closed and completed", status, settlement)
	}
}

// forceReason gives a rollup an unconfirmed reason the guard of the table would
// only give it in the case that makes it true, by turning the guard off for the
// one statement, as a fixture.
func (e *env) forceReason(sc scope.Scope, reason string) {
	e.t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		`ALTER TABLE usage_rollups DISABLE TRIGGER usage_rollups_possible_send_immutable`,
		`UPDATE usage_rollups SET closure_unresolved_reason = '` + reason + `', last_error = '` + reason + `' WHERE tenant_id = '` + sc.Tenant.String() + `'`,
		`ALTER TABLE usage_rollups ENABLE ALWAYS TRIGGER usage_rollups_possible_send_immutable`,
	} {
		if _, err := e.db.Pool.Exec(ctx, stmt); err != nil {
			e.t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// A send that started and is marked as unconfirmed because of a mixed cutoff is
// handled like the other unconfirmed sends: it does not hold the drain, it blocks
// completion, and staff can resolve it. No pending started rollup is left
// without a way out.
func TestMixedCutoffUnconfirmedSendHasAResolutionPath(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve-mixed")
	rollup, sc, account := unconfirmedRollup(t, e, b, "Mixed Co")
	e.forceReason(sc, "mixed_cutoff_send_unconfirmed")
	e.closing(tenant.Tenant{BrandID: sc.Brand, ID: sc.Tenant})
	if done, err := e.svc.Drained(ctx, sc); err != nil || !done {
		t.Fatalf("drained with a mixed cutoff send = %v %v, want true", done, err)
	}
	if blocked := count(t, e.db, `SELECT count(*) FROM (SELECT tenant_closure_review_blocker($1, $2) AS b) q WHERE b IS NOT NULL`, sc.Brand, sc.Tenant); blocked != 1 {
		t.Fatal("completion is not blocked by a mixed cutoff send nobody resolved")
	}
	e.fake.InvoiceUsage(b.Slug, account, e.clock.Now().AddDate(0, 0, -2), e.clock.Now().AddDate(0, 0, 2))
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup, metering.ResolveDecisionForwarded); err != nil {
		t.Fatalf("resolve = %v", err)
	}
	if blocked := count(t, e.db, `SELECT count(*) FROM (SELECT tenant_closure_review_blocker($1, $2) AS b) q WHERE b IS NOT NULL`, sc.Brand, sc.Tenant); blocked != 0 {
		t.Fatal("completion is still blocked after the resolution")
	}
}
