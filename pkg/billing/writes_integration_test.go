//go:build integration

package billing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// usePlugin makes the brand's payment plugin the fake's hosted plugin,
// which takes more than one payment method per account.
func (e *env) usePlugin() {
	e.t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE brands SET manifest = jsonb_set(manifest, '{billing,payment_plugin}',
    '{"name": "hosted-test", "mode": "test", "config_ref": "env://BRAND_ZS_ACME__PLUGIN"}'::jsonb) WHERE id = $1`, e.brand.ID); err != nil {
		e.t.Fatal(err)
	}
}

// Kill Bill sends the same body again when an object changes and changes
// back. The state Core reads from Kill Bill tells a change from a retry.
func TestNotificationDedupFollowsKillBillState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.tenant("Tenant A")
	overdue, _ := json.Marshal(map[string]any{"eventType": "OVERDUE_CHANGE", "accountId": a.KillBillAccountID, "objectType": "ACCOUNT",
		"objectId": a.KillBillAccountID, "metaData": nil})
	handle := func(body []byte) billing.NotificationResult {
		t.Helper()
		r, err := e.svc.HandleNotification(ctx, e.brand.ID, body)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for i, step := range []struct {
		state     string
		duplicate bool
	}{
		{"WARNING", false}, {"WARNING", true}, {"__KILLBILL__CLEAR__OVERDUE_STATE__", false},
		{"WARNING", false}, {"WARNING", true},
	} {
		e.fake.SetOverdue("zs-acme", a.KillBillAccountID, killbill.OverdueState{Name: step.state, IsClearState: strings.HasPrefix(step.state, "__")})
		if r := handle(overdue); r.Duplicate != step.duplicate || (r.Event == "") != step.duplicate {
			t.Fatalf("step %d, overdue %s = %+v", i, step.state, r)
		}
	}

	sub, _, err := e.svc.CreateSubscription(ctx, a.Scope(), e.call(), enginetest.PlanUsage, "sub-dedup")
	if err != nil {
		t.Fatal(err)
	}
	change, _ := json.Marshal(map[string]any{"eventType": "SUBSCRIPTION_CHANGE", "accountId": a.KillBillAccountID, "objectType": "SUBSCRIPTION",
		"objectId": sub.ID, "metaData": nil})
	if r := handle(change); r.Event != billing.EventSubscriptionChanged {
		t.Fatalf("first change = %+v", r)
	}
	if r := handle(change); !r.Duplicate {
		t.Fatalf("retry = %+v", r)
	}
	if _, err := e.svc.ChangePlan(ctx, a.Scope(), e.call(), sub.ID, enginetest.PlanBasic, billing.PolicyImmediate); err != nil {
		t.Fatal(err)
	}
	if r := handle(change); r.Event != billing.EventSubscriptionChanged {
		t.Fatalf("second change with the same body = %+v", r)
	}
	var n int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type IN ('billing.overdue.changed', 'billing.subscription.changed')`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("events = %d %v, want 5", n, err)
	}
}

