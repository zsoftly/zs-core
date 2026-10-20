//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

var webhookSecret = []byte(strings.Repeat("w", 32))

func newBillingAPI(t *testing.T) *tenantAPI {
	t.Helper()
	a := newTenantAPI(t)
	svc := &billing.Service{DB: a.db.Pool, KillBill: a.fake.KillBill(t), Secrets: a.fake.Secrets(), WebhookSecret: webhookSecret}
	a.srv.Close()
	a.srv = httptest.NewServer(httpapi.New(httpapi.Options{DB: a.db.Pool, Migrations: migrations.FS,
		Verifier: a.signer.Verifier(a.db.Pool), Tenants: a.eng, Billing: svc}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *tenantAPI) raw(method, path, credential, contentType string, body []byte) (int, http.Header, []byte) {
	a.t.Helper()
	req, _ := http.NewRequest(method, a.srv.URL+path, bytes.NewReader(body))
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, out
}

func TestBillingRoutesEndToEnd(t *testing.T) {
	a := newBillingAPI(t)
	ctx := context.Background()
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-bill")
	other := a.fake.InsertBrand(t, a.db.Pool, "zs-elsewhere")
	owner := a.person(b, "owner@example.test")
	member := a.person(b, "member@example.test")
	outsider := a.person(other, "outsider@example.test")
	tok := a.token(owner, mfa())
	id := a.createTenant(owner, "Billing Co")
	a.join(id, owner, member, "member")
	memberTok := a.token(member, enginetest.TokenOptions{})
	base := "/v1/tenants/" + id
	brandID := idgen.Format(idgen.Brand, b.ID)

	if r := a.call(http.MethodGet, "/v1/brands/"+brandID+"/catalogue", memberTok, nil); r.status != http.StatusOK || len(r.body["products"].([]any)) != 2 {
		t.Fatalf("catalogue = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, "/v1/brands/"+brandID+"/catalogue", a.token(outsider, enginetest.TokenOptions{}), nil); r.status != http.StatusNotFound {
		t.Fatalf("catalogue of another brand = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/billing", tok, nil); r.status != http.StatusOK || r.str("currency") != "CAD" || r.body["on_hold"] != false {
		t.Fatalf("summary = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/billing", memberTok, nil); r.status != http.StatusForbidden || r.code() != "permission_denied" {
		t.Fatalf("summary without billing:read = %d %v", r.status, r.body)
	}
	// tenant:read is enough for an entitlement check.
	if r := a.call(http.MethodGet, base+"/entitlements/core-test", memberTok, nil); r.status != http.StatusOK || r.body["allowed"] != false || r.str("reason") != "no_active_subscription" {
		t.Fatalf("entitlement = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/entitlements/bad%20feature", memberTok, nil); r.status != http.StatusBadRequest {
		t.Fatalf("bad feature = %d %v", r.status, r.body)
	}
	sub := a.call(http.MethodPost, base+"/subscriptions", tok, map[string]any{"plan": enginetest.PlanUsage, "idempotency_key": "http-sub-1", "reason": "signup"})
	if sub.status != http.StatusCreated || sub.str("phase") != "TRIAL" {
		t.Fatalf("subscribe = %d %v", sub.status, sub.body)
	}
	if r := a.call(http.MethodPost, base+"/subscriptions", tok, map[string]any{"plan": enginetest.PlanUsage, "idempotency_key": "http-sub-1", "reason": "signup"}); r.status != http.StatusCreated || r.str("id") != sub.str("id") {
		t.Fatalf("replay = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/subscriptions", memberTok, map[string]any{"plan": enginetest.PlanUsage, "idempotency_key": "http-sub-2"}); r.status != http.StatusForbidden {
		t.Fatalf("subscribe without billing:manage = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/entitlements/api-call", memberTok, nil); r.body["allowed"] != true || r.str("plan") != enginetest.PlanUsage || r.body["limit"] != nil {
		t.Fatalf("entitlement after subscribing = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/subscriptions/"+sub.str("id")+"/change", tok, map[string]any{"plan": enginetest.PlanBasic, "policy": "immediate"}); r.status != http.StatusOK || r.str("plan") != enginetest.PlanBasic {
		t.Fatalf("change = %d %v", r.status, r.body)
	}
	list := a.call(http.MethodGet, base+"/subscriptions", tok, nil)
	if list.status != http.StatusOK || len(list.body["data"].([]any)) != 1 {
		t.Fatalf("subscriptions = %d %v", list.status, list.body)
	}
	inv := a.call(http.MethodGet, base+"/invoices?limit=10", tok, nil)
	invoices, _ := inv.body["data"].([]any)
	if inv.status != http.StatusOK || len(invoices) != 1 || inv.body["next_cursor"] != nil {
		t.Fatalf("invoices = %d %v", inv.status, inv.body)
	}
	invID := invoices[0].(map[string]any)["id"].(string)
	if r := a.call(http.MethodGet, base+"/invoices/"+invID, tok, nil); r.status != http.StatusOK || len(r.body["items"].([]any)) != 1 {
		t.Fatalf("invoice = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/invoices/"+invID+"/pdf", tok, nil); r.status != http.StatusNotFound || r.code() != "invoice_pdf_unavailable" {
		t.Fatalf("pdf = %d %v", r.status, r.body)
	}

	// Codes: the first redemption is 201, the replay 200.
	if err := pgx.BeginFunc(ctx, a.db.Pool, func(tx pgx.Tx) error {
		_, err := billing.AddCreditCode(ctx, tx, b.ID, billing.NewCreditCode{Code: "HTTP-CODE", AmountMinor: 250, Currency: "CAD", Reason: "x", CreatedBy: "ops"},
			events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}, "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := a.call(http.MethodPost, base+"/credits/redeem", tok, map[string]any{"code": "http-code"}); r.status != http.StatusCreated || r.str("source") != "credit_code" {
		t.Fatalf("redeem = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/credits/redeem", tok, map[string]any{"code": "HTTP-CODE"}); r.status != http.StatusOK {
		t.Fatalf("redeem again = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/credits/redeem", tok, map[string]any{"code": "NOPE-NOPE"}); r.status != http.StatusNotFound || r.code() != "credit_code_invalid" {
		t.Fatalf("unknown code = %d %v", r.status, r.body)
	}

	// Staff routes need a billing grant in the brand.
	staff := a.person(b, "staff@example.test")
	staffTok := a.token(staff, enginetest.TokenOptions{Staff: true})
	grant := map[string]any{"amount_minor": 1000, "currency": "CAD", "reason": "goodwill", "idempotency_key": "http-grant-1"}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+id+"/credits", staffTok, grant); r.status != http.StatusNotFound {
		t.Fatalf("staff credit without a grant = %d %v", r.status, r.body)
	}
	if err := pgx.BeginFunc(ctx, a.db.Pool, func(tx pgx.Tx) error {
		_, err := identity.AddStaffGrant(ctx, tx, identity.StaffGrant{BrandID: b.ID, Issuer: b.Issuer, Subject: staff.Subject, Scope: identity.StaffBilling,
			GrantedBy: "test", Reason: "test"}, events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}, "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+id+"/credits", staffTok, grant); r.status != http.StatusCreated || r.str("source") != "staff" {
		t.Fatalf("staff credit = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+id+"/credits", staffTok, grant); r.status != http.StatusOK {
		t.Fatalf("staff credit replay = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+id+"/credits", tok, grant); r.status != http.StatusNotFound {
		t.Fatalf("customer on the staff route = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/billing", tok, nil); r.body["credit_minor"] != float64(1250) {
		t.Fatalf("summary after credits = %v", r.body)
	}
	tn, err := tenant.Get(ctx, a.db.Pool, scopeOf(b.ID, id))
	if err != nil {
		t.Fatal(err)
	}
	pay := a.fake.AddPurchase("zs-bill", tn.KillBillAccountID, 900, "CAD")
	refund := map[string]any{"amount_minor": 400, "reason": "partial", "idempotency_key": "http-refund-1"}
	if r := a.call(http.MethodPost, base+"/payments/"+pay+"/refund", tok, refund); r.status != http.StatusNotFound {
		t.Fatalf("refund by a customer = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/payments/"+pay+"/refund", staffTok, refund); r.status != http.StatusCreated || r.body["refunded_minor"] != float64(400) {
		t.Fatalf("refund = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/payments", tok, nil); r.status != http.StatusOK || len(r.body["data"].([]any)) != 1 {
		t.Fatalf("payments = %d %v", r.status, r.body)
	}
	pm := a.call(http.MethodPost, base+"/payment-methods", tok, map[string]any{"idempotency_key": "http-pm-1", "set_default": true})
	method, _ := pm.body["payment_method"].(map[string]any)
	if pm.status != http.StatusCreated || method["plugin_name"] != enginetest.ExternalPlugin {
		t.Fatalf("payment method = %d %v", pm.status, pm.body)
	}
	if r := a.call(http.MethodPut, base+"/payment-methods/"+method["id"].(string)+"/default", tok, nil); r.status != http.StatusOK || r.body["is_default"] != true {
		t.Fatalf("set default = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/payment-methods", tok, nil); r.status != http.StatusOK || len(r.body["data"].([]any)) != 1 {
		t.Fatalf("payment methods = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodDelete, base+"/payment-methods/"+method["id"].(string), tok, nil); r.status != http.StatusUnprocessableEntity || r.code() != "billing_rejected" {
		t.Fatalf("delete the default = %d %v", r.status, r.body)
	}

	// Credentials and the proxy.
	cred := a.call(http.MethodGet, base+"/billing/credentials", tok, nil)
	if cred.status != http.StatusOK || !strings.HasPrefix(cred.str("token"), billing.CredentialPrefix) || cred.str("account_id") != tn.KillBillAccountID {
		t.Fatalf("credentials = %d %v", cred.status, cred.body)
	}
	if r := a.call(http.MethodGet, base+"/billing/credentials", memberTok, nil); r.status != http.StatusForbidden {
		t.Fatalf("credentials without billing:manage = %d %v", r.status, r.body)
	}
	status, _, body := a.raw(http.MethodGet, cred.str("proxy_path")+"1.0/kb/accounts/"+tn.KillBillAccountID+"/invoices", cred.str("token"), "", nil)
	if status != http.StatusOK || !strings.Contains(string(body), invID) {
		t.Fatalf("proxied invoices = %d %s", status, body)
	}
	otherTenant := a.createTenant(owner, "Other Co")
	otherTn, _ := tenant.Get(ctx, a.db.Pool, scopeOf(b.ID, otherTenant))
	if status, _, body := a.raw(http.MethodGet, cred.str("proxy_path")+"1.0/kb/accounts/"+otherTn.KillBillAccountID, cred.str("token"), "", nil); status != http.StatusNotFound {
		t.Fatalf("another tenant's account through the proxy = %d %s", status, body)
	}
	if status, _, _ := a.raw(http.MethodGet, "/v1/tenants/"+otherTenant+"/billing/proxy/1.0/kb/accounts/"+otherTn.KillBillAccountID, cred.str("token"), "", nil); status != http.StatusUnauthorized {
		t.Fatalf("credential on another tenant's route = %d", status)
	}
	sbody := []byte(`{"accountId":"` + tn.KillBillAccountID + `","planName":"` + enginetest.PlanUsage + `"}`)
	status, header, _ := a.raw(http.MethodPost, cred.str("proxy_path")+"1.0/kb/subscriptions", cred.str("token"), "application/json", sbody)
	if status != http.StatusCreated || !strings.HasPrefix(header.Get("Location"), cred.str("proxy_path")+"1.0/kb/subscriptions/") || header.Get("Cache-Control") != "no-store" {
		t.Fatalf("proxied subscription = %d %v", status, header)
	}

	// Push notifications.
	hook := brand.WebhookPath(webhookSecret, b.ID)
	note, _ := json.Marshal(map[string]any{"eventType": "INVOICE_CREATION", "accountId": tn.KillBillAccountID, "objectType": "INVOICE", "objectId": invID, "metaData": nil})
	for i := 0; i < 2; i++ {
		if status, _, body := a.raw(http.MethodPost, hook, "", "application/json", note); status != http.StatusNoContent {
			t.Fatalf("notification %d = %d %s", i, status, body)
		}
	}
	var events int
	if err := a.db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type = 'billing.invoice.created'`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("invoice events = %d %v", events, err)
	}
	if status, _, _ := a.raw(http.MethodPost, "/v1/billing/webhooks/killbill/"+brandID+"/"+strings.Repeat("0", 64), "", "application/json", note); status != http.StatusNotFound {
		t.Fatalf("wrong token = %d", status)
	}

	// A closed tenant has no billing routes.
	if _, err := tenant.Close(ctx, a.db.Pool, a.eng, scopeOf(b.ID, id), events0(), "test", "test"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/billing", "/subscriptions", "/invoices", "/payments", "/payment-methods", "/entitlements/core-test", "/billing/credentials"} {
		if r := a.call(http.MethodGet, base+path, tok, nil); r.status != http.StatusNotFound || r.code() != "tenant_not_found" {
			t.Errorf("closed tenant %s = %d %v", path, r.status, r.body)
		}
	}
	if status, _, _ := a.raw(http.MethodGet, cred.str("proxy_path")+"1.0/kb/accounts/"+tn.KillBillAccountID, cred.str("token"), "", nil); status != http.StatusNotFound {
		t.Fatalf("proxy for a closed tenant = %d", status)
	}
}

func scopeOf(b idgen.UUID, tenantID string) scope.Scope {
	id, _ := idgen.ParseAs(idgen.Tenant, tenantID)
	return scope.Scope{Brand: b, Tenant: id}
}

func events0() events.Actor { return events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"} }
