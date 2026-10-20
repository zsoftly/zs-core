//go:build integration

package stacktest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// The plans of brands/zs-reference/catalogue.xml.
const (
	refPlanUsageFree = "core-basic-monthly"
	refPlanUsage     = "core-test-monthly"
)

type stackClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *stackClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().UTC().Add(c.offset)
}

func (c *stackClock) advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

// kbRaw calls Kill Bill as the stack's admin with a tenant's credentials,
// for what Core never does itself, such as a payment taken outside Core.
func kbRaw(t *testing.T, creds killbill.TenantCredentials, method, path string, body any) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, os.Getenv("ZS_TEST_KILLBILL_URL")+path, reader)
	req.SetBasicAuth("admin", os.Getenv("ZS_TEST_KILLBILL_ADMIN_PASSWORD"))
	creds.Apply(req.Header)
	req.Header.Set("X-Killbill-CreatedBy", "stack-test")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

// discoveredIssuer reads a realm's issuer from Keycloak discovery, so the
// test also runs where Keycloak is reached by another name than its
// configured hostname.
func discoveredIssuer(t *testing.T, kcURL, realm string) string {
	t.Helper()
	resp, err := http.Get(strings.TrimRight(kcURL, "/") + "/realms/" + realm + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || d.Issuer == "" {
		t.Fatalf("discovery of %s: %v", realm, err)
	}
	return d.Issuer
}

// The Phase 4a billing surface on Kill Bill 0.24.22. With
// ZS_TEST_CALLBACK_ADDR set to an address Kill Bill can reach, the test
// serves Core there and checks that Kill Bill's push notifications arrive;
// otherwise that part is skipped. See docs/development.md.
func TestBillingOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()
	clock := &stackClock{}
	secret := []byte("stack-test-webhook-secret-" + strings.Repeat("x", 16))
	svc := &billing.Service{DB: db.Pool, KillBill: s.kb, Secrets: secrets.Default{}, WebhookSecret: secret, Now: clock.Now}
	engines := tenant.Engines{Keycloak: s.kc, KillBill: s.kb, Secrets: secrets.Default{}}
	reg := brand.NewRegistry(db.Pool, time.Second, nil)
	handler := httpapi.New(httpapi.Options{DB: db.Pool, Migrations: migrations.FS, Brands: reg, Tenants: engines, Billing: svc,
		Verifier: &identity.Verifier{Brands: reg, Keys: keycloak.NewKeySet(s.kcURL, keycloak.KeySetOptions{})}})
	srv := httptest.NewUnstartedServer(handler)
	callbackAddr := os.Getenv("ZS_TEST_CALLBACK_ADDR")
	if callbackAddr != "" {
		l, err := net.Listen("tcp", callbackAddr)
		if err != nil {
			t.Fatal(err)
		}
		srv.Listener = l
		deps.WebhookBaseURL, deps.WebhookSecret = "http://"+callbackAddr, secret
	}
	srv.Start()
	t.Cleanup(srv.Close)
	api := apiServer{srv}

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	refDiff := apply(t, deps, devVariant(t, ref))
	refID, _ := idgen.ParseAs(idgen.Brand, refDiff.BrandID)
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), s.deps(t, db), ref, cli) })
	if callbackAddr != "" {
		registered := false
		for _, c := range refDiff.Changes {
			registered = registered || c.Target == brand.TargetNotificationCallback
		}
		cbs, err := s.kb.NotificationCallbacks(ctx, killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)})
		if err != nil || len(cbs) != 1 || !strings.HasSuffix(cbs[0], brand.WebhookPath(secret, refID)) {
			t.Fatalf("callback = %v %v (registered in diff: %v)", cbs, err, registered)
		}
	}

	// Stripe 8.0.3 on a second brand, with a dummy test-mode key, through
	// KPM. A live key and a live mode are refused before anything changes.
	second := secondManifest(t, ref)
	t.Setenv("BRAND_ZS_SECOND__PLUGIN_LIVE", "org.killbill.billing.plugin.stripe.apiKey=sk_live_not_a_real_key")
	live := clone(t, second)
	live.Billing.PaymentPlugin.ConfigRef = "env://BRAND_ZS_SECOND__PLUGIN_LIVE"
	if _, err := brand.Apply(ctx, deps, live, cli); errName(err) != "brand_apply_rejected" {
		t.Fatalf("live Stripe key: %v", err)
	}
	// A configuration without the key, a text a reader other than Java's
	// would misread, and a configuration key of another plugin are refused
	// too.
	for name, config := range map[string]string{
		"no key":          "org.killbill.billing.plugin.stripe.chargeDescription=x",
		"unicode escape":  secondPluginConfig + "\norg.killbill.billing.plugin.stripe.api\\u004bey=sk_live_not_a_real_key",
		"carriage return": secondPluginConfig + "\rorg.killbill.billing.plugin.stripe.apiKey=sk_live_not_a_real_key",
		"adyen live":      secondPluginConfig + "\norg.killbill.billing.plugin.adyen.enviroment=LIVE",
	} {
		t.Setenv("BRAND_ZS_SECOND__PLUGIN_BAD", config)
		bad := clone(t, second)
		bad.Billing.PaymentPlugin.ConfigRef = "env://BRAND_ZS_SECOND__PLUGIN_BAD"
		if _, err := brand.Apply(ctx, deps, bad, cli); errName(err) != "brand_apply_rejected" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	swapped := clone(t, second)
	swapped.Billing.PaymentPlugin.ConfigKey = "killbill-adyen"
	if _, err := brand.Apply(ctx, deps, swapped, cli); errName(err) != "brand_manifest_invalid" {
		t.Fatalf("configuration key of another plugin: %v", err)
	}
	liveMode := clone(t, second)
	liveMode.Billing.PaymentPlugin.Mode = brand.PaymentModeLive
	if _, err := brand.Apply(ctx, deps, liveMode, cli); errName(err) != "brand_manifest_invalid" {
		t.Fatalf("live mode: %v", err)
	}
	paypal := clone(t, second)
	paypal.Billing.PaymentPlugin.Name = "paypal-express"
	if _, err := brand.Apply(ctx, deps, paypal, cli); errName(err) != "plugin_unavailable" {
		t.Fatalf("paypal-express: %v", err)
	}
	stripeRunning := func(nodes []killbill.NodeInfo) bool {
		for _, n := range nodes {
			found := false
			for _, p := range n.PluginsInfo {
				found = found || (p.PluginKey == "stripe" && p.Version == "8.0.3" && p.State == "RUNNING")
			}
			if !found {
				return false
			}
		}
		return len(nodes) > 0
	}
	before, err := s.kb.NodesInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wasRunning := stripeRunning(before)
	secondDiff := apply(t, deps, second)
	secondID, _ := idgen.ParseAs(idgen.Brand, secondDiff.BrandID)
	pluginChanged := false
	for _, c := range secondDiff.Changes {
		pluginChanged = pluginChanged || c.Target == brand.TargetPlugin
	}
	if pluginChanged == wasRunning {
		t.Fatalf("plugin change %v while Stripe was running before %v: %s", pluginChanged, wasRunning, mustJSON(secondDiff))
	}
	nodes, err := s.kb.NodesInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		found := false
		for _, p := range n.PluginsInfo {
			if p.PluginKey == "stripe" && p.Version == "8.0.3" && p.State == "RUNNING" {
				found = true
			}
		}
		if !found {
			t.Fatalf("Stripe 8.0.3 is not RUNNING on node %s: %+v", n.NodeName, n.PluginsInfo)
		}
	}
	if !wasRunning {
		var installs int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM killbill_plugin_installs WHERE plugin = 'stripe' AND version = '8.0.3' AND started_at IS NOT NULL`).Scan(&installs); err != nil || installs != len(nodes) {
			t.Fatalf("install records = %d %v", installs, err)
		}
	}
	if cfg, found, err := s.kb.PluginConfig(ctx, killbill.TenantCredentials{APIKey: "zs-second", APISecret: secondKillBillSecret}, "killbill-stripe"); err != nil || !found || !strings.Contains(cfg, "sk_test_") {
		t.Fatalf("zs-second Stripe configuration is not the test one: %v", err)
	}
	if again := apply(t, deps, second); len(again.Changes) != 0 {
		t.Fatalf("second apply of zs-second = %s", mustJSON(again))
	}

	// Tenants of the reference brand, created through Core.
	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	owner := "billing-owner-" + suffix
	createUser(t, s.kc, "zs-reference", owner, "pw-"+suffix)
	tok := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", owner, "pw-"+suffix)
	if status, out := api.call(t, http.MethodGet, "/v1/me", tok.access, nil, ""); status != http.StatusOK {
		t.Fatalf("/v1/me = %d %s", status, mustJSON(out))
	}
	newTenant := func(name string) tenant.Tenant {
		t.Helper()
		status, out := api.call(t, http.MethodPost, "/v1/tenants", tok.access, jsonBody(t, map[string]any{"idempotency_key": "billing-" + strings.ReplaceAll(name, " ", "-") + "-" + suffix, "name": name + " " + suffix}), "application/json")
		if status != http.StatusCreated {
			t.Fatalf("create tenant = %d %s", status, mustJSON(out))
		}
		id, _ := idgen.ParseAs(idgen.Tenant, out["id"].(string))
		tn, err := tenant.Get(ctx, db.Pool, scope.Scope{Brand: refID, Tenant: id})
		if err != nil {
			t.Fatal(err)
		}
		return tn
	}
	a, b := newTenant("Billing A"), newTenant("Billing B")
	refIssuer := discoveredIssuer(t, s.kcURL, "zs-reference")
	ident, err := identity.Get(ctx, db.Pool, refID, refIssuer, tok.subject)
	if err != nil {
		t.Fatal(err)
	}
	call := billing.Call{Actor: events.Actor{Type: events.ActorUser, ID: ident.PublicID()}, CorrelationID: "stack-" + suffix}
	staff := billing.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:stack-staff"}, Reason: "stack test", CorrelationID: "stack-staff-" + suffix}
	refCreds := killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)}

	// The catalogue in force has both plans; the usage-free plan has a
	// trial and no usage.
	cat, err := svc.Catalogue(ctx, refID)
	if err != nil {
		t.Fatal(err)
	}
	var usageFree *billing.Plan
	for _, p := range cat.Products {
		for i := range p.Plans {
			if p.Plans[i].Name == refPlanUsageFree {
				usageFree = &p.Plans[i]
			}
		}
	}
	if usageFree == nil || usageFree.Phases[0].Type != "TRIAL" || len(usageFree.Phases[1].Usage) != 0 || usageFree.Phases[1].RecurringPrices[0].AmountMinor != 500 {
		t.Fatalf("catalogue = %s", mustJSON(cat))
	}

	// A subscription with a trial on the usage-free plan, and its invoice.
	sub, _, err := svc.CreateSubscription(ctx, a.Scope(), call, refPlanUsageFree, "stack-sub-"+suffix)
	if err != nil || sub.Phase != "TRIAL" || sub.State != "ACTIVE" {
		t.Fatalf("subscribe = %+v %v", sub, err)
	}
	if again, replayed, err := svc.CreateSubscription(ctx, a.Scope(), call, refPlanUsageFree, "stack-sub-"+suffix); err != nil || !replayed || again.ID != sub.ID {
		t.Fatalf("replay = %+v %v %v", again, replayed, err)
	}
	if _, _, err := svc.CreateSubscription(ctx, a.Scope(), call, "no-such-plan", "stack-sub-bad-"+suffix); errName(err) != "billing_rejected" && errName(err) != "billing_unavailable" {
		t.Fatalf("unknown plan: %v", err)
	}
	if e := svc.CheckEntitlement(ctx, a.Scope(), "core-basic"); !e.Allowed || e.Plan != refPlanUsageFree {
		t.Fatalf("entitlement = %+v", e)
	}
	if e := svc.CheckEntitlement(ctx, a.Scope(), "api-call"); e.Allowed || e.Reason != billing.ReasonNotInPlan {
		t.Fatalf("usage unit of another plan = %+v", e)
	}
	var invoices []billing.Invoice
	for i := 0; i < 20 && len(invoices) == 0; i++ {
		if invoices, _, err = svc.ListInvoices(ctx, a.Scope(), tenant.Page{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(invoices) == 0 {
		t.Fatal("no invoice after the subscription")
	}
	inv, err := svc.GetInvoice(ctx, a.Scope(), invoices[0].ID)
	if err != nil || len(inv.Items) == 0 || inv.Currency != "CAD" {
		t.Fatalf("invoice = %+v %v", inv, err)
	}
	if _, err := svc.GetInvoice(ctx, b.Scope(), invoices[0].ID); errName(err) != "not_found" {
		t.Fatalf("invoice from another tenant: %v", err)
	}
	if _, err := svc.InvoicePDF(ctx, a.Scope(), invoices[0].ID); errName(err) != "invoice_pdf_unavailable" {
		t.Fatalf("pdf: %v", err)
	}
	changed, err := svc.ChangePlan(ctx, a.Scope(), call, sub.ID, refPlanUsage, billing.PolicyEndOfTerm)
	if err != nil || changed.ID != sub.ID {
		t.Fatalf("change at end of term = %+v %v", changed, err)
	}

	// A code redeemed, visible on the account in Kill Bill, and the
	// overdue state read.
	code := "STACK-" + strings.ToUpper(suffix)
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, err := billing.AddCreditCode(ctx, tx, refID, billing.NewCreditCode{Code: code, AmountMinor: 1500, Currency: "CAD", MaxRedemptions: intPtr(1), Reason: "stack test", CreatedBy: "stack-test"},
			events.Actor{Type: events.ActorService, ID: "zs-core-cli:stack-test"}, "stack-test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	credit, created, err := svc.Redeem(ctx, a.Scope(), call, code)
	if err != nil || !created || credit.KillBillCreditID == "" {
		t.Fatalf("redeem = %+v %v %v", credit, created, err)
	}
	if again, created, err := svc.Redeem(ctx, a.Scope(), call, code); err != nil || created || again.ID != credit.ID {
		t.Fatalf("redeem again = %+v %v %v", again, created, err)
	}
	if _, _, err := svc.Redeem(ctx, b.Scope(), call, code); errName(err) != "credit_code_exhausted" {
		t.Fatalf("redeem past the cap: %v", err)
	}
	sum, err := svc.Summary(ctx, a.Scope())
	if err != nil || sum.CreditMinor != 1500 || sum.Currency != "CAD" || !sum.Overdue.Clear || sum.Overdue.State == "" || sum.OnHold || sum.PaymentPlugin != brand.ExternalPaymentPlugin {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	status, _, body := kbRaw(t, refCreds, http.MethodGet, "/1.0/kb/credits/"+credit.KillBillCreditID, nil)
	if status != http.StatusOK || !strings.Contains(string(body), "CREDIT_ADJ") {
		t.Fatalf("Kill Bill credit = %d %s", status, body)
	}

	// A staff credit with an expiry: the sweeper reverses the unused part
	// in Kill Bill.
	expires := clock.Now().Add(time.Hour)
	if _, _, err := svc.GrantCredit(ctx, b.Scope(), staff, billing.StaffCredit{AmountMinor: 1000, Currency: "CAD", ExpiresAt: &expires, IdempotencyKey: "stack-grant-" + suffix}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.kb.CreateExternalCharge(ctx, refCreds, killbill.CallContext{CreatedBy: "stack-test"}, killbill.InvoiceItem{AccountID: b.KillBillAccountID, Amount: "2.50", Currency: "CAD", Description: "usage outside Core"}); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Hour)
	report, err := (&billing.CreditSweeper{Service: svc}).SweepOnce(ctx)
	if err != nil || report.Expired < 1 || report.ReversedMinor != 750 || report.Errors != 0 {
		t.Fatalf("sweep = %+v %v", report, err)
	}
	if sum, err := svc.Summary(ctx, b.Scope()); err != nil || sum.CreditMinor != 0 {
		t.Fatalf("summary after expiry = %+v %v", sum, err)
	}

	// Payment methods on the external payment plugin, a payment taken
	// outside Core, and a staff refund.
	pm, _, err := svc.AddPaymentMethod(ctx, a.Scope(), call, billing.NewPaymentMethod{IdempotencyKey: "stack-pm-" + suffix, SetDefault: true})
	if err != nil || pm.PaymentMethod == nil || pm.PaymentMethod.PluginName != brand.ExternalPaymentPlugin {
		t.Fatalf("payment method = %+v %v", pm, err)
	}
	if hosted, _, err := svc.AddPaymentMethod(ctx, b.Scope(), call, billing.NewPaymentMethod{IdempotencyKey: "stack-hosted-" + suffix, HostedPage: true}); err != nil || hosted.Form == nil || hosted.PaymentMethod == nil {
		t.Fatalf("hosted page = %+v %v", hosted, err)
	}
	status, _, body = kbRaw(t, refCreds, http.MethodPost, "/1.0/kb/accounts/"+a.KillBillAccountID+"/payments",
		map[string]any{"transactionType": "PURCHASE", "amount": 5.00, "currency": "CAD", "transactionExternalKey": "stack-purchase-" + suffix})
	if status != http.StatusCreated {
		t.Fatalf("purchase = %d %s", status, body)
	}
	payments, _, err := svc.ListPayments(ctx, a.Scope(), tenant.Page{})
	if err != nil || len(payments) != 1 || payments[0].PurchasedMinor != 500 {
		t.Fatalf("payments = %+v %v", payments, err)
	}
	refunded, _, err := svc.Refund(ctx, a.Scope(), staff, payments[0].ID, 200, "stack-refund-"+suffix)
	if err != nil || refunded.RefundedMinor != 200 {
		t.Fatalf("refund = %+v %v", refunded, err)
	}
	if again, replayed, err := svc.Refund(ctx, a.Scope(), staff, payments[0].ID, 200, "stack-refund-"+suffix); err != nil || !replayed || again.RefundedMinor != 200 {
		t.Fatalf("refund replay = %+v %v %v", again, replayed, err)
	}
	if _, _, err := svc.Refund(ctx, a.Scope(), staff, payments[0].ID, 301, "stack-refund-over-"+suffix); errName(err) != "billing_rejected" || !strings.Contains(err.Error(), "refund_exceeds_payment") {
		t.Fatalf("refund past the payment: %v", err)
	}
	// Every write is audited with the Kill Bill IDs.
	for action, want := range map[string]int{"billing.subscription.created": 1, "billing.subscription.plan_changed": 1, "billing.payment_method.added": 1, "billing.payment.refunded": 1} {
		var n int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE brand_id = $1 AND tenant_id = $2 AND action = $3 AND details ->> 'killbill_account_id' = $4`,
			refID, a.ID, action, a.KillBillAccountID).Scan(&n); err != nil || n != want {
			t.Fatalf("%s audit events = %d %v, want %d", action, n, err, want)
		}
	}

	// Suspension blocks the account in Kill Bill: billing is on hold.
	operator := events.Actor{Type: events.ActorService, ID: "zs-core-cli:stack-test"}
	if _, err := tenant.Suspend(ctx, db.Pool, engines, a.Scope(), operator, "stack test", "stack-test"); err != nil {
		t.Fatal(err)
	}
	if e := svc.CheckEntitlement(ctx, a.Scope(), "core-basic"); e.Allowed || e.Reason != billing.ReasonWalletOnHold {
		t.Fatalf("entitlement while suspended = %+v", e)
	}
	if sum, err := svc.Summary(ctx, a.Scope()); err != nil || strings.Join(sum.HoldReasons, ",") != "tenant_suspended,account_blocked" {
		t.Fatalf("summary while suspended = %+v %v", sum, err)
	}
	if _, err := tenant.Reactivate(ctx, db.Pool, engines, a.Scope(), operator, "stack test", "stack-test"); err != nil {
		t.Fatal(err)
	}
	if e := svc.CheckEntitlement(ctx, a.Scope(), "core-basic"); !e.Allowed {
		t.Fatalf("entitlement after reactivation = %+v", e)
	}

	// The credentials proxy reaches the tenant's own account only.
	status, out := api.call(t, http.MethodGet, "/v1/tenants/"+a.PublicID()+"/billing/credentials", tok.access, nil, "")
	if status != http.StatusOK {
		t.Fatalf("credentials = %d %s", status, mustJSON(out))
	}
	token, proxyPath := out["token"].(string), out["proxy_path"].(string)
	proxied := func(method, path string, body []byte) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+proxyPath+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}
	if status, body := proxied(http.MethodGet, "1.0/kb/accounts/"+a.KillBillAccountID+"/invoices", nil); status != http.StatusOK || !strings.Contains(string(body), invoices[0].ID) {
		t.Fatalf("own invoices through the proxy = %d %s", status, body)
	}
	if status, body := proxied(http.MethodGet, "1.0/kb/accounts?externalKey="+a.PublicID(), nil); status != http.StatusOK || !strings.Contains(string(body), a.KillBillAccountID) {
		t.Fatalf("own account by external key = %d %s", status, body)
	}
	for _, path := range []string{"1.0/kb/accounts/" + b.KillBillAccountID, "1.0/kb/accounts/" + b.KillBillAccountID + "/invoices", "1.0/kb/accounts?externalKey=" + b.PublicID()} {
		if status, body := proxied(http.MethodGet, path, nil); status != http.StatusNotFound || strings.Contains(string(body), b.KillBillAccountID) {
			t.Fatalf("another tenant's account %s = %d %s", path, status, body)
		}
	}
	if status, _ := proxied(http.MethodPost, "1.0/kb/subscriptions", []byte(`{"accountId":"`+b.KillBillAccountID+`","planName":"`+refPlanUsageFree+`"}`)); status != http.StatusNotFound {
		t.Fatalf("subscription for another tenant through the proxy = %d", status)
	}
	if status, body := proxied(http.MethodPost, "1.0/kb/subscriptions", []byte(`{"accountId":"`+a.KillBillAccountID+`","planName":"`+refPlanUsage+`","externalKey":"stack-proxy-`+suffix+`"}`)); status != http.StatusCreated {
		t.Fatalf("subscription through the proxy = %d %s", status, body)
	}
	// Another brand's tenant credentials cannot read this brand's account.
	if _, err := s.kb.AccountByExternalKey(ctx, killbill.TenantCredentials{APIKey: "zs-second", APISecret: secondKillBillSecret}, a.PublicID()); err == nil {
		t.Fatal("zs-second reads a zs-reference account")
	}
	_ = secondID

	// Push notifications from Kill Bill reach Core.
	if callbackAddr == "" {
		t.Log("ZS_TEST_CALLBACK_ADDR is not set: Kill Bill cannot reach this test, so push notifications are not checked")
		return
	}
	deadline := time.Now().Add(90 * time.Second)
	var got map[string]int
	for time.Now().Before(deadline) {
		got = map[string]int{}
		rows, err := db.Pool.Query(ctx, `SELECT event_type, count(*) FROM outbox WHERE tenant_id = $1 AND event_type LIKE 'billing.%' GROUP BY event_type`, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var typ string
			var n int
			if err := rows.Scan(&typ, &n); err != nil {
				t.Fatal(err)
			}
			got[typ] = n
		}
		if got[billing.EventSubscriptionChanged] > 0 && got[billing.EventInvoiceCreated] > 0 && got[billing.EventPaymentSucceeded] > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if got[billing.EventSubscriptionChanged] == 0 || got[billing.EventInvoiceCreated] == 0 || got[billing.EventPaymentSucceeded] == 0 {
		t.Fatalf("Core events from Kill Bill push notifications = %v", got)
	}
	var last string
	if err := db.Pool.QueryRow(ctx, `SELECT coalesce(last_event_type, '') FROM tenant_billing_standing WHERE tenant_id = $1`, a.ID).Scan(&last); err != nil || last == "" {
		t.Fatalf("standing cache = %q %v", last, err)
	}
	var receipts int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM killbill_notifications WHERE brand_id = $1`, refID).Scan(&receipts); err != nil || receipts == 0 {
		t.Fatalf("receipts = %d %v", receipts, err)
	}
	t.Logf("Core events from Kill Bill: %v, receipts %d", got, receipts)
}

func intPtr(n int) *int { return &n }
