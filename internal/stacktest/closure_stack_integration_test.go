//go:build integration

package stacktest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/doctor"
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
	"github.com/zsoftly/zs-core/pkg/quota"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

// invoiceCalls logs the status of every invoice call the closure makes to Kill
// Bill, with the time since the first one, so that a closure that ends in
// manual review shows what Kill Bill answered.
type invoiceCalls struct {
	t    *testing.T
	next http.RoundTripper
	// triggerNotFound counts the invoice triggers Kill Bill answered with 404.
	triggerNotFound *atomic.Int64
}

func (l invoiceCalls) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := l.next.RoundTrip(r)
	if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/1.0/kb/invoices") || r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/subscriptions/") {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		if status == http.StatusNotFound && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/1.0/kb/invoices") {
			l.triggerNotFound.Add(1)
		}
		l.t.Logf("%s %s -> %d %v (%s) at %s", r.Method, r.URL.Path, status, err, time.Since(start).Round(time.Millisecond), start.UTC().Format("15:04:05.000"))
	}
	return resp, err
}

// closureStack is a tenant of the reference brand with a usage-only
// subscription three days old and six calls of usage that are not yet with
// Kill Bill. The subscription is three days old because Kill Bill bills the
// usage of a subscription that is created and cancelled on one UTC day in a
// window of zero length, and so never bills it.
type closureStack struct {
	s       stack
	db      *postgrestest.DB
	engines tenant.Engines
	tenant  tenant.Tenant
	creds   killbill.TenantCredentials
	subID   string
	// from and to bound the days Kill Bill's usage is read over.
	from, to time.Time
	today    time.Time
	// triggerNotFound counts the invoice triggers Kill Bill answered with 404.
	triggerNotFound *atomic.Int64
}

func newClosureStack(t *testing.T) closureStack {
	t.Helper()
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()
	svc := &billing.Service{DB: db.Pool, KillBill: s.kb, Secrets: secrets.Default{}}
	meter := &metering.Service{DB: db.Pool, Billing: svc}
	wal := &wallet.Service{DB: db.Pool, Billing: svc, Metering: meter}
	quo := &quota.Service{DB: db.Pool, Billing: svc}
	var notFound atomic.Int64
	logged, err := killbill.New(killbill.Config{URL: os.Getenv("ZS_TEST_KILLBILL_URL"), AdminUser: "admin", AdminPassword: os.Getenv("ZS_TEST_KILLBILL_ADMIN_PASSWORD"),
		HTTPClient: &http.Client{Transport: invoiceCalls{t, http.DefaultTransport, &notFound}}})
	if err != nil {
		t.Fatal(err)
	}
	engines := tenant.Engines{Keycloak: s.kc, KillBill: logged, Secrets: secrets.Default{}}
	reg := brand.NewRegistry(db.Pool, time.Second, nil)
	srv := httptest.NewServer(httpapi.New(httpapi.Options{DB: db.Pool, Migrations: migrations.FS, Brands: reg, Tenants: engines,
		Billing: svc, Wallet: wal, Metering: meter, Quota: quo,
		Verifier: &identity.Verifier{Brands: reg, Keys: keycloak.NewKeySet(s.kcURL, keycloak.KeySetOptions{})}}))
	t.Cleanup(srv.Close)
	api := apiServer{srv}

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	first := apply(t, deps, devVariant(t, ref))
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), s.deps(t, db), ref, cli) })
	refID, _ := idgen.ParseAs(idgen.Brand, first.BrandID)

	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	owner := "closure-owner-" + suffix
	createUser(t, s.kc, "zs-reference", owner, "pw-"+suffix)
	tok := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", owner, "pw-"+suffix)
	status, out := api.call(t, http.MethodPost, "/v1/tenants", tok.access, jsonBody(t, map[string]any{"idempotency_key": "closure-" + suffix, "name": "Closure " + suffix}), "application/json")
	if status != http.StatusCreated {
		t.Fatalf("create tenant = %d %s", status, mustJSON(out))
	}
	tenantID, _ := idgen.ParseAs(idgen.Tenant, out["id"].(string))
	tn, err := tenant.Get(ctx, db.Pool, scope.Scope{Brand: refID, Tenant: tenantID})
	if err != nil || tn.KillBillAccountID == "" {
		t.Fatalf("tenant = %+v %v", tn, err)
	}
	creds := killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)}

	// The subscription is made straight on Kill Bill, because Core's subscribe
	// call starts one today.
	started := time.Now().UTC().AddDate(0, 0, -3).Format(time.DateOnly)
	status, hdr, body := kbRaw(t, creds, http.MethodPost, "/1.0/kb/subscriptions?entitlementDate="+started+"&billingDate="+started,
		map[string]any{"accountId": tn.KillBillAccountID, "planName": refPlanMetered, "externalKey": "stack-closure-" + suffix})
	if status != http.StatusCreated {
		t.Fatalf("backdated subscription = %d %s", status, body)
	}
	location := hdr.Get("Location")
	subID := location[strings.LastIndex(location, "/")+1:]

	// Six calls through Core, none sent to Kill Bill yet: the drain of the
	// closure sends them.
	for i, n := range []int64{1, 2, 3} {
		ev := map[string]any{"meter": "api-calls", "event_id": "closure-" + suffix + "-" + string(rune('a'+i)), "quantity_micro": n * 1_000_000}
		if status, out := api.call(t, http.MethodPost, "/v1/tenants/"+tn.PublicID()+"/usage", tok.access, jsonBody(t, ev), "application/json"); status != http.StatusCreated {
			t.Fatalf("usage = %d %s", status, mustJSON(out))
		}
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	c := closureStack{s: s, db: db, engines: engines, tenant: tn, creds: creds, subID: subID, from: today.AddDate(0, 0, -1), to: today.AddDate(0, 0, 2), today: today, triggerNotFound: &notFound}
	if got := kbUsage(t, creds, subID, c.from, c.to); got != 0 {
		t.Fatalf("Kill Bill holds %d micro-units before the closure", got)
	}
	return c
}

