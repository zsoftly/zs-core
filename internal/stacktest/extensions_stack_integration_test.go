//go:build integration

package stacktest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/quota"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

// The metered plan of brands/zs-reference/catalogue.xml: no trial, api-call
// at 0.01 CAD per call in arrears.
const refPlanMetered = "core-metered-monthly"

// kbUsage reads Kill Bill's rolled-up api-call usage of a subscription
// between two days, in micro-units.
func kbUsage(t *testing.T, creds killbill.TenantCredentials, sub string, from, to time.Time) int64 {
	t.Helper()
	q := url.Values{"startDate": {from.UTC().Format(time.DateOnly)}, "endDate": {to.UTC().Format(time.DateOnly)}}
	status, _, body := kbRaw(t, creds, http.MethodGet, "/1.0/kb/usages/"+sub+"?"+q.Encode(), nil)
	if status != http.StatusOK {
		t.Fatalf("Kill Bill usage = %d %s", status, body)
	}
	var got killbill.RolledUpUsage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, u := range got.RolledUpUnits {
		if u.UnitType == "api-call" {
			v, err := billing.DecimalToMicro(u.Amount)
			if err != nil {
				t.Fatal(err)
			}
			total += v
		}
	}
	return total
}

// kbCredit reads the account credit from Kill Bill, in minor units.
func kbCredit(t *testing.T, creds killbill.TenantCredentials, accountID string) int64 {
	t.Helper()
	status, _, body := kbRaw(t, creds, http.MethodGet, "/1.0/kb/accounts/"+accountID+"?accountWithBalanceAndCBA=true", nil)
	if status != http.StatusOK {
		t.Fatalf("Kill Bill account = %d %s", status, body)
	}
	var a killbill.Account
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	v, err := billing.ToMinor(a.AccountCBA, "CAD")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The Phase 4b extensions on Kill Bill 0.24.22: meters and quotas applied
// from the manifest, usage forwarded as usage records, a reservation
// settled into usage and into a charge, a settlement queued while Kill Bill
// is unreachable and completed by the relay, the wallet view against Kill
// Bill's credit, the reconciliation sweep, and an invoice run that bills
// the usage.
func TestExtensionsOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()
	clock := &stackClock{}
	svc := &billing.Service{DB: db.Pool, KillBill: s.kb, Secrets: secrets.Default{}, Now: clock.Now}
	meter := &metering.Service{DB: db.Pool, Billing: svc, Now: clock.Now}
	wal := &wallet.Service{DB: db.Pool, Billing: svc, Metering: meter, CreditTTL: 2 * time.Second, Now: clock.Now}
	quo := &quota.Service{DB: db.Pool, Billing: svc, Now: clock.Now}
	engines := tenant.Engines{Keycloak: s.kc, KillBill: s.kb, Secrets: secrets.Default{}}
	reg := brand.NewRegistry(db.Pool, time.Second, nil)
	srv := httptest.NewServer(httpapi.New(httpapi.Options{DB: db.Pool, Migrations: migrations.FS, Brands: reg, Tenants: engines,
		Billing: svc, Wallet: wal, Metering: meter, Quota: quo,
		Verifier: &identity.Verifier{Brands: reg, Keys: keycloak.NewKeySet(s.kcURL, keycloak.KeySetOptions{})}}))
	t.Cleanup(srv.Close)
	api := apiServer{srv}

	// The manifest's meters and quotas are applied, and a second apply
	// changes nothing.
	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	first := apply(t, deps, devVariant(t, ref))
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), s.deps(t, db), ref, cli) })
	refID, _ := idgen.ParseAs(idgen.Brand, first.BrandID)
	if again := apply(t, deps, devVariant(t, ref)); len(again.Changes) != 0 {
		t.Fatalf("second apply = %s", mustJSON(again))
	}
	meters, err := brand.ListMeters(ctx, db.Pool, refID)
	if err != nil || len(meters) != 1 || meters[0].Slug != "api-calls" || meters[0].Status != brand.MeterActive {
		t.Fatalf("meters = %+v %v", meters, err)
	}
	quotas, err := brand.ListQuotas(ctx, db.Pool, refID, "")
	if err != nil || len(quotas) != 2 {
		t.Fatalf("quotas = %+v %v", quotas, err)
	}
	bad := clone(t, devVariant(t, ref))
	(*bad.Billing.Meters)[0].KillBillUnit = "gpu-hour"
	if _, err := brand.Apply(ctx, deps, bad, cli); errName(err) != "brand_manifest_invalid" {
		t.Fatalf("meter on a unit outside the catalogue: %v", err)
	}

	// A tenant created through Core, with Core credit and a subscription on
	// the metered plan.
	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	owner := "ext-owner-" + suffix
	createUser(t, s.kc, "zs-reference", owner, "pw-"+suffix)
	tok := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", owner, "pw-"+suffix)
	status, out := api.call(t, http.MethodPost, "/v1/tenants", tok.access, jsonBody(t, map[string]any{"idempotency_key": "ext-" + suffix, "name": "Extensions " + suffix}), "application/json")
	if status != http.StatusCreated {
		t.Fatalf("create tenant = %d %s", status, mustJSON(out))
	}
	tenantID, _ := idgen.ParseAs(idgen.Tenant, out["id"].(string))
	a, err := tenant.Get(ctx, db.Pool, scope.Scope{Brand: refID, Tenant: tenantID})
	if err != nil {
		t.Fatal(err)
	}
	base := "/v1/tenants/" + a.PublicID()
	refCreds := killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)}
	ident, err := identity.Get(ctx, db.Pool, refID, discoveredIssuer(t, s.kcURL, "zs-reference"), tok.subject)
	if err != nil {
		t.Fatal(err)
	}
	call := billing.Call{Actor: events.Actor{Type: events.ActorUser, ID: ident.PublicID()}, CorrelationID: "stack-ext-" + suffix}
	staff := billing.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:stack-staff"}, Reason: "stack test", CorrelationID: "stack-ext-staff-" + suffix}
	if _, _, err := svc.GrantCredit(ctx, a.Scope(), staff, billing.StaffCredit{AmountMinor: 2000, Currency: "CAD", IdempotencyKey: "stack-ext-credit-" + suffix}); err != nil {
		t.Fatal(err)
	}
	sub, _, err := svc.CreateSubscription(ctx, a.Scope(), call, refPlanMetered, "stack-ext-sub-"+suffix)
	if err != nil || sub.State != "ACTIVE" || sub.Phase != "EVERGREEN" {
		t.Fatalf("subscribe = %+v %v", sub, err)
	}
	day := clock.Now().Truncate(24 * time.Hour)
	from, to := day.AddDate(0, 0, -1), day.AddDate(0, 0, 2)

	// Usage over HTTP, forwarded as one usage record with a deterministic
	// tracking ID.
	for i, n := range []int64{1, 2, 3} {
		ev := map[string]any{"meter": "api-calls", "event_id": fmt.Sprintf("stack-%s-%d", suffix, i), "quantity_micro": n * 1_000_000,
			"dimensions": map[string]string{"region": "yow"}}
		if status, out := api.call(t, http.MethodPost, base+"/usage", tok.access, jsonBody(t, ev), "application/json"); status != http.StatusCreated {
			t.Fatalf("usage = %d %s", status, mustJSON(out))
		}
	}
	report, err := meter.Forward(ctx)
	if err != nil || report.Forwarded < 1 || report.Failed != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	if got := kbUsage(t, refCreds, sub.ID, from, to); got != 6_000_000 {
		t.Fatalf("Kill Bill usage = %d", got)
	}
	var tracking string
	if err := db.Pool.QueryRow(ctx, `SELECT tracking_id FROM usage_rollups WHERE tenant_id = $1`, a.ID).Scan(&tracking); err != nil {
		t.Fatal(err)
	}
	// A forced retry of the rollup: Kill Bill holds the tracking ID and
	// records nothing more.
	if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'pending', forwarded_at = NULL WHERE tenant_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if report, err := meter.Forward(ctx); err != nil || report.Duplicates != 1 {
		t.Fatalf("forced retry = %+v %v", report, err)
	}
	if got := kbUsage(t, refCreds, sub.ID, from, to); got != 6_000_000 {
		t.Fatalf("Kill Bill usage after a retry = %d", got)
	}

	// A reservation settled into usage, and one settled into a charge.
	reserve := func(ref string, amount int64) {
		t.Helper()
		status, out := api.call(t, http.MethodPost, base+"/reservations", tok.access, jsonBody(t, map[string]any{"external_ref": ref, "amount_minor": amount}), "application/json")
		if status != http.StatusCreated {
			t.Fatalf("reserve %s = %d %s", ref, status, mustJSON(out))
		}
	}
	reserve("run-"+suffix, 500)
	status, out = api.call(t, http.MethodPost, base+"/reservations/run-"+suffix+"/settle", tok.access,
		jsonBody(t, map[string]any{"usage": map[string]any{"subscription": sub.ID, "unit": "api-call", "quantity_micro": 120_000_000}}), "application/json")
	if status != http.StatusOK || out["killbill_ref_type"] != "usage_record" || out["settled_minor"] != float64(120) || out["rated_by"] != "catalogue_price" {
		t.Fatalf("usage settlement = %d %s", status, mustJSON(out))
	}
	usageTracking := out["killbill_ref"].(string)
	if got := kbUsage(t, refCreds, sub.ID, from, to); got != 126_000_000 {
		t.Fatalf("Kill Bill usage after the settlement = %d", got)
	}
	reserve("charge-"+suffix, 300)
	status, out = api.call(t, http.MethodPost, base+"/reservations/charge-"+suffix+"/settle", tok.access, jsonBody(t, map[string]any{"credit_minor": 200}), "application/json")
	if status != http.StatusOK || out["killbill_ref_type"] != "external_charge" {
		t.Fatalf("charge settlement = %d %s", status, mustJSON(out))
	}
	if got := kbCredit(t, refCreds, a.KillBillAccountID); got != 1800 {
		t.Fatalf("Kill Bill credit after the charge = %d", got)
	}

	// A settlement while Kill Bill cannot be reached is queued, the hold
	// keeps counting, and the relay completes it against the real Kill Bill.
	reserve("queued-"+suffix, 100)
	dead, err := killbill.New(killbill.Config{URL: "http://127.0.0.1:1", AdminUser: "admin", AdminPassword: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	offline := &wallet.Service{DB: db.Pool, Billing: &billing.Service{DB: db.Pool, KillBill: dead, Secrets: secrets.Default{}}, Metering: meter}
	// The hold belongs to the owner's membership, which made it over HTTP.
	m, err := tenant.MembershipOf(ctx, db.Pool, a.Scope(), ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	holder := wallet.Call{Actor: call.Actor, CorrelationID: call.CorrelationID, Holder: "membership:" + m.PublicID()}
	var pending wallet.Reservation
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		var err error
		pending, err = offline.Settle(ctx, tx, a.Scope(), holder, "queued-"+suffix, 0, wallet.Settlement{CreditMinor: 60})
		return err
	}); err != nil || !pending.SettlementPending {
		t.Fatalf("settle with Kill Bill unreachable = %+v %v", pending, err)
	}
	status, out = api.call(t, http.MethodGet, base+"/wallet", tok.access, nil, "")
	if status != http.StatusOK || out["reserved_minor"] != float64(100) || out["pending_settlements"] != float64(1) || out["killbill_credit_minor"] != float64(1800) {
		t.Fatalf("wallet with a pending settlement = %d %s", status, mustJSON(out))
	}
	relay := outbox.NewRelay("stack", outbox.NewPostgresReader(db.Pool).WithEventTypes(wallet.EventSettlementRequested), wal.RelayHandler(), 0, 0, nil)
	if n, err := relay.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("relay = %d %v", n, err)
	}
	done, err := wallet.Get(ctx, db.Pool, a.Scope(), "queued-"+suffix, 0)
	if err != nil || done.Status != wallet.StatusSettled || done.SettledMinor == nil || *done.SettledMinor != 60 {
		t.Fatalf("after the relay = %+v %v", done, err)
	}

	// The wallet view matches Kill Bill: its credit less the holds.
	reserve("held-"+suffix, 150)
	credit := kbCredit(t, refCreds, a.KillBillAccountID)
	status, out = api.call(t, http.MethodGet, base+"/wallet", tok.access, nil, "")
	// Kill Bill's dry run rates the 126 calls not yet invoiced at 1.26 CAD,
	// and the wallet subtracts them.
	if status != http.StatusOK || credit != 1740 || out["killbill_credit_minor"] != float64(credit) || out["reserved_minor"] != float64(150) ||
		out["unbilled_minor"] != float64(126) || out["available_minor"] != float64(credit-150-126) || out["currency"] != "CAD" {
		t.Fatalf("wallet = %d %s, Kill Bill credit %d", status, mustJSON(out), credit)
	}
	// Reservations cannot pass the credit less the unbilled usage.
	if status, out := api.call(t, http.MethodPost, base+"/reservations", tok.access, jsonBody(t, map[string]any{"external_ref": "edge-" + suffix,
		"amount_minor": credit - 150 - 126 + 1}), "application/json"); status != http.StatusConflict || out["error"].(map[string]any)["code"] != "insufficient_credit" {
		t.Fatalf("reserve past credit less unbilled = %d %s", status, mustJSON(out))
	}
	// The hold expires with its lease.
	clock.advance(time.Hour)
	if r, err := (&wallet.Sweeper{Service: wal, ReconcileEvery: time.Nanosecond}).SweepOnce(ctx); err != nil || r.Expired != 1 || r.Mismatches != 0 {
		t.Fatalf("sweep = %+v %v", r, err)
	}
	// A credit added behind Core's back is one mismatch.
	if status, _, body := kbRaw(t, refCreds, http.MethodPost, "/1.0/kb/credits?autoCommit=true",
		[]map[string]any{{"accountId": a.KillBillAccountID, "amount": 7.77, "currency": "CAD", "description": "added outside Core"}}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("credit outside Core = %d %s", status, body)
	}
	if found, err := wal.Reconcile(ctx, a.Scope()); err != nil || found != 1 {
		t.Fatalf("reconcile = %d %v", found, err)
	}
	if found, err := wal.Reconcile(ctx, a.Scope()); err != nil || found != 0 {
		t.Fatalf("reconcile again = %d %v", found, err)
	}

	// Entitlement with the month quota, and Kill Bill's billed usage over
	// HTTP.
	status, out = api.call(t, http.MethodGet, base+"/entitlements/api-call", tok.access, nil, "")
	if status != http.StatusOK || out["allowed"] != true || out["limit"] != float64(100000) || out["used"] != float64(126) || out["plan"] != refPlanMetered {
		t.Fatalf("entitlement = %d %s", status, mustJSON(out))
	}
	status, out = api.call(t, http.MethodGet, base+"/usage/billed?subscription="+sub.ID, tok.access, nil, "")
	data, _ := out["data"].([]any)
	if status != http.StatusOK || len(data) != 1 {
		t.Fatalf("billed = %d %s", status, mustJSON(out))
	}
	if units, _ := data[0].(map[string]any)["units"].([]any); len(units) != 1 || units[0].(map[string]any)["unit"] != "api-call" ||
		units[0].(map[string]any)["quantity_micro"] != float64(126_000_000) {
		t.Fatalf("billed units = %s", mustJSON(data[0]))
	}
	status, out = api.call(t, http.MethodPost, base+"/entitlements/concurrent-jobs/consume", tok.access, jsonBody(t, map[string]any{"ref": "job-1"}), "application/json")
	if status != http.StatusOK || out["used"] != float64(1) {
		t.Fatalf("consume = %d %s", status, mustJSON(out))
	}

	// An invoice run past the end of the first period bills the usage.
	// Kill Bill's test clock cannot move on this stack, so the run names a
	// target date instead.
	if status, _, body := kbRaw(t, refCreds, http.MethodGet, "/1.0/kb/test/clock", nil); status == http.StatusOK {
		t.Logf("Kill Bill clock: %s", body)
	}
	target := day.AddDate(0, 0, 32).Format(time.DateOnly)
	if status, _, body := kbRaw(t, refCreds, http.MethodPost, "/1.0/kb/invoices?accountId="+a.KillBillAccountID+"&targetDate="+target, nil); status != http.StatusCreated {
		t.Fatalf("invoice run = %d %s", status, body)
	}
	status, _, body := kbRaw(t, refCreds, http.MethodGet, "/1.0/kb/accounts/"+a.KillBillAccountID+"/invoices?withItems=true&includeInvoiceComponents=true", nil)
	if status != http.StatusOK {
		t.Fatalf("invoices = %d %s", status, body)
	}
	var invoices []struct {
		TrackingIDs []string               `json:"trackingIds"`
		Items       []killbill.InvoiceItem `json:"items"`
	}
	if err := json.Unmarshal(body, &invoices); err != nil {
		t.Fatal(err)
	}
	var billed int64
	var ids []string
	for _, in := range invoices {
		for _, it := range in.Items {
			if it.ItemType == "USAGE" && it.PlanName == refPlanMetered {
				v, _ := billing.ToMinor(it.Amount, "CAD")
				billed += v
				ids = append(ids, in.TrackingIDs...)
			}
		}
	}
	if billed != 126 || !strings.Contains(strings.Join(ids, ","), tracking) || !strings.Contains(strings.Join(ids, ","), usageTracking) {
		t.Fatalf("usage invoiced = %d minor, tracking IDs %v", billed, ids)
	}
	t.Logf("usage invoiced: %d minor units under tracking IDs %v", billed, ids)
	// The client reads the tracking IDs the reconciliation compares, and
	// the dates of the usage item, and the invoiced usage reconciles.
	read, err := s.kb.AccountInvoices(ctx, refCreds, a.KillBillAccountID, true)
	if err != nil {
		t.Fatal(err)
	}
	var readIDs []string
	dated := false
	for _, in := range read {
		readIDs = append(readIDs, in.TrackingIDs...)
		for _, it := range in.Items {
			if it.ItemType == "USAGE" && it.StartDate != "" && it.EndDate != "" && it.SubscriptionID == sub.ID {
				dated = true
			}
		}
	}
	if !strings.Contains(strings.Join(readIDs, ","), tracking) || !strings.Contains(strings.Join(readIDs, ","), usageTracking) || !dated {
		t.Fatalf("tracking IDs read by the client = %v, usage item dated %v", readIDs, dated)
	}
	if found, err := wal.Reconcile(ctx, a.Scope()); err != nil || found != 0 {
		t.Fatalf("reconcile after the invoice run = %d %v", found, err)
	}
}