// Every Kill Bill write Core makes is audited once, with Kill Bill IDs and
// amounts and nothing the plugin was given.
func TestEveryBillingWriteIsAudited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.usePlugin()
	a := e.tenant("Tenant A")
	sc := a.Scope()
	const secret = "pm_not_for_audit"

	pmA, _, err := e.svc.AddPaymentMethod(ctx, sc, e.call(), billing.NewPaymentMethod{IdempotencyKey: "pm-key-a", ExternalPaymentMethodID: secret,
		PluginProperties: []billing.Property{{Key: "token", Value: secret}}})
	if err != nil || pmA.PaymentMethod == nil {
		t.Fatalf("add = %+v %v", pmA, err)
	}
	if _, replayed, err := e.svc.AddPaymentMethod(ctx, sc, e.call(), billing.NewPaymentMethod{IdempotencyKey: "pm-key-a", ExternalPaymentMethodID: secret,
		PluginProperties: []billing.Property{{Key: "token", Value: secret}}}); err != nil || !replayed {
		t.Fatalf("replay = %v %v", replayed, err)
	}
	pmB, _, err := e.svc.AddPaymentMethod(ctx, sc, e.call(), billing.NewPaymentMethod{IdempotencyKey: "pm-key-b", ExternalPaymentMethodID: secret})
	if err != nil || pmB.PaymentMethod == nil {
		t.Fatalf("second = %+v %v", pmB, err)
	}
	if _, err := e.svc.SetDefaultPaymentMethod(ctx, sc, e.call(), pmB.PaymentMethod.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePaymentMethod(ctx, sc, e.call(), pmA.PaymentMethod.ID); err != nil {
		t.Fatal(err)
	}
	sub, _, err := e.svc.CreateSubscription(ctx, sc, e.call(), enginetest.PlanUsage, "sub-audit")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.CreateSubscription(ctx, sc, e.call(), enginetest.PlanUsage, "sub-audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ChangePlan(ctx, sc, e.call(), sub.ID, enginetest.PlanBasic, billing.PolicyImmediate); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelSubscription(ctx, sc, e.call(), sub.ID, billing.PolicyEndOfTerm); err != nil {
		t.Fatal(err)
	}
	pay := e.fake.AddPurchase("zs-acme", a.KillBillAccountID, 2000, "CAD")
	if _, _, err := e.svc.Refund(ctx, sc, e.staff("damaged"), pay, 500, "refund-audit"); err != nil {
		t.Fatal(err)
	}
	m, err := tenant.MembershipOf(ctx, e.db.Pool, sc, e.owner.Identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := e.svc.IssueCredential(ctx, sc, e.call(), billing.Holder{MembershipID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Proxy(ctx, billing.ProxyRequest{Token: cred.Token, CorrelationID: "corr-proxy", Tenant: a.ID, Method: http.MethodPost,
		Path: "1.0/kb/subscriptions", ContentType: "application/json",
		Body: []byte(`{"accountId":"` + a.KillBillAccountID + `","planName":"` + enginetest.PlanBasic + `"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Proxy(ctx, billing.ProxyRequest{Token: cred.Token, Tenant: a.ID, Method: http.MethodGet, Path: "1.0/kb/accounts/" + a.KillBillAccountID}); err != nil {
		t.Fatal(err)
	}

	for action, want := range map[string]int{
		"billing.payment_method.added": 2, "billing.payment_method.default_set": 1, "billing.payment_method.deleted": 1,
		"billing.subscription.created": 1, "billing.subscription.plan_changed": 1, "billing.subscription.cancelled": 1,
		"billing.payment.refunded": 1, "billing.credential.issued": 1, "billing.proxy.write": 1,
	} {
		got := e.audits(sc, action)
		if len(got) != want {
			t.Errorf("%s: %d audit events, want %d", action, len(got), want)
			continue
		}
		for _, ev := range got {
			details, _ := json.Marshal(ev.Details)
			if strings.Contains(string(details), secret) || ev.Details["killbill_account_id"] != a.KillBillAccountID {
				t.Errorf("%s details = %s", action, details)
			}
		}
	}
	refund := e.audits(sc, "billing.payment.refunded")[0]
	if refund.Details["killbill_payment_id"] != pay || refund.Details["amount_minor"] != float64(500) || refund.Details["killbill_transaction_id"] == nil || refund.Reason != "damaged" {
		t.Fatalf("refund audit = %+v", refund)
	}
	if proxy := e.audits(sc, "billing.proxy.write")[0]; proxy.CorrelationID != "corr-proxy" || proxy.Details["killbill_object_id"] == nil || proxy.Actor.Type != events.ActorUser {
		t.Fatalf("proxy audit = %+v", proxy)
	}
}

// A refund larger than what the payment captured and has not refunded is
// refused by Core before Kill Bill sees it.
func TestRefundCannotExceedThePayment(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.tenant("Tenant A")
	pay := e.fake.AddPurchase("zs-acme", a.KillBillAccountID, 2000, "CAD")
	if _, _, err := e.svc.Refund(ctx, a.Scope(), e.staff("x"), pay, 1500, "refund-1"); err != nil {
		t.Fatal(err)
	}
	refunds := func() int {
		n := 0
		for _, w := range e.fake.Writes() {
			if strings.HasSuffix(w.Path, "/refunds") {
				n++
			}
		}
		return n
	}
	_, _, err := e.svc.Refund(ctx, a.Scope(), e.staff("x"), pay, 501, "refund-2")
	if codeOf(err) != "billing_rejected" || !strings.Contains(err.Error(), "refund_exceeds_payment") || refunds() != 1 {
		t.Fatalf("refund past the payment = %v, Kill Bill refund calls %d", err, refunds())
	}
	if p, _, err := e.svc.Refund(ctx, a.Scope(), e.staff("x"), pay, 500, "refund-3"); err != nil || p.RefundedMinor != 2000 {
		t.Fatalf("refund of the rest = %+v %v", p, err)
	}
	if p, replayed, err := e.svc.Refund(ctx, a.Scope(), e.staff("x"), pay, 500, "refund-3"); err != nil || !replayed || p.RefundedMinor != 2000 {
		t.Fatalf("replay = %+v %v %v", p, replayed, err)
	}
}

// A subscription through the proxy may name one of the tenant's own bundles
// only: Kill Bill adds it to the bundle the key names.
func TestProxyRefusesAnotherAccountsBundle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, b := e.tenant("Tenant A"), e.tenant("Tenant B")
	own, _, err := e.svc.CreateSubscription(ctx, a.Scope(), e.call(), enginetest.PlanUsage, "sub-key-a")
	if err != nil {
		t.Fatal(err)
	}
	theirs, _, err := e.svc.CreateSubscription(ctx, b.Scope(), e.call(), enginetest.PlanUsage, "sub-key-b")
	if err != nil {
		t.Fatal(err)
	}
	m, err := tenant.MembershipOf(ctx, e.db.Pool, a.Scope(), e.owner.Identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := e.svc.IssueCredential(ctx, a.Scope(), e.call(), billing.Holder{MembershipID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	post := func(bundle string) error {
		body := `{"accountId":"` + a.KillBillAccountID + `","planName":"` + enginetest.PlanBasic + `"` + bundle + `}`
		_, err := e.svc.Proxy(ctx, billing.ProxyRequest{Token: cred.Token, Tenant: a.ID, Method: http.MethodPost, Path: "1.0/kb/subscriptions",
			ContentType: "application/json", Body: []byte(body)})
		return err
	}
	before := e.fake.Subscriptions("zs-acme")
	for _, bundle := range []string{`,"bundleExternalKey":"` + theirs.BundleID + `"`, `,"bundleExternalKey":"no-such-bundle"`, `,"bundleExternalKey":7`, `,"bundleExternalKey":""`} {
		if err := post(bundle); codeOf(err) != "not_found" {
			t.Errorf("%s: %v", bundle, err)
		}
	}
	if e.fake.Subscriptions("zs-acme") != before {
		t.Fatal("a refused request reached Kill Bill")
	}
	for _, bundle := range []string{`,"bundleExternalKey":"` + own.BundleID + `"`, `,"bundleExternalKey":null`, ``} {
		if err := post(bundle); err != nil {
			t.Errorf("%s: %v", bundle, err)
		}
	}
}

// A redemption resumed after Kill Bill was unreachable keeps the expiry of
// its first attempt.
func TestResumedRedemptionKeepsItsExpiry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.code(billing.NewCreditCode{Code: "LATER-ON", AmountMinor: 100, Currency: "CAD", CreditValidDays: intp(30), Reason: "x", CreatedBy: "ops"})
	a := e.tenant("Tenant A")
	first := e.clock.Now()
	e.fake.SetDown(false, true)
	if _, _, err := e.svc.Redeem(ctx, a.Scope(), e.call(), "LATER-ON"); codeOf(err) != "billing_unavailable" {
		t.Fatalf("Kill Bill down: %v", err)
	}
	e.fake.SetDown(false, false)
	e.clock.advance(3 * 24 * time.Hour)
	c, created, err := e.svc.Redeem(ctx, a.Scope(), e.call(), "LATER-ON")
	if err != nil || !created || c.ExpiresAt == nil || !c.ExpiresAt.Equal(first.AddDate(0, 0, 30)) {
		t.Fatalf("resumed = %+v %v %v, want expiry %s", c, created, err, first.AddDate(0, 0, 30))
	}
}

// A redemption names a code of its own brand: the database refuses another.
func TestRedemptionCodeBelongsToTheBrand(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.tenant("Tenant A")
	other := e.fake.InsertBrand(t, e.db.Pool, "zs-other")
	var code billing.CreditCode
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		var err error
		code, err = billing.AddCreditCode(ctx, tx, other.ID, billing.NewCreditCode{Code: "ELSEWHERE", AmountMinor: 1, Currency: "CAD", Reason: "x", CreatedBy: "ops"}, operator(), "t")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	codeID, _ := idgen.ParseAs(idgen.CreditCode, code.ID)
	_, err := e.db.Pool.Exec(ctx, `INSERT INTO credit_code_redemptions (id, brand_id, tenant_id, credit_code_id, amount_minor, currency, actor_type, correlation_id)
VALUES ($1, $2, $3, $4, 1, 'CAD', 'system', 't')`, idgen.MustNewV7(), e.brand.ID, a.ID, codeID)
	if err == nil || !strings.Contains(err.Error(), "credit_code_redemptions_brand_id_credit_code_id_fkey") {
		t.Fatalf("redemption of another brand's code: %v", err)
	}
	_, err = e.db.Pool.Exec(ctx, `INSERT INTO killbill_notifications (brand_id, dedupe_key, event_type, tenant_id, object_key) VALUES ($1, 'k', 'X', $2, 'x:y')`, other.ID, a.ID)
	if err == nil || !strings.Contains(err.Error(), "killbill_notifications_brand_id_tenant_id_fkey") {
		t.Fatalf("receipt for another brand's tenant: %v", err)
	}
}
