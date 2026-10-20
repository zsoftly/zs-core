//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/quota"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

func countRows(t *testing.T, a *extensionsAPI, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := a.db.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type extensionsAPI struct {
	*tenantAPI
	bill  *billing.Service
	meter *metering.Service
}

func newExtensionsAPI(t *testing.T) *extensionsAPI {
	t.Helper()
	a := newTenantAPI(t)
	bill := &billing.Service{DB: a.db.Pool, KillBill: a.fake.KillBill(t), Secrets: a.fake.Secrets(), WebhookSecret: webhookSecret}
	meter := &metering.Service{DB: a.db.Pool, Billing: bill}
	a.srv.Close()
	a.srv = httptest.NewServer(httpapi.New(httpapi.Options{DB: a.db.Pool, Migrations: migrations.FS,
		Verifier: a.signer.Verifier(a.db.Pool), Tenants: a.eng, Billing: bill,
		Wallet:   &wallet.Service{DB: a.db.Pool, Billing: bill, Metering: meter},
		Metering: meter, Quota: &quota.Service{DB: a.db.Pool, Billing: bill}}))
	t.Cleanup(a.srv.Close)
	return &extensionsAPI{tenantAPI: a, bill: bill, meter: meter}
}

func TestExtensionRoutesEndToEnd(t *testing.T) {
	a := newExtensionsAPI(t)
	ctx := context.Background()
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-ext")
	other := a.fake.InsertBrand(t, a.db.Pool, "zs-ext-other")
	owner := a.person(b, "owner@example.test")
	member := a.person(b, "member@example.test")
	operator := a.person(b, "operator@example.test")
	support := a.person(b, "support@example.test")
	outsider := a.person(other, "outsider@example.test")
	grantStaff(t, a.db, b, operator, identity.StaffOperator)
	grantStaff(t, a.db, b, support, identity.StaffSupport)
	grantStaff(t, a.db, other, outsider, identity.StaffOperator)
	tok := a.token(owner, mfa())
	memberTok := a.token(member, enginetest.TokenOptions{})
	opTok := a.token(operator, enginetest.TokenOptions{Staff: true, MFA: true})
	supportTok := a.token(support, enginetest.TokenOptions{Staff: true, MFA: true})
	id := a.createTenant(owner, "Extensions Co")
	a.join(id, owner, member, "member")
	tenantID, _ := idgen.ParseAs(idgen.Tenant, id)
	sc := scope.Scope{Brand: b.ID, Tenant: tenantID}
	base := "/v1/tenants/" + id
	brandPath := "/v1/brands/" + idgen.Format(idgen.Brand, b.ID)

	// Meters and quotas: staff operator of the brand only.
	meters := map[string]any{"meters": []map[string]any{{"slug": "api-calls", "unit": "request", "killbill_unit": enginetest.UnitAPICall, "dimensions": []string{"region"}}}, "reason": "launch"}
	if r := a.call(http.MethodPut, brandPath+"/meters", tok, meters); r.status != http.StatusNotFound {
		t.Fatalf("meters by a customer = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPut, brandPath+"/meters", a.token(outsider, enginetest.TokenOptions{Staff: true, MFA: true}), meters); r.status != http.StatusNotFound {
		t.Fatalf("meters by another brand's operator = %d %v", r.status, r.body)
	}
	bad := map[string]any{"meters": []map[string]any{{"slug": "gpu", "unit": "count", "killbill_unit": "gpu-hour"}}}
	if r := a.call(http.MethodPut, brandPath+"/meters", opTok, bad); r.status != http.StatusBadRequest || r.code() != "brand_manifest_invalid" {
		t.Fatalf("meter on a unit outside the catalogue = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPut, brandPath+"/meters", opTok, meters); r.status != http.StatusOK || len(r.body["data"].([]any)) != 1 {
		t.Fatalf("meters = %d %v", r.status, r.body)
	}
	quotas := map[string]any{"quotas": []map[string]any{
		{"feature": "jobs", "plan": "*", "limit": 1, "window": "concurrent"},
		{"feature": enginetest.UnitAPICall, "plan": enginetest.PlanMetered, "limit": 1000, "window": "month", "meter": "api-calls"}}}
	if r := a.call(http.MethodPut, brandPath+"/quotas", opTok, quotas); r.status != http.StatusOK || len(r.body["data"].([]any)) != 2 {
		t.Fatalf("quotas = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, brandPath+"/quotas", opTok, nil); r.status != http.StatusOK || len(r.body["data"].([]any)) != 2 {
		t.Fatalf("list quotas = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, brandPath+"/meters", memberTok, nil); r.status != http.StatusNotFound {
		t.Fatalf("meters for a member = %d %v", r.status, r.body)
	}

	// Credit and a usage subscription, through Kill Bill.
	if _, _, err := a.bill.GrantCredit(ctx, sc, billing.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:staff"}, Reason: "test", CorrelationID: "c"},
		billing.StaffCredit{AmountMinor: 1000, Currency: "CAD", IdempotencyKey: "http-credit-1"}); err != nil {
		t.Fatal(err)
	}
	acct, _ := a.fake.Account(b.Slug, id)
	sub, err := a.fake.KillBill(t).CreateSubscription(ctx, killbill.TenantCredentials{APIKey: b.Slug, APISecret: "kb-secret-" + b.Slug},
		killbill.CallContext{CreatedBy: "test"}, acct.ID, enginetest.PlanMetered, "http-metered")
	if err != nil {
		t.Fatal(err)
	}

	// Wallet and reservations.
	if r := a.call(http.MethodGet, base+"/wallet", tok, nil); r.status != http.StatusOK || r.body["killbill_credit_minor"] != float64(1000) || r.body["available_minor"] != float64(1000) {
		t.Fatalf("wallet = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/wallet", memberTok, nil); r.status != http.StatusForbidden {
		t.Fatalf("wallet without billing:read = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations", memberTok, map[string]any{"external_ref": "job-1", "amount_minor": 10}); r.status != http.StatusForbidden || r.code() != "permission_denied" {
		t.Fatalf("reserve without wallet:reserve = %d %v", r.status, r.body)
	}
	hold := a.call(http.MethodPost, base+"/reservations", tok, map[string]any{"external_ref": "job-1", "amount_minor": 600, "label": "build", "lease_seconds": 600})
	if hold.status != http.StatusCreated || hold.str("status") != "held" {
		t.Fatalf("reserve = %d %v", hold.status, hold.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations", tok, map[string]any{"external_ref": "job-1", "amount_minor": 500}); r.status != http.StatusOK || r.body["amount_minor"] != float64(600) {
		t.Fatalf("reserve replay = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations", tok, map[string]any{"external_ref": "job-2", "amount_minor": 500}); r.status != http.StatusConflict || r.code() != "insufficient_credit" {
		t.Fatalf("reserve past the credit = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations/job-1/extend", tok, map[string]any{"lease_seconds": 1200}); r.status != http.StatusOK {
		t.Fatalf("extend = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations/job-1/settle", tok, map[string]any{"credit_minor": 250, "reason": "build done"}); r.status != http.StatusOK ||
		r.str("status") != "settled" || r.str("killbill_ref_type") != "external_charge" || r.body["settled_minor"] != float64(250) {
		t.Fatalf("settle = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations/job-1/release", tok, map[string]any{}); r.status != http.StatusConflict || r.code() != "reservation_not_held" {
		t.Fatalf("release a settled hold = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations/no%20such/release", tok, map[string]any{}); r.status != http.StatusNotFound || r.code() != "reservation_not_found" {
		t.Fatalf("release a malformed ref = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations", tok, map[string]any{"external_ref": "job-3", "amount_minor": 100}); r.status != http.StatusCreated {
		t.Fatalf("reserve job-3 = %d %v", r.status, r.body)
	}
	// A usage settlement needs Kill Bill's reads to choose and price the
	// subscription: with Kill Bill down it is refused and nothing changes.
	a.fake.SetDown(false, true)
	if r := a.call(http.MethodPost, base+"/reservations/job-3/settle", tok, map[string]any{"usage": map[string]any{"unit": enginetest.UnitAPICall, "quantity_micro": 5_000_000}}); r.status != http.StatusServiceUnavailable || r.code() != "billing_unavailable" {
		t.Fatalf("settle with Kill Bill down = %d %v", r.status, r.body)
	}
	a.fake.SetDown(false, false)
	// Kill Bill cannot take the write: the settlement waits.
	a.fake.FailWrites("/1.0/kb/usages", 1, http.StatusServiceUnavailable)
	if r := a.call(http.MethodPost, base+"/reservations/job-3/settle", tok, map[string]any{"usage": map[string]any{"unit": enginetest.UnitAPICall, "quantity_micro": 5_000_000}}); r.status != http.StatusAccepted || r.body["settlement_pending"] != true || r.str("killbill_subscription_id") != sub {
		t.Fatalf("settle with Kill Bill unable to take it = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/reservations?status=held", tok, nil); r.status != http.StatusOK || len(r.body["data"].([]any)) != 1 {
		t.Fatalf("held reservations = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/wallet", tok, nil); r.body["killbill_credit_minor"] != float64(750) || r.body["reserved_minor"] != float64(100) || r.body["pending_settlements"] != float64(1) {
		t.Fatalf("wallet with a pending settlement = %d %v", r.status, r.body)
	}

	// Usage.
	key := a.call(http.MethodPost, base+"/api-keys", tok, map[string]any{"name": "meter", "permissions": []string{"usage:write", "usage:read"}})
	if key.status != http.StatusCreated {
		t.Fatalf("api key = %d %v", key.status, key.body)
	}
	keyTok := key.str("secret")
	ev := map[string]any{"meter": "api-calls", "event_id": "e-1", "quantity_micro": 2_000_000, "dimensions": map[string]string{"region": "yow"}}
	first := a.call(http.MethodPost, base+"/usage", keyTok, ev)
	if first.status != http.StatusCreated || first.str("meter") != "api-calls" {
		t.Fatalf("usage = %d %v", first.status, first.body)
	}
	ev["occurred_at"] = first.str("occurred_at")
	if r := a.call(http.MethodPost, base+"/usage", keyTok, ev); r.status != http.StatusOK || r.str("id") != first.str("id") {
		t.Fatalf("usage replay = %d %v", r.status, r.body)
	}
	ev["quantity_micro"] = 3
	if r := a.call(http.MethodPost, base+"/usage", keyTok, ev); r.status != http.StatusConflict || r.code() != "usage_event_conflict" {
		t.Fatalf("usage conflict = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/usage", keyTok, map[string]any{"meter": "gpu", "event_id": "e-9", "quantity_micro": 1}); r.status != http.StatusNotFound || r.code() != "meter_not_found" {
		t.Fatalf("unknown meter = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/usage", memberTok, map[string]any{"meter": "api-calls", "event_id": "m", "quantity_micro": 1}); r.status != http.StatusForbidden {
		t.Fatalf("usage without usage:write = %d %v", r.status, r.body)
	}
	batch := a.call(http.MethodPost, base+"/usage/batch", keyTok, map[string]any{"events": []map[string]any{
		{"meter": "api-calls", "event_id": "b-1", "quantity_micro": 1_000_000}, {"meter": "api-calls", "event_id": "e-1", "quantity_micro": 2_000_000, "dimensions": map[string]string{"region": "yow"}, "occurred_at": first.str("occurred_at")}}})
	items, _ := batch.body["data"].([]any)
	if batch.status != http.StatusOK || len(items) != 2 || items[0].(map[string]any)["replayed"] != false || items[1].(map[string]any)["replayed"] != true {
		t.Fatalf("batch = %d %v", batch.status, batch.body)
	}
	if r := a.call(http.MethodGet, base+"/usage/events?limit=1", keyTok, nil); r.status != http.StatusOK || len(r.body["data"].([]any)) != 1 || r.body["next_cursor"] == nil {
		t.Fatalf("events = %d %v", r.status, r.body)
	}
	sum := a.call(http.MethodGet, base+"/usage/summary?meter=api-calls", keyTok, nil)
	if totals, _ := sum.body["totals"].([]any); sum.status != http.StatusOK || len(totals) != 1 || totals[0].(map[string]any)["quantity_micro"] != float64(3_000_000) {
		t.Fatalf("summary = %d %v", sum.status, sum.body)
	}
	if _, err := a.meter.Forward(ctx); err != nil {
		t.Fatal(err)
	}
	billed := a.call(http.MethodGet, base+"/usage/billed?subscription="+sub, keyTok, nil)
	data, _ := billed.body["data"].([]any)
	if billed.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("billed = %d %v", billed.status, billed.body)
	}
	if units := data[0].(map[string]any)["units"].([]any); len(units) != 1 || units[0].(map[string]any)["quantity_micro"] != float64(3_000_000) {
		t.Fatalf("billed units = %v", units)
	}

	// Entitlements, consumption and overrides.
	if r := a.call(http.MethodGet, base+"/entitlements/"+enginetest.UnitAPICall, memberTok, nil); r.status != http.StatusOK || r.body["allowed"] != true ||
		r.body["limit"] != float64(1000) || r.body["used"] != float64(3) || r.str("window") != "month" {
		t.Fatalf("entitlement = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/consume", memberTok, map[string]any{"ref": "job-a"}); r.status != http.StatusForbidden {
		t.Fatalf("consume without quota:consume = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/consume", tok, map[string]any{"ref": "job-a"}); r.status != http.StatusOK || r.body["used"] != float64(1) {
		t.Fatalf("consume = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/consume", tok, map[string]any{"ref": "job-b"}); r.status != http.StatusConflict || r.code() != "quota_exceeded" {
		t.Fatalf("consume past the limit = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+id+"/quota-overrides", opTok, map[string]any{"feature": "jobs", "limit": 3, "reason": "pilot"}); r.status != http.StatusNotFound {
		t.Fatalf("override by an operator = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+id+"/quota-overrides", supportTok, map[string]any{"feature": "jobs", "limit": 3, "reason": "pilot"}); r.status != http.StatusCreated || r.body["limit"] != float64(3) {
		t.Fatalf("override = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/consume", tok, map[string]any{"ref": "job-b", "amount": 2}); r.status != http.StatusOK || r.body["used"] != float64(3) || r.body["allowed"] != false {
		t.Fatalf("consume within the override = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/release", tok, map[string]any{"ref": "job-b"}); r.status != http.StatusOK || r.body["released"] != float64(2) {
		t.Fatalf("release = %d %v", r.status, r.body)
	}

	// Another tenant's member sees none of it.
	stranger := a.person(b, "stranger@example.test")
	a.createTenant(stranger, "Stranger Co")
	strangerTok := a.token(stranger, mfa())
	for _, path := range []string{"/wallet", "/reservations", "/usage/events", "/usage/summary", "/entitlements/jobs"} {
		if r := a.call(http.MethodGet, base+path, strangerTok, nil); r.status != http.StatusNotFound || r.code() != "tenant_not_found" {
			t.Fatalf("stranger on %s = %d %v", path, r.status, r.body)
		}
	}
	if r := a.call(http.MethodPost, base+"/reservations/job-3/release", strangerTok, map[string]any{}); r.status != http.StatusNotFound {
		t.Fatalf("stranger releasing a hold = %d %v", r.status, r.body)
	}
}

// Holds over HTTP: admins do not hold wallet:reserve, a hold belongs to the
// API key or membership that made it, staff with the support grant and a
// reason settle or release another holder's, and a suspended tenant may
// still settle, release and extend its holds and release its leases, but
// never reserve or consume.
func TestHoldsBelongToTheirHolderOverHTTP(t *testing.T) {
	a := newExtensionsAPI(t)
	ctx := context.Background()
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-ext-holder")
	owner := a.person(b, "owner@example.test")
	admin := a.person(b, "admin@example.test")
	support := a.person(b, "support@example.test")
	operator := a.person(b, "operator@example.test")
	grantStaff(t, a.db, b, support, identity.StaffSupport)
	grantStaff(t, a.db, b, operator, identity.StaffOperator)
	tok := a.token(owner, mfa())
	supportTok := a.token(support, enginetest.TokenOptions{Staff: true, MFA: true})
	opTok := a.token(operator, enginetest.TokenOptions{Staff: true, MFA: true})
	id := a.createTenant(owner, "Holder Co")
	a.join(id, owner, admin, "admin")
	adminTok := a.token(admin, mfa())
	tenantID, _ := idgen.ParseAs(idgen.Tenant, id)
	sc := scope.Scope{Brand: b.ID, Tenant: tenantID}
	base := "/v1/tenants/" + id
	staffBase := "/v1/staff/tenants/" + id
	brandPath := "/v1/brands/" + idgen.Format(idgen.Brand, b.ID)
	if r := a.call(http.MethodPut, brandPath+"/quotas", opTok, map[string]any{"quotas": []map[string]any{{"feature": "jobs", "plan": "*", "limit": 5, "window": "concurrent"}}}); r.status != http.StatusOK {
		t.Fatalf("quotas = %d %v", r.status, r.body)
	}
	if _, _, err := a.bill.GrantCredit(ctx, sc, billing.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:staff"}, Reason: "test", CorrelationID: "c"},
		billing.StaffCredit{AmountMinor: 1000, Currency: "CAD", IdempotencyKey: "holder-credit"}); err != nil {
		t.Fatal(err)
	}
	acct, _ := a.fake.Account(b.Slug, id)
	if _, err := a.fake.KillBill(t).CreateSubscription(ctx, killbill.TenantCredentials{APIKey: b.Slug, APISecret: "kb-secret-" + b.Slug},
		killbill.CallContext{CreatedBy: "test"}, acct.ID, enginetest.PlanBasic, "holder-basic"); err != nil {
		t.Fatal(err)
	}
	// An admin manages members but holds no workload permission.
	for path, body := range map[string]map[string]any{
		"/reservations":              {"external_ref": "admin-job", "amount_minor": 10},
		"/entitlements/jobs/consume": {"ref": "admin-job"},
		"/usage":                     {"meter": "api-calls", "event_id": "a", "quantity_micro": 1},
	} {
		if r := a.call(http.MethodPost, base+path, adminTok, body); r.status != http.StatusForbidden || r.code() != "permission_denied" {
			t.Fatalf("admin on %s = %d %v", path, r.status, r.body)
		}
	}
	key := a.call(http.MethodPost, base+"/api-keys", tok, map[string]any{"name": "scheduler", "permissions": []string{"wallet:reserve", "quota:consume"}})
	if key.status != http.StatusCreated {
		t.Fatalf("api key = %d %v", key.status, key.body)
	}
	keyTok := key.str("secret")
	for _, ref := range []string{"job-1", "job-2", "job-3", "job-4"} {
		if r := a.call(http.MethodPost, base+"/reservations", keyTok, map[string]any{"external_ref": ref, "amount_minor": 100}); r.status != http.StatusCreated {
			t.Fatalf("key reserves %s = %d %v", ref, r.status, r.body)
		}
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/consume", keyTok, map[string]any{"ref": "lease-1"}); r.status != http.StatusOK {
		t.Fatalf("key consumes = %d %v", r.status, r.body)
	}
	// The owner holds wallet:reserve, not the key's holds.
	for path, body := range map[string]map[string]any{
		"/reservations/job-1/settle":  {"credit_minor": 10},
		"/reservations/job-1/release": {},
		"/reservations/job-1/extend":  {"lease_seconds": 600},
	} {
		if r := a.call(http.MethodPost, base+path, tok, body); r.status != http.StatusNotFound || r.code() != "reservation_not_found" {
			t.Fatalf("owner on %s = %d %v", path, r.status, r.body)
		}
	}
	if r := a.call(http.MethodPost, base+"/reservations", tok, map[string]any{"external_ref": "job-1", "amount_minor": 500}); r.status != http.StatusNotFound || r.code() != "reservation_not_found" {
		t.Fatalf("owner grows the key's hold = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/release", tok, map[string]any{"ref": "lease-1"}); r.status != http.StatusNotFound {
		t.Fatalf("owner releases the key's lease = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations/job-1/settle", keyTok, map[string]any{"credit_minor": 40}); r.status != http.StatusOK || r.str("status") != "settled" {
		t.Fatalf("key settles = %d %v", r.status, r.body)
	}
	// Staff: the support grant and a reason.
	if r := a.call(http.MethodPost, staffBase+"/reservations/job-2/release", opTok, map[string]any{"reason": "stuck"}); r.status != http.StatusNotFound {
		t.Fatalf("operator releases = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, staffBase+"/reservations/job-2/release", supportTok, map[string]any{"reason": "stuck job"}); r.status != http.StatusOK || r.str("status") != "released" {
		t.Fatalf("support releases = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, staffBase+"/reservations/job-3/settle", supportTok, map[string]any{"credit_minor": 30, "reason": "finished by hand"}); r.status != http.StatusOK || r.str("status") != "settled" {
		t.Fatalf("support settles = %d %v", r.status, r.body)
	}
	if n := countRows(t, a, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'wallet.reservation.staff_override'`, tenantID); n != 2 {
		t.Fatalf("staff override audits = %d", n)
	}
	// Suspended: only what gives back goes through.
	if _, err := tenant.Suspend(ctx, a.db.Pool, a.eng, sc, events.Actor{Type: events.ActorService, ID: "ops"}, "unpaid", "test"); err != nil {
		t.Fatal(err)
	}
	if r := a.call(http.MethodPost, base+"/reservations", keyTok, map[string]any{"external_ref": "job-5", "amount_minor": 10}); r.status != http.StatusNotFound {
		t.Fatalf("reserve while suspended = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations", keyTok, map[string]any{"external_ref": "job-4", "amount_minor": 200}); r.status != http.StatusNotFound {
		t.Fatalf("grow while suspended = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/consume", keyTok, map[string]any{"ref": "lease-2"}); r.status != http.StatusNotFound {
		t.Fatalf("consume while suspended = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations/job-4/extend", keyTok, map[string]any{"lease_seconds": 600}); r.status != http.StatusOK {
		t.Fatalf("extend while suspended = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/reservations/job-4/settle", keyTok, map[string]any{"credit_minor": 20}); r.status != http.StatusOK || r.str("status") != "settled" {
		t.Fatalf("settle while suspended = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/entitlements/jobs/release", keyTok, map[string]any{"ref": "lease-1"}); r.status != http.StatusOK || r.body["released"] != float64(1) {
		t.Fatalf("release a lease while suspended = %d %v", r.status, r.body)
	}
}