// start has staff start the closure of the tenant.
func (c closureStack) start(t *testing.T) {
	t.Helper()
	actor := events.Actor{Type: events.ActorStaff, ID: "sub:stack-staff"}
	id := "stack-closure-" + c.tenant.PublicID()
	if _, err := tenant.StartClosure(context.Background(), c.db.Pool, c.tenant.Scope(), actor, tenant.ClosureRequest{Reason: "stack closure", RequestID: id}, id); err != nil {
		t.Fatal(err)
	}
}

// settle is how long the closure waits after the cancel before finance runs.
// Kill Bill makes the invoice of a cancel about three and a half seconds after
// the cancel. The sweeper moves from the cancel to the finance step in one
// pass, so its dry run and its trigger come within about a second of the
// cancel, and when Kill Bill's own invoice lands between them the trigger
// answers a bare 404. The finance step settles that: see
// TestClosureFinancesRightAfterTheCancelOnTheStack. The tests that use settle
// do the cancel first and let Kill Bill invoice it, so that they stay
// deterministic and do not depend on that race.
const settle = 8 * time.Second

// cancelFirst runs the cancelling step, with its drain, until it reports the
// subscriptions cancelled and the usage drained, and then waits for Kill Bill
// to make the invoice of the cancel.
func (c closureStack) cancelFirst(t *testing.T, steps tenant.ClosureSteps) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 10; attempt++ {
		prepared, err := steps.Prepare(ctx, c.db.Pool, c.tenant.Scope())
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if prepared.Cancelled && prepared.Drained {
			time.Sleep(settle)
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatal("the cancelling step did not finish")
}

// sweep runs the tenant sweeper, with the closure steps given, until the
// tenant is closed or the passes are used. A pass makes the settlement due
// first, because a step that waits is looked at again only after five
// minutes. It returns whether the tenant is closed.
func (c closureStack) sweep(t *testing.T, steps tenant.ClosureSteps, passes int) bool {
	t.Helper()
	ctx := context.Background()
	sweeper := &tenant.Sweeper{DB: c.db.Pool, Engines: c.engines, Closure: steps, Log: slog.New(slog.NewTextHandler(testLog{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	for pass := 0; pass < passes; pass++ {
		if _, err := c.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET next_attempt_at = now() - interval '1 second' WHERE tenant_id = $1`, c.tenant.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := sweeper.SweepOnce(ctx); err != nil {
			t.Fatalf("sweep %d: %v", pass, err)
		}
		cur, err := tenant.Get(ctx, c.db.Pool, c.tenant.Scope())
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status == tenant.StatusClosed {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// testLog writes the sweeper's log lines to the test log.
type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// stuck explains a closure that did not complete.
func (c closureStack) stuck(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var state, lastErr string
	_ = c.db.Pool.QueryRow(ctx, `SELECT status, coalesce(last_error, '') FROM tenant_closure_settlements WHERE tenant_id = $1`, c.tenant.ID).Scan(&state, &lastErr)
	var kinds []string
	rows, _ := c.db.Pool.Query(ctx, `SELECT o.kind FROM tenant_closure_invoice_outcomes o JOIN tenant_closure_invoice_attempts a ON a.id = o.attempt_id WHERE a.tenant_id = $1 ORDER BY o.id`, c.tenant.ID)
	for rows != nil && rows.Next() {
		var k string
		_ = rows.Scan(&k)
		kinds = append(kinds, k)
	}
	var reviewsOpen int
	_ = c.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_review_items i LEFT JOIN tenant_closure_review_resolutions r ON r.item_id = i.id WHERE i.tenant_id = $1 AND r.item_id IS NULL`, c.tenant.ID).Scan(&reviewsOpen)
	_, _, inv := kbRaw(t, c.creds, http.MethodGet, "/1.0/kb/accounts/"+c.tenant.KillBillAccountID+"/invoices?includeInvoiceComponents=true", nil)
	t.Fatalf("the closure did not complete: settlement at %s, last error %q, invoice outcomes %v, open reviews %d, Kill Bill invoices %s", state, lastErr, kinds, reviewsOpen, inv)
}

// invoices reads the account's invoices from Kill Bill with their items and
// tracking IDs.
func (c closureStack) invoices(t *testing.T) []killbill.Invoice {
	t.Helper()
	status, _, body := kbRaw(t, c.creds, http.MethodGet, "/1.0/kb/accounts/"+c.tenant.KillBillAccountID+"/invoices?includeInvoiceComponents=true", nil)
	var invoices []killbill.Invoice
	if status != http.StatusOK || json.Unmarshal(body, &invoices) != nil {
		t.Fatalf("invoices = %d %s", status, body)
	}
	return invoices
}

// carrying returns the invoice that lists the tracking ID, or the zero value.
func (c closureStack) carrying(t *testing.T, tracking string) killbill.Invoice {
	t.Helper()
	for _, in := range c.invoices(t) {
		if slices.Contains(in.TrackingIDs, tracking) {
			return in
		}
	}
	return killbill.Invoice{}
}

// A tenant is closed on Kill Bill 0.24.22: its usage is sent to Kill Bill by
// the drain, the subscription is cancelled IMMEDIATELY, the cancel's repair
// invoice bills the usage and lists its tracking ID, the sweeper invoices what
// is left and blocks the account with ZS_TENANT_CLOSED, leaving billing open,
// and the final invoice can still be paid. The account is blocked and never
// closed. The tenant sweeper drives it with tenant.EngineClosureSteps and the
// metering service as the drain.
func TestClosureOnTheStack(t *testing.T) {
	c := newClosureStack(t)
	ctx := context.Background()
	meter := &metering.Service{DB: c.db.Pool, Billing: &billing.Service{DB: c.db.Pool, KillBill: c.s.kb, Secrets: secrets.Default{}}}
	steps := tenant.EngineClosureSteps{Engines: c.engines, Drain: meter}
	c.start(t)
	c.cancelFirst(t, steps)
	if !c.sweep(t, steps, 15) {
		c.stuck(t)
	}

	// The usage reached Kill Bill once, from the drain.
	if got := kbUsage(t, c.creds, c.subID, c.from, c.to); got != 6_000_000 {
		t.Fatalf("Kill Bill holds %d micro-units after the closure, want 6000000", got)
	}
	var tracking, rollupStatus string
	if err := c.db.Pool.QueryRow(ctx, `SELECT tracking_id, status FROM usage_rollups WHERE tenant_id = $1`, c.tenant.ID).Scan(&tracking, &rollupStatus); err != nil || rollupStatus != "forwarded" {
		t.Fatalf("rollup = %q %q %v, want one forwarded rollup", tracking, rollupStatus, err)
	}

	// The subscription is cancelled today.
	status, _, body := kbRaw(t, c.creds, http.MethodGet, "/1.0/kb/subscriptions/"+c.subID, nil)
	var sub killbill.Subscription
	if status != http.StatusOK || json.Unmarshal(body, &sub) != nil || sub.State != "CANCELLED" {
		t.Fatalf("subscription = %d %s", status, body)
	}
	if cancelled, err := killbill.CancelEffectiveDate(sub); err != nil || cancelled != c.today.Format(time.DateOnly) {
		t.Fatalf("cancelled on %q %v, want %s", cancelled, err, c.today.Format(time.DateOnly))
	}

	// The cancel's invoice bills the usage and lists its tracking ID.
	usageInvoice := c.carrying(t, tracking)
	if usageInvoice.InvoiceID == "" || usageInvoice.Status != "COMMITTED" {
		t.Fatalf("no committed invoice carries %s: %s", tracking, mustJSON(c.invoices(t)))
	}
	var usageMinor int64
	for _, item := range usageInvoice.Items {
		if item.ItemType == "USAGE" && item.SubscriptionID == c.subID {
			v, err := billing.ToMinor(item.Amount, usageInvoice.Currency)
			if err != nil {
				t.Fatal(err)
			}
			usageMinor += v
		}
	}
	if usageMinor != 6 {
		t.Fatalf("the usage invoice bills %d minor units of usage, want 6: %s", usageMinor, mustJSON(usageInvoice))
	}

	// The account is blocked with billing open, never closed: it takes no new
	// subscription.
	if states := blockingStates(t, c.s, c.creds, c.tenant.KillBillAccountID); !slices.Equal(states, []string{tenant.BlockingClosed + ":false"}) {
		t.Fatalf("blocking states = %v, want only %s with billing open", states, tenant.BlockingClosed)
	}
	if status, _, body := kbRaw(t, c.creds, http.MethodPost, "/1.0/kb/subscriptions",
		map[string]any{"accountId": c.tenant.KillBillAccountID, "planName": refPlanMetered, "externalKey": "stack-closure-late-" + c.tenant.PublicID()}); status == http.StatusCreated {
		t.Fatalf("a closed tenant's account took a new subscription: %s", body)
	}

	// The final invoice can still be paid: closing the account would have
	// written it off.
	owed := func(in killbill.Invoice) int64 {
		v, err := billing.ToMinor(in.Balance, in.Currency)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if owed(usageInvoice) <= 0 {
		t.Fatalf("the usage invoice owes nothing, so collectibility is not tested: %s", mustJSON(usageInvoice))
	}
	if status, _, body := kbRaw(t, c.creds, http.MethodPost, "/1.0/kb/invoices/"+usageInvoice.InvoiceID+"/payments?externalPayment=true",
		map[string]any{"accountId": c.tenant.KillBillAccountID, "purchasedAmount": usageInvoice.Balance}); status != http.StatusCreated && status != http.StatusNoContent {
		t.Fatalf("payment of the final invoice = %d %s", status, body)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, _, body := kbRaw(t, c.creds, http.MethodGet, "/1.0/kb/invoices/"+usageInvoice.InvoiceID, nil)
		var paid killbill.Invoice
		if status == http.StatusOK && json.Unmarshal(body, &paid) == nil && owed(paid) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the final invoice is still unpaid: %s", body)
		}
		time.Sleep(time.Second)
	}

	// Every step is idempotent: another sweep changes nothing.
	before := blockingStates(t, c.s, c.creds, c.tenant.KillBillAccountID)
	if _, err := (&tenant.Sweeper{DB: c.db.Pool, Engines: c.engines, Closure: steps}).SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if after := blockingStates(t, c.s, c.creds, c.tenant.KillBillAccountID); !slices.Equal(after, before) {
		t.Fatalf("a sweep after the closure changed the blocking states: %v -> %v", before, after)
	}
}

// lostAnswers is a Kill Bill transport that passes every request on and loses
// the answer to the usage record: Kill Bill holds the record and Core does not
// know it.
type lostAnswers struct{ next http.RoundTripper }

func (l lostAnswers) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/1.0/kb/usages") {
		_ = resp.Body.Close()
		return nil, errors.New("connection reset by peer")
	}
	return resp, err
}

// A usage send whose answer is lost does not hold the closure back, and it
// gates its completion until staff resolve it with Kill Bill's evidence: the
// usage record is with Kill Bill, the closure cancels, the repair invoice
// bills it and lists its tracking ID, the tenant waits at blocking with the
// account blocked, the doctor says why, staff confirm the rollup as forwarded
// from that invoice, and the tenant closes. A not billed decision is refused
// while an invoice carries the record.
func TestClosureWithALostUsageAnswerOnTheStack(t *testing.T) {
	c := newClosureStack(t)
	ctx := context.Background()
	lossy, err := killbill.New(killbill.Config{URL: os.Getenv("ZS_TEST_KILLBILL_URL"), AdminUser: "admin", AdminPassword: os.Getenv("ZS_TEST_KILLBILL_ADMIN_PASSWORD"),
		HTTPClient: &http.Client{Transport: lostAnswers{http.DefaultTransport}}})
	if err != nil {
		t.Fatal(err)
	}
	lossyMeter := &metering.Service{DB: c.db.Pool, Billing: &billing.Service{DB: c.db.Pool, KillBill: lossy, Secrets: secrets.Default{}}}
	c.start(t)
	steps := tenant.EngineClosureSteps{Engines: c.engines, Drain: lossyMeter}
	c.cancelFirst(t, steps)
	if c.sweep(t, steps, 8) {
		t.Fatal("the tenant closed with a usage send nobody confirmed")
	}

	// Kill Bill holds the record; Core has it as an unconfirmed send.
	if got := kbUsage(t, c.creds, c.subID, c.from, c.to); got != 6_000_000 {
		t.Fatalf("Kill Bill holds %d micro-units, want 6000000", got)
	}
	var rollupID idgen.UUID
	var tracking, reason, state string
	if err := c.db.Pool.QueryRow(ctx, `SELECT id, tracking_id, coalesce(closure_unresolved_reason, '') FROM usage_rollups WHERE tenant_id = $1`, c.tenant.ID).Scan(&rollupID, &tracking, &reason); err != nil || reason == "" {
		t.Fatalf("rollup %v %q reason %q %v, want an unconfirmed send", rollupID, tracking, reason, err)
	}
	if err := c.db.Pool.QueryRow(ctx, `SELECT status FROM tenant_closure_settlements WHERE tenant_id = $1`, c.tenant.ID).Scan(&state); err != nil || state != "blocking" {
		t.Fatalf("settlement = %q %v, want it at blocking, refused completion", state, err)
	}
	if states := blockingStates(t, c.s, c.creds, c.tenant.KillBillAccountID); !slices.Equal(states, []string{tenant.BlockingClosed + ":false"}) {
		t.Fatalf("blocking states = %v, want the account blocked with billing open", states)
	}
	carrier := c.carrying(t, tracking)
	if carrier.InvoiceID == "" || carrier.Status != "COMMITTED" {
		t.Fatalf("no committed invoice carries %s: %s", tracking, mustJSON(c.invoices(t)))
	}

	// The doctor says what waits, naming the tenant and never the usage.
	findings, err := doctor.ClosureFindings(ctx, c.db.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, f := range findings {
		if f.Check == "usage_unconfirmed" && strings.Contains(f.Message, c.tenant.PublicID()) {
			named = true
		}
	}
	if !named {
		t.Fatalf("doctor findings = %+v, want usage_unconfirmed for %s", findings, c.tenant.PublicID())
	}

	// Staff resolve it. Not billed is refused, because an invoice carries it.
	good := &metering.Service{DB: c.db.Pool, Billing: &billing.Service{DB: c.db.Pool, KillBill: c.s.kb, Secrets: secrets.Default{}}}
	staff := metering.StaffCall{Call: metering.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:stack-staff"}, CorrelationID: "stack-resolve-" + c.tenant.PublicID()}, Reason: "stack closure"}
	rollup := idgen.Format(idgen.Rollup, rollupID)
	if _, _, err := good.ResolveUnconfirmedRollup(ctx, c.tenant.Scope(), staff, rollup, metering.ResolveDecisionNotBilled); errName(err) != "conflict" {
		t.Fatalf("not billed with an invoice that carries the record = %v, want a conflict", err)
	}
	res, replayed, err := good.ResolveUnconfirmedRollup(ctx, c.tenant.Scope(), staff, rollup, metering.ResolveDecisionForwarded)
	if err != nil || replayed || res.InvoiceID != carrier.InvoiceID {
		t.Fatalf("forwarded = %+v replayed %v %v, want the invoice %s", res, replayed, err, carrier.InvoiceID)
	}
	if again, replayed, err := good.ResolveUnconfirmedRollup(ctx, c.tenant.Scope(), staff, rollup, metering.ResolveDecisionForwarded); err != nil || !replayed || again.InvoiceID != res.InvoiceID {
		t.Fatalf("second forwarded = %+v replayed %v %v, want a replay", again, replayed, err)
	}

	// The closure completes, and the doctor no longer reports the send.
	if !c.sweep(t, steps, 10) {
		c.stuck(t)
	}
	findings, err = doctor.ClosureFindings(ctx, c.db.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.Check == "usage_unconfirmed" {
			t.Fatalf("the doctor still reports %+v after the resolution", f)
		}
	}
	if got := kbUsage(t, c.creds, c.subID, c.from, c.to); got != 6_000_000 {
		t.Fatalf("Kill Bill holds %d micro-units after the closure, want 6000000: nothing was sent again", got)
	}
}

// outcomes lists the kinds of the closure invoice outcomes, in order.
func (c closureStack) outcomes(t *testing.T) []string {
	t.Helper()
	rows, err := c.db.Pool.Query(context.Background(), `SELECT o.kind FROM tenant_closure_invoice_outcomes o JOIN tenant_closure_invoice_attempts a ON a.id = o.attempt_id WHERE a.tenant_id = $1 ORDER BY o.id`, c.tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	return kinds
}

// The finance step runs right after the cancel, with no wait for Kill Bill's
// invoice of the cancel, and closes the tenant. Kill Bill makes that invoice
// about three and a half seconds after the cancel, so the dry run and the
// trigger of the finance step either both come before it, or the invoice lands
// between them and the trigger answers a bare 404. In both cases the closure
// completes without a person: the invoice that carries the usage is found,
// recorded and read back strictly, and the usage is billed once. It is looped
// because the race is a matter of a second; ZS_TEST_CLOSURE_LOOPS sets how
// many closures run, six by default, and the log says in how many the trigger
// answered 404.
func TestClosureFinancesRightAfterTheCancelOnTheStack(t *testing.T) {
	loops := 6
	if v := os.Getenv("ZS_TEST_CLOSURE_LOOPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("ZS_TEST_CLOSURE_LOOPS = %q, want a positive number", v)
		}
		loops = n
	}
	notFound := 0
	for i := range loops {
		t.Run(fmt.Sprintf("closure-%d", i), func(t *testing.T) {
			c := newClosureStack(t)
			meter := &metering.Service{DB: c.db.Pool, Billing: &billing.Service{DB: c.db.Pool, KillBill: c.s.kb, Secrets: secrets.Default{}}}
			steps := tenant.EngineClosureSteps{Engines: c.engines, Drain: meter}
			c.start(t)
			if !c.sweep(t, steps, 15) {
				c.stuck(t)
			}
			kinds := c.outcomes(t)
			for _, k := range kinds {
				if k == "post_unknown" || k == "read_mismatch" {
					t.Fatalf("the finance step needs a person: outcomes %v", kinds)
				}
			}
			// Either the dry run came before the invoice of the cancel and the
			// trigger made or found it, or the invoice was there first and the dry
			// run found nothing left to invoice.
			if !slices.Equal(kinds, []string{"started", "invoice_id", "evidenced"}) && !slices.Equal(kinds, []string{"no_invoice"}) {
				t.Fatalf("outcomes = %v, want started, invoice_id, evidenced or only no_invoice", kinds)
			}
			var tracking string
			if err := c.db.Pool.QueryRow(context.Background(), `SELECT tracking_id FROM usage_rollups WHERE tenant_id = $1`, c.tenant.ID).Scan(&tracking); err != nil {
				t.Fatal(err)
			}
			carrying, usageMinor := 0, int64(0)
			for _, in := range c.invoices(t) {
				if slices.Contains(in.TrackingIDs, tracking) {
					carrying++
				}
				for _, item := range in.Items {
					if item.ItemType == "USAGE" && item.SubscriptionID == c.subID {
						v, err := billing.ToMinor(item.Amount, in.Currency)
						if err != nil {
							t.Fatal(err)
						}
						usageMinor += v
					}
				}
			}
			if carrying != 1 || usageMinor != 6 {
				t.Fatalf("%d invoices carry the tracking ID and bill %d minor units of usage, want 1 and 6: %s", carrying, usageMinor, mustJSON(c.invoices(t)))
			}
			notFound += int(c.triggerNotFound.Load())
		})
	}
	t.Logf("the invoice trigger answered 404 in %d of %d closures", notFound, loops)
}
