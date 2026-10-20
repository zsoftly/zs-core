package killbill_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
)

var cc = killbill.CallContext{CreatedBy: "user:idn_test", Reason: "test reason", Comment: "correlation-1"}

func fakeTenant(t *testing.T) (*enginetest.Fake, *killbill.Client, killbill.TenantCredentials, string) {
	t.Helper()
	f := enginetest.New(t)
	f.AddKillBillTenant("zs-test", "secret")
	kb := f.KillBill(t)
	creds := killbill.TenantCredentials{APIKey: "zs-test", APISecret: "secret"}
	id, err := kb.CreateAccount(context.Background(), creds, cc, killbill.Account{ExternalKey: "tnt_x", Name: "X", Currency: "CAD"})
	if err != nil {
		t.Fatal(err)
	}
	return f, kb, creds, id
}

func TestCatalogueSubscriptionsAndInvoices(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, acct := fakeTenant(t)
	cat, err := kb.Catalogue(ctx, creds)
	if err != nil || len(cat) != 1 || len(cat[0].Products) != 3 || cat[0].Products[0].Plans[0].Phases[1].Prices[0].Value != "10.00" {
		t.Fatalf("catalogue = %+v %v", cat, err)
	}
	id, err := kb.CreateSubscription(ctx, creds, cc, acct, enginetest.PlanUsage, "zs-sub-1")
	if err != nil || id == "" {
		t.Fatalf("create = %q %v", id, err)
	}
	// Kill Bill 0.24.22 answers a repeated external key with 500.
	if _, err := kb.CreateSubscription(ctx, creds, cc, acct, enginetest.PlanUsage, "zs-sub-1"); !killbill.IsUnavailable(err) {
		t.Fatalf("repeated external key: %v", err)
	}
	byKey, err := kb.SubscriptionByExternalKey(ctx, creds, "zs-sub-1")
	if err != nil || byKey.SubscriptionID != id || byKey.PhaseType != "TRIAL" || byKey.State != "ACTIVE" {
		t.Fatalf("by key = %+v %v", byKey, err)
	}
	if _, err := kb.SubscriptionByExternalKey(ctx, creds, "zs-none"); !errors.Is(err, killbill.ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := kb.ChangePlan(ctx, creds, cc, id, enginetest.PlanBasic, killbill.PolicyImmediate); err != nil {
		t.Fatal(err)
	}
	if s, _ := kb.Subscription(ctx, creds, id); s.PlanName != enginetest.PlanBasic {
		t.Fatalf("after change = %+v", s)
	}
	var kbErr *killbill.Error
	if err := kb.ChangePlan(ctx, creds, cc, id, "no-such-plan", killbill.PolicyImmediate); !errors.As(err, &kbErr) || kbErr.Code == 0 {
		t.Fatalf("unknown plan: %v", err)
	}
	if err := kb.CancelSubscription(ctx, creds, cc, id, killbill.PolicyImmediate); err != nil {
		t.Fatal(err)
	}
	bundles, err := kb.AccountBundles(ctx, creds, acct)
	if err != nil || len(bundles) != 1 || bundles[0].Subscriptions[0].State != "CANCELLED" {
		t.Fatalf("bundles = %+v %v", bundles, err)
	}
	invoices, err := kb.AccountInvoices(ctx, creds, acct, true)
	if err != nil || len(invoices) != 1 || invoices[0].Items[0].ItemType != "FIXED" {
		t.Fatalf("invoices = %+v %v", invoices, err)
	}
	if in, err := kb.Invoice(ctx, creds, invoices[0].InvoiceID); err != nil || in.AccountID != acct {
		t.Fatalf("invoice = %+v %v", in, err)
	}
	// Every write carried the audit headers.
	for _, w := range f.Writes() {
		if w.APIKey == "zs-test" && (w.CreatedBy != cc.CreatedBy || w.Reason != cc.Reason || w.Comment != cc.Comment) {
			t.Fatalf("write without audit headers: %+v", w)
		}
	}
}

func TestCreditsChargesAndBalance(t *testing.T) {
	ctx := context.Background()
	_, kb, creds, acct := fakeTenant(t)
	item, err := kb.CreateCredit(ctx, creds, cc, killbill.InvoiceItem{AccountID: acct, Amount: "12.34", Currency: "CAD", Description: "zs-core credit crd_1"})
	if err != nil || item.InvoiceItemID == "" || item.ItemType != "CREDIT_ADJ" || item.Amount != "12.34" {
		t.Fatalf("credit = %+v %v", item, err)
	}
	var kbErr *killbill.Error
	if _, err := kb.CreateCredit(ctx, creds, cc, killbill.InvoiceItem{AccountID: acct, Amount: "-1.00", Currency: "CAD"}); !errors.As(err, &kbErr) || kbErr.Code != 4009 {
		t.Fatalf("negative credit: %v", err)
	}
	if _, err := kb.CreateExternalCharge(ctx, creds, cc, killbill.InvoiceItem{AccountID: acct, Amount: "2.00", Currency: "CAD", Description: "expiry"}); err != nil {
		t.Fatal(err)
	}
	a, err := kb.AccountWithBalance(ctx, creds, acct)
	if err != nil || a.AccountCBA != "10.34" || a.AccountBalance != "-10.34" {
		t.Fatalf("balance = %+v %v", a, err)
	}
}

func TestPaymentsRefundsAndPaymentMethods(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, acct := fakeTenant(t)
	pay := f.AddPurchase("zs-test", acct, 500, "CAD")
	refund := killbill.PaymentTransaction{Amount: "2.00", Currency: "CAD", TransactionExternalKey: "zs-refund-1"}
	if err := kb.Refund(ctx, creds, cc, pay, refund); err != nil {
		t.Fatal(err)
	}
	if err := kb.Refund(ctx, creds, cc, pay, refund); !errors.Is(err, killbill.ErrTransactionKeyExists) {
		t.Fatalf("second refund with one key: %v", err)
	}
	p, err := kb.Payment(ctx, creds, pay)
	if err != nil || p.RefundedAmount != "2.00" || len(p.Transactions) != 2 {
		t.Fatalf("payment = %+v %v", p, err)
	}
	pm, err := kb.AddPaymentMethod(ctx, creds, cc, acct, killbill.PaymentMethod{PluginName: enginetest.ExternalPlugin, ExternalKey: "zs-pm-1"}, true)
	if err != nil || pm == "" {
		t.Fatalf("add = %q %v", pm, err)
	}
	var kbErr *killbill.Error
	// The external payment plugin takes one payment method per account.
	if _, err := kb.AddPaymentMethod(ctx, creds, cc, acct, killbill.PaymentMethod{PluginName: enginetest.ExternalPlugin}, false); !errors.As(err, &kbErr) || kbErr.Code != 7023 {
		t.Fatalf("second external payment method: %v", err)
	}
	form, err := kb.HostedPaymentPage(ctx, creds, cc, acct, killbill.PaymentMethod{PluginName: enginetest.HostedPlugin, ExternalKey: "zs-pm-2"}, nil, nil)
	if err != nil || form.FormURL == "" || form.FormFields["session"] != "sess_test_123" {
		t.Fatalf("hosted page = %+v %v", form, err)
	}
	list, err := kb.AccountPaymentMethods(ctx, creds, acct)
	if err != nil || len(list) != 2 {
		t.Fatalf("payment methods = %+v %v", list, err)
	}
	if err := kb.DeletePaymentMethod(ctx, creds, cc, pm); !errors.As(err, &kbErr) {
		t.Fatalf("deleting the default: %v", err)
	}
	if err := kb.SetDefaultPaymentMethod(ctx, creds, cc, acct, list[1].PaymentMethodID); err != nil {
		t.Fatal(err)
	}
	if err := kb.DeletePaymentMethod(ctx, creds, cc, pm); err != nil {
		t.Fatal(err)
	}
}

func TestOverdueBlockingCallbacksAndNodes(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, acct := fakeTenant(t)
	od, err := kb.AccountOverdue(ctx, creds, acct)
	if err != nil || !od.IsClearState || od.Name != "__KILLBILL__CLEAR__OVERDUE_STATE__" {
		t.Fatalf("overdue = %+v %v", od, err)
	}
	if err := kb.SetAccountBlockingState(ctx, creds, cc, acct, killbill.BlockingState{StateName: "S", Service: "svc", IsBlockChange: true}); err != nil {
		t.Fatal(err)
	}
	states, err := kb.AccountBlockingStates(ctx, creds, acct)
	if err != nil || len(states) != 1 || !states[0].IsBlockChange || states[0].EffectiveDate == "" {
		t.Fatalf("blocking = %+v %v", states, err)
	}
	for _, cb := range []string{"http://core/a", "http://core/b"} {
		if err := kb.RegisterNotificationCallback(ctx, creds, cc, cb); err != nil {
			t.Fatal(err)
		}
	}
	// Registering replaces the callback, as Kill Bill 0.24.22 does.
	if got, err := kb.NotificationCallbacks(ctx, creds); err != nil || len(got) != 1 || got[0] != "http://core/b" {
		t.Fatalf("callbacks = %v %v", got, err)
	}
	if err := kb.SendNodeCommand(ctx, cc, killbill.NodeCommand{NodeCommandType: killbill.NodeCommandInstallPlugin, IsSystemCommandType: true,
		NodeCommandProperties: []killbill.NodeCommandProperty{{Key: "pluginKey", Value: "stripe"}, {Key: "pluginVersion", Value: "8.0.3"}, {Key: "pluginUri", Value: "https://repo.example.test/x.jar"}}}); err != nil {
		t.Fatal(err)
	}
	if v, state := f.Plugin("stripe"); v != "8.0.3" || state != "STOPPED" {
		t.Fatalf("after install = %s %s", v, state)
	}
	nodes, err := kb.NodesInfo(ctx)
	if err != nil || len(nodes) != 1 || nodes[0].NodeName == "" {
		t.Fatalf("nodes = %+v %v", nodes, err)
	}
}

// A 408, which Kill Bill sends when a call that waits for completion runs
// out of time after it accepted the change, is unavailability, never a
// rejection.
func TestRequestTimeoutIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusRequestTimeout) }))
	t.Cleanup(srv.Close)
	kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = kb.CreateSubscription(context.Background(), killbill.TenantCredentials{APIKey: "k", APISecret: "s"}, cc, "a", "p", "k")
	if !killbill.IsUnavailable(err) {
		t.Fatalf("408: %v", err)
	}
}

