//go:build integration

package billing_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func (e *env) creds() killbill.TenantCredentials {
	return killbill.TenantCredentials{APIKey: "zs-acme", APISecret: "kb-secret-zs-acme"}
}

// charge writes an external charge behind Core's back, as usage billed in
// Kill Bill would, and returns its item.
func (e *env) charge(accountID, amount, description string) killbill.InvoiceItem {
	e.t.Helper()
	item, err := e.fake.KillBill(e.t).CreateExternalCharge(context.Background(), e.creds(), killbill.CallContext{CreatedBy: "test"},
		killbill.InvoiceItem{AccountID: accountID, Amount: json.Number(amount), Currency: "CAD", Description: description})
	if err != nil {
		e.t.Fatal(err)
	}
	return item
}

// grant writes a staff credit of amount minor units that expires in d.
func (e *env) grant(sc scope.Scope, amount int64, d time.Duration, key string) billing.Credit {
	e.t.Helper()
	req := billing.StaffCredit{AmountMinor: amount, Currency: "CAD", IdempotencyKey: key}
	if d > 0 {
		x := e.clock.Now().Add(d)
		req.ExpiresAt = &x
	}
	c, _, err := e.svc.GrantCredit(context.Background(), sc, e.staff("test"), req)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) audits(sc scope.Scope, action string) []audit.Event {
	e.t.Helper()
	page, err := audit.Query(context.Background(), e.db.Pool, sc, audit.Filter{Action: action, Limit: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	return page.Events
}

func (e *env) reversal(credit billing.Credit) (int64, *string, *time.Time, int) {
	e.t.Helper()
	id, err := idgen.ParseAs(idgen.Credit, credit.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	var reversed int64
	var charge *string
	var expired *time.Time
	var attempts int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT reversed_minor, reversal_killbill_id::text, expired_at, attempts FROM staff_credits WHERE id = $1`, id).
		Scan(&reversed, &charge, &expired, &attempts); err != nil {
		e.t.Fatal(err)
	}
	return reversed, charge, expired, attempts
}

// A Core credit spent in full, then account credit from an overpayment:
// the overpayment is not Core's to reverse.
func TestSweeperNeverReversesCreditFromOtherSources(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.tenant("Tenant A"), e.tenant("Tenant B")
	spent := e.grant(a.Scope(), 1000, time.Hour, "grant-spent")
	e.charge(a.KillBillAccountID, "10.00", "usage")
	e.fake.AddKillBillCredit("zs-acme", a.KillBillAccountID, 500)
	partly := e.grant(b.Scope(), 1000, time.Hour, "grant-partly")
	e.charge(b.KillBillAccountID, "4.00", "usage")
	e.clock.advance(2 * time.Hour)

	r, err := (&billing.CreditSweeper{Service: e.svc}).SweepOnce(ctx)
	if err != nil || r.Expired != 2 || r.ReversedMinor != 600 || r.Errors != 0 {
		t.Fatalf("sweep = %+v %v", r, err)
	}
	if got := e.fake.Credit("zs-acme", a.KillBillAccountID); got != 500 {
		t.Fatalf("tenant A credit = %d, want the 500 of the overpayment", got)
	}
	if reversed, charge, expired, _ := e.reversal(spent); reversed != 0 || charge != nil || expired == nil {
		t.Fatalf("spent credit = %d %v %v", reversed, charge, expired)
	}
	if n := len(e.audits(a.Scope(), "billing.credit.expired_nothing_to_reverse")); n != 1 || len(e.audits(a.Scope(), "billing.credit.expired")) != 0 {
		t.Fatalf("tenant A audit = %d", n)
	}
	if got := e.fake.Credit("zs-acme", b.KillBillAccountID); got != 0 {
		t.Fatalf("tenant B credit = %d", got)
	}
	if reversed, charge, _, _ := e.reversal(partly); reversed != 600 || charge == nil {
		t.Fatalf("partly used credit = %d %v", reversed, charge)
	}
	if ev := e.audits(b.Scope(), "billing.credit.expired"); len(ev) != 1 || ev[0].Details["killbill_charge_id"] == nil {
		t.Fatalf("tenant B audit = %+v", ev)
	}
}

// A pending credit may already be in Kill Bill's account credit, so the
// tenant waits until it completes. Then the newer credit is left alone.
func TestSweeperWaitsForPendingCredits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.tenant("Tenant A")
	expiring := e.grant(a.Scope(), 1000, time.Hour, "grant-expiring")
	e.charge(a.KillBillAccountID, "4.00", "usage")
	e.clock.advance(time.Minute)
	e.fake.SetDown(false, true)
	newer := billing.StaffCredit{AmountMinor: 500, Currency: "CAD", IdempotencyKey: "grant-pending"}
	if _, _, err := e.svc.GrantCredit(ctx, a.Scope(), e.staff("test"), newer); codeOf(err) != "billing_unavailable" {
		t.Fatalf("grant with Kill Bill down: %v", err)
	}
	e.fake.SetDown(false, false)
	var pendingID idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM staff_credits WHERE status = 'pending'`).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	// Kill Bill wrote the pending credit; its answer was lost.
	if _, err := e.fake.KillBill(t).CreateCredit(ctx, e.creds(), killbill.CallContext{CreatedBy: "test"}, killbill.InvoiceItem{
		AccountID: a.KillBillAccountID, Amount: "5.00", Currency: "CAD", Description: "zs-core credit " + idgen.Format(idgen.Credit, pendingID)}); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(2 * time.Hour)
	sweeper := &billing.CreditSweeper{Service: e.svc}
	r, err := sweeper.SweepOnce(ctx)
	if err != nil || r.Expired != 0 || r.Deferred != 1 || r.Errors != 0 {
		t.Fatalf("sweep with a pending credit = %+v %v", r, err)
	}
	if got := e.fake.Credit("zs-acme", a.KillBillAccountID); got != 1100 {
		t.Fatalf("credit = %d, want 1100 untouched", got)
	}
	if _, _, expired, attempts := e.reversal(expiring); expired != nil || attempts != 0 {
		t.Fatalf("deferred credit = %v %d", expired, attempts)
	}
	if _, _, err := e.svc.GrantCredit(ctx, a.Scope(), e.staff("test"), newer); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r, err = sweeper.SweepOnce(ctx)
	if err != nil || r.Expired != 1 || r.ReversedMinor != 600 {
		t.Fatalf("sweep after completion = %+v %v", r, err)
	}
	if got := e.fake.Credit("zs-acme", a.KillBillAccountID); got != 500 {
		t.Fatalf("credit = %d, want the newer 500", got)
	}
}

// A charge an earlier pass wrote is recorded with its own amount and ID,
// and never written twice.
func TestSweeperRecordsAnEarlierChargeAsItIs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.tenant("Tenant A")
	c := e.grant(a.Scope(), 1000, time.Hour, "grant-retry")
	e.clock.advance(2 * time.Hour)
	earlier := e.charge(a.KillBillAccountID, "3.00", "zs-core credit expiry "+c.ID)
	r, err := (&billing.CreditSweeper{Service: e.svc}).SweepOnce(ctx)
	if err != nil || r.Expired != 1 || r.ReversedMinor != 300 {
		t.Fatalf("sweep = %+v %v", r, err)
	}
	if reversed, charge, _, _ := e.reversal(c); reversed != 300 || charge == nil || *charge != earlier.InvoiceItemID {
		t.Fatalf("recorded = %d %v, want 300 and %s", reversed, charge, earlier.InvoiceItemID)
	}
	invoices, err := e.fake.KillBill(t).AccountInvoices(ctx, e.creds(), a.KillBillAccountID, true)
	if err != nil {
		t.Fatal(err)
	}
	charges := 0
	for _, in := range invoices {
		for _, it := range in.Items {
			if it.ItemType == "EXTERNAL_CHARGE" && it.Description == "zs-core credit expiry "+c.ID {
				charges++
			}
		}
	}
	if charges != 1 || e.fake.Credit("zs-acme", a.KillBillAccountID) != 700 {
		t.Fatalf("charges = %d, credit %d", charges, e.fake.Credit("zs-acme", a.KillBillAccountID))
	}
}

// tenantIn creates a tenant of another brand.
func (e *env) tenantIn(b enginetest.Brand, name string) tenant.Tenant {
	e.t.Helper()
	owner := enginetest.InsertPerson(e.t, e.db.Pool, b, "owner-"+b.Slug+"@example.test")
	tn, err := tenant.CreateOrganization(context.Background(), e.db.Pool, e.eng, tenant.CreateRequest{
		Brand: b.ID, Creator: owner.Identity.ID, Name: name, IdempotencyKey: "key-" + idgen.MustNewV7().String(),
		BillingCountry: "CA", Actor: e.actor(), CorrelationID: "test",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tn
}

// A credit that fails backs off, 10 seconds then doubling. A credit of a
// brand that is not active waits without counting an attempt. Neither holds
// up the credits behind it.
func TestSweeperBacksOffAndPagesPastWaitingCredits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	other := e.fake.InsertBrand(t, e.db.Pool, "zs-other")
	x := e.tenantIn(other, "Tenant X")
	first := e.grant(x.Scope(), 100, time.Hour, "grant-other")
	e.clock.advance(time.Minute)
	a := e.tenant("Tenant A")
	second := e.grant(a.Scope(), 100, time.Hour, "grant-acme")
	e.clock.advance(2 * time.Hour)
	sweeper := &billing.CreditSweeper{Service: e.svc, Batch: 1}

	if _, err := e.db.Pool.Exec(ctx, `UPDATE brands SET status = 'disabled' WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	r, err := sweeper.SweepOnce(ctx)
	if err != nil || r.Deferred != 1 || r.Expired != 1 || r.Errors != 0 {
		t.Fatalf("sweep with an inactive brand = %+v %v", r, err)
	}
	if _, _, expired, attempts := e.reversal(first); expired != nil || attempts != 0 {
		t.Fatalf("credit of the inactive brand = %v %d", expired, attempts)
	}
	if _, _, expired, _ := e.reversal(second); expired == nil {
		t.Fatal("the credit behind it was not expired")
	}

	if _, err := e.db.Pool.Exec(ctx, `UPDATE brands SET status = 'active' WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	nextAttempt := func() time.Duration {
		var next time.Time
		if err := e.db.Pool.QueryRow(ctx, `SELECT next_attempt_at FROM staff_credits WHERE idempotency_key = 'grant-other'`).Scan(&next); err != nil {
			t.Fatal(err)
		}
		return next.Sub(e.clock.Now())
	}
	e.fake.SetDown(false, true)
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Errors != 1 {
		t.Fatalf("sweep with Kill Bill down = %+v %v", r, err)
	}
	if _, _, _, attempts := e.reversal(first); attempts != 1 || nextAttempt() != 10*time.Second {
		t.Fatalf("after one failure: attempts %d, next in %s", attempts, nextAttempt())
	}
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Errors != 0 || r.Expired != 0 {
		t.Fatalf("sweep before the credit is due = %+v %v", r, err)
	}
	e.clock.advance(10 * time.Second)
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Errors != 1 {
		t.Fatalf("second failure = %+v %v", r, err)
	}
	if _, _, _, attempts := e.reversal(first); attempts != 2 || nextAttempt() != 20*time.Second {
		t.Fatalf("after two failures: attempts %d, next in %s", attempts, nextAttempt())
	}
	e.fake.SetDown(false, false)
	e.clock.advance(20 * time.Second)
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Expired != 1 || r.ReversedMinor != 100 {
		t.Fatalf("sweep once due = %+v %v", r, err)
	}
}

// Receipts of push notifications older than seven days are deleted, so
// Kill Bill's retry window bounds the table.
func TestSweeperDeletesOldNotificationReceipts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	body := []byte(`{"eventType":"TENANT_CONFIG_CHANGE","accountId":null,"objectType":"TENANT_KVS","objectId":"x","metaData":"{}"}`)
	if r, err := e.svc.HandleNotification(ctx, e.brand.ID, body); err != nil || !r.Ignored {
		t.Fatalf("notification = %+v %v", r, err)
	}
	sweeper := &billing.CreditSweeper{Service: e.svc}
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.NotificationsDeleted != 0 {
		t.Fatalf("fresh receipt deleted = %+v %v", r, err)
	}
	e.clock.advance(billing.NotificationRetention + time.Minute)
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.NotificationsDeleted != 1 {
		t.Fatalf("old receipt = %+v %v", r, err)
	}
}