// Forward passes the tenant credentials, adds the audit headers on writes
// only, and returns no Kill Bill text in its errors.
func TestForward(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 7005, "message": "secret provider detail"})
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	kb, _ := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	creds := killbill.TenantCredentials{APIKey: "k", APISecret: "s"}
	resp, err := kb.Forward(context.Background(), creds, cc, http.MethodGet, "/1.0/kb/accounts/x", "", nil)
	if err != nil || string(resp.Body) != `{"ok":true}` || got.Get(killbill.HeaderAPIKey) != "k" || got.Get(killbill.HeaderCreatedBy) != "" {
		t.Fatalf("GET = %s %v %v", resp.Body, err, got)
	}
	_, err = kb.Forward(context.Background(), creds, cc, http.MethodPost, "/1.0/kb/subscriptions", "application/json", []byte(`{}`))
	var kbErr *killbill.Error
	if !errors.As(err, &kbErr) || kbErr.Code != 7005 || strings.Contains(err.Error(), "secret provider detail") || got.Get(killbill.HeaderCreatedBy) != cc.CreatedBy {
		t.Fatalf("POST = %v %v", err, got)
	}
}

func TestUsageRecordsAndTrackingIDs(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, acct := fakeTenant(t)
	sub, err := kb.CreateSubscription(ctx, creds, cc, acct, enginetest.PlanMetered, "zs-sub-metered")
	if err != nil {
		t.Fatal(err)
	}
	rec := killbill.SubscriptionUsageRecord{SubscriptionID: sub, TrackingID: "zs-usg-1", UnitUsageRecords: []killbill.UnitUsageRecords{
		{UnitType: enginetest.UnitAPICall, UsageRecords: []killbill.UsageRecord{{RecordDate: "2026-10-20", Amount: "2.5"}}}}}
	if err := kb.RecordUsage(ctx, creds, cc, rec); err != nil {
		t.Fatal(err)
	}
	// Kill Bill deduplicates on the tracking ID per subscription, whatever
	// the amount.
	rec.UnitUsageRecords[0].UsageRecords[0].Amount = "7"
	if err := kb.RecordUsage(ctx, creds, cc, rec); !errors.Is(err, killbill.ErrTrackingIDExists) {
		t.Fatalf("same tracking ID: %v", err)
	}
	rec.SubscriptionID = "00000000-0000-0000-0000-000000000001"
	if err := kb.RecordUsage(ctx, creds, cc, rec); !errors.Is(err, killbill.ErrNotFound) {
		t.Fatalf("unknown subscription: %v", err)
	}
	got, err := kb.RolledUpUsage(ctx, creds, sub, "2026-10-01", "2026-11-01")
	if err != nil || len(got.RolledUpUnits) != 1 || got.RolledUpUnits[0].UnitType != enginetest.UnitAPICall || got.RolledUpUnits[0].Amount != "2.500000000" {
		t.Fatalf("rolled up = %+v %v", got, err)
	}
	if n := len(f.UsageRecords("zs-test")); n != 1 {
		t.Fatalf("records = %d", n)
	}
}
