//go:build integration

package quota_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/quota"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

type env struct {
	t    *testing.T
	db   *postgrestest.DB
	fake *enginetest.Fake
	eng  tenant.Engines
	svc  *quota.Service
	now  time.Time
	call quota.Call
}

var operator = events.Actor{Type: events.ActorStaff, ID: "sub:op"}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := postgrestest.New(t)
	fake := enginetest.New(t)
	// Mid-month and mid-day, so no window ends during a test.
	e := &env{t: t, db: db, fake: fake, now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC),
		eng:  tenant.Engines{Keycloak: fake.Keycloak(t), KillBill: fake.KillBill(t), Secrets: fake.Secrets()},
		call: quota.Call{Actor: events.Actor{Type: events.ActorService, ID: "scheduler"}, CorrelationID: "corr-quota"}}
	bill := &billing.Service{DB: db.Pool, KillBill: fake.KillBill(t), Secrets: fake.Secrets()}
	e.svc = &quota.Service{DB: db.Pool, Billing: bill, Now: func() time.Time { return e.now }}
	return e
}

// brand inserts a brand with a meter and the given quotas.
func (e *env) brand(slug string, quotas ...brand.Entitlement) enginetest.Brand {
	e.t.Helper()
	ctx := context.Background()
	b := e.fake.InsertBrand(e.t, e.db.Pool, slug)
	facts := brand.FactsFromCatalog([]killbill.Catalog{enginetest.TestCatalog()})
	err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		if _, _, err := brand.ReplaceMeters(ctx, tx, b.ID, []brand.Meter{{Slug: "api-calls", Unit: "request", KillBillUnit: enginetest.UnitAPICall}},
			facts, operator, "test", "test"); err != nil {
			return err
		}
		_, _, err := brand.ReplaceQuotas(ctx, tx, b.ID, quotas, facts, operator, "test", "test")
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) tenant(b enginetest.Brand, name string) tenant.Tenant {
	e.t.Helper()
	owner := enginetest.InsertPerson(e.t, e.db.Pool, b, name+"@example.test")
	tn, err := tenant.CreateOrganization(context.Background(), e.db.Pool, e.eng, tenant.CreateRequest{
		Brand: b.ID, Creator: owner.Identity.ID, Name: name, IdempotencyKey: "key-" + idgen.MustNewV7().String(),
		BillingCountry: "CA", Actor: events.Actor{Type: events.ActorUser, ID: owner.Identity.PublicID()}, CorrelationID: "test",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tn
}

func (e *env) subscribe(b enginetest.Brand, tn tenant.Tenant, plan string) string {
	e.t.Helper()
	id, err := e.fake.KillBill(e.t).CreateSubscription(context.Background(), killbill.TenantCredentials{APIKey: b.Slug, APISecret: "kb-secret-" + b.Slug},
		killbill.CallContext{CreatedBy: "test"}, tn.KillBillAccountID, plan, "sub-"+idgen.MustNewV7().String())
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) use(sc scope.Scope, id string, micro int64) {
	e.t.Helper()
	m := &metering.Service{Now: func() time.Time { return e.now }}
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		_, _, err := m.RecordUsage(context.Background(), tx, sc, metering.Call{Actor: e.call.Actor, CorrelationID: "corr"},
			metering.Event{Meter: "api-calls", EventID: id, QuantityMicro: micro})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) consume(sc scope.Scope, feature, ref string, n int64) (quota.Entitlement, error) {
	var out quota.Entitlement
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = e.svc.Consume(context.Background(), tx, sc, e.call, feature, ref, n)
		return err
	})
	return out, err
}

func (e *env) release(sc scope.Scope, feature, ref string) (int64, error) {
	var out int64
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = e.svc.Release(context.Background(), tx, sc, e.call, feature, ref)
		return err
	})
	return out, err
}

func (e *env) override(sc scope.Scope, feature string, limit *int64, expires *time.Time) error {
	return e.overrideReq(sc, quota.OverrideRequest{Feature: feature, Limit: limit, ExpiresAt: expires})
}

func (e *env) overrideReq(sc scope.Scope, req quota.OverrideRequest) error {
	return pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		_, err := e.svc.SetOverride(context.Background(), tx, sc, quota.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:support"},
			Reason: "customer asked", CorrelationID: "corr-support"}, req)
		return err
	})
}

func codeOf(err error) string {
	var z *zserrors.Error
	if err == nil {
		return "ok"
	}
	if !errors.As(err, &z) {
		return "untyped: " + err.Error()
	}
	return zserrors.Lookup(z.Code).Name
}

func i64(n int64) *int64 { return &n }

func describe(e quota.Entitlement) string {
	v := func(p *int64) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("allowed=%v reason=%s plan=%s window=%s limit=%s used=%s remaining=%s", e.Allowed, e.Reason, e.Plan, e.Window, v(e.Limit), v(e.Used), v(e.Remaining))
}

func TestPlanLimitAndOverride(t *testing.T) {
	e := newEnv(t)
	b := e.brand("zs-quota", brand.Entitlement{Feature: enginetest.UnitAPICall, Plan: enginetest.PlanMetered, Limit: 3, Window: brand.WindowMonth, Meter: "api-calls"},
		brand.Entitlement{Feature: "seats", Plan: brand.AnyPlan, Limit: 5, Window: brand.WindowNone})
	tn := e.tenant(b, "quota")
	sc := tn.Scope()
	ctx := context.Background()
	if got := e.svc.CheckEntitlement(ctx, sc, enginetest.UnitAPICall); got.Allowed || got.Reason != billing.ReasonNoActiveSubscription {
		t.Fatalf("without a subscription = %s", describe(got))
	}
	e.subscribe(b, tn, enginetest.PlanMetered)
	got := e.svc.CheckEntitlement(ctx, sc, enginetest.UnitAPICall)
	if !got.Allowed || got.Plan != enginetest.PlanMetered || *got.Limit != 3 || *got.Used != 0 || *got.Remaining != 3 || got.Window != brand.WindowMonth {
		t.Fatalf("fresh = %s", describe(got))
	}
	e.use(sc, "u-1", 2_000_000)
	if got := e.svc.CheckEntitlement(ctx, sc, enginetest.UnitAPICall); !got.Allowed || *got.Used != 2 || *got.Remaining != 1 {
		t.Fatalf("after 2 = %s", describe(got))
	}
	// Used rounds up: 3.5 units count as 4.
	e.use(sc, "u-2", 1_500_000)
	if got := e.svc.CheckEntitlement(ctx, sc, enginetest.UnitAPICall); got.Allowed || got.Reason != billing.ReasonQuotaExceeded || *got.Used != 4 || *got.Remaining != 0 {
		t.Fatalf("over the limit = %s", describe(got))
	}
	var counter int64
	if err := e.db.Pool.QueryRow(ctx, `SELECT value FROM quota_counters WHERE tenant_id = $1 AND feature = $2`, tn.ID, enginetest.UnitAPICall).Scan(&counter); err != nil || counter != 4 {
		t.Fatalf("month counter = %d %v", counter, err)
	}
	// The tenant's override wins over the plan.
	if err := e.override(sc, enginetest.UnitAPICall, i64(10), nil); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, enginetest.UnitAPICall); !got.Allowed || *got.Limit != 10 || *got.Remaining != 6 {
		t.Fatalf("with an override = %s", describe(got))
	}
	// A cleared override brings the plan back, and an expired one is
	// ignored.
	if err := e.override(sc, enginetest.UnitAPICall, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, enginetest.UnitAPICall); got.Allowed || *got.Limit != 3 {
		t.Fatalf("after clearing = %s", describe(got))
	}
	soon := e.now.Add(time.Hour)
	if err := e.override(sc, enginetest.UnitAPICall, i64(50), &soon); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, enginetest.UnitAPICall); *got.Limit != 50 {
		t.Fatalf("override in force = %s", describe(got))
	}
	e.now = e.now.Add(2 * time.Hour)
	if got := e.svc.CheckEntitlement(ctx, sc, enginetest.UnitAPICall); *got.Limit != 3 {
		t.Fatalf("expired override = %s", describe(got))
	}
	if err := e.override(sc, enginetest.UnitAPICall, i64(1), &soon); codeOf(err) != "validation_failed" {
		t.Fatalf("override expiring in the past: %v", err)
	}
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, err := e.svc.SetOverride(ctx, tx, sc, quota.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:support"}, CorrelationID: "c"},
			quota.OverrideRequest{Feature: "seats", Limit: i64(1)})
		return err
	}); codeOf(err) != "reason_required" {
		t.Fatalf("override without a reason: %v", err)
	}
	// A quota for every plan grants the feature with its limit, and a none
	// window has no counter.
	if got := e.svc.CheckEntitlement(ctx, sc, "seats"); !got.Allowed || *got.Limit != 5 || got.Used != nil || got.Plan != enginetest.PlanMetered {
		t.Fatalf("seats = %s", describe(got))
	}
	// A feature with no quota and outside the plan is refused, one in the
	// plan has no limit.
	if got := e.svc.CheckEntitlement(ctx, sc, "core-basic"); got.Allowed || got.Reason != billing.ReasonNotInPlan {
		t.Fatalf("other product = %s", describe(got))
	}
	if got := e.svc.CheckEntitlement(ctx, sc, enginetest.ProductMeter); !got.Allowed || got.Limit != nil {
		t.Fatalf("product without a quota = %s", describe(got))
	}
	for _, action := range []string{"quota.override.set"} {
		var n int
		if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = $2`, tn.ID, action).Scan(&n); err != nil || n != 3 {
			t.Fatalf("%s audit events = %d %v", action, n, err)
		}
	}
}

func TestConcurrentConsumeAndRelease(t *testing.T) {
	e := newEnv(t)
	b := e.brand("zs-jobs", brand.Entitlement{Feature: "jobs", Plan: brand.AnyPlan, Limit: 2, Window: brand.WindowConcurrent},
		brand.Entitlement{Feature: enginetest.UnitAPICall, Plan: brand.AnyPlan, Limit: 2, Window: brand.WindowDay, Meter: "api-calls"})
	tn := e.tenant(b, "jobs")
	sc := tn.Scope()
	ctx := context.Background()
	e.subscribe(b, tn, enginetest.PlanBasic)
	got, err := e.consume(sc, "jobs", "job-1", 1)
	if err != nil || !got.Allowed || *got.Used != 1 || *got.Remaining != 1 {
		t.Fatalf("consume = %s %v", describe(got), err)
	}
	// The same ref again takes nothing more.
	if got, err := e.consume(sc, "jobs", "job-1", 1); err != nil || *got.Used != 1 {
		t.Fatalf("consume replay = %s %v", describe(got), err)
	}
	if _, err := e.consume(sc, "jobs", "job-1", 2); codeOf(err) != "conflict" {
		t.Fatalf("same ref, other amount: %v", err)
	}
	if got, err := e.consume(sc, "jobs", "job-2", 1); err != nil || got.Allowed || *got.Used != 2 {
		t.Fatalf("second job = %s %v", describe(got), err)
	}
	if _, err := e.consume(sc, "jobs", "job-3", 1); codeOf(err) != "quota_exceeded" {
		t.Fatalf("third job: %v", err)
	}
	if n, err := e.release(sc, "jobs", "job-1"); err != nil || n != 1 {
		t.Fatalf("release = %d %v", n, err)
	}
	if n, err := e.release(sc, "jobs", "job-1"); err != nil || n != 0 {
		t.Fatalf("release again = %d %v", n, err)
	}
	if n, err := e.release(sc, "jobs", "job-2"); err != nil || n != 1 {
		t.Fatalf("release job-2 = %d %v", n, err)
	}
	// Ten admissions at once: exactly the limit pass.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
	)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.consume(sc, "jobs", fmt.Sprintf("race-%d", i), 1)
			switch codeOf(err) {
			case "ok":
				mu.Lock()
				admitted++
				mu.Unlock()
			case "quota_exceeded":
			default:
				t.Errorf("race-%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if admitted != 2 {
		t.Fatalf("admitted = %d", admitted)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, "jobs"); got.Allowed || *got.Used != 2 {
		t.Fatalf("after the race = %s", describe(got))
	}
	// A day quota is consumed against its counter.
	if got, err := e.consume(sc, enginetest.UnitAPICall, "x", 1); err != nil || !got.Allowed || *got.Used != 1 || got.Window != brand.WindowDay {
		t.Fatalf("consume a day quota = %s %v", describe(got), err)
	}
	if _, err := e.consume(sc, "no-such-feature", "x", 1); codeOf(err) != "quota_exceeded" {
		t.Fatalf("consume an unknown feature: %v", err)
	}

	// Kill Bill down: the check and Consume fail closed, Release goes
	// through.
	e.fake.SetDown(false, true)
	if got := e.svc.CheckEntitlement(ctx, sc, "jobs"); got.Allowed || got.Reason != billing.ReasonBillingUnavailable {
		t.Fatalf("check with Kill Bill down = %s", describe(got))
	}
	if _, err := e.consume(sc, "jobs", "down", 1); codeOf(err) != "billing_unavailable" {
		t.Fatalf("consume with Kill Bill down: %v", err)
	}
	if n, err := e.release(sc, "jobs", "race-0"); err != nil {
		t.Fatalf("release with Kill Bill down = %d %v", n, err)
	}
	e.fake.SetDown(false, false)

	// A suspended tenant is on hold.
	if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, sc, events.Actor{Type: events.ActorService, ID: "ops"}, "test", "test"); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, "jobs"); got.Allowed || got.Reason != billing.ReasonWalletOnHold {
		t.Fatalf("suspended = %s", describe(got))
	}
	if _, err := e.consume(sc, "jobs", "held", 1); codeOf(err) != "wallet_on_hold" {
		t.Fatalf("consume while suspended: %v", err)
	}
}

func TestQuotaIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.brand("zs-quota-a", brand.Entitlement{Feature: "jobs", Plan: brand.AnyPlan, Limit: 1, Window: brand.WindowConcurrent})
	bb := e.brand("zs-quota-b")
	x, y := e.tenant(a, "x"), e.tenant(a, "y")
	z := e.tenant(bb, "z")
	for _, tn := range []tenant.Tenant{x, y} {
		e.subscribe(a, tn, enginetest.PlanBasic)
	}
	e.subscribe(bb, z, enginetest.PlanBasic)
	if _, err := e.consume(x.Scope(), "jobs", "job", 1); err != nil {
		t.Fatal(err)
	}
	// X's lease does not count for Y.
	if got, err := e.consume(y.Scope(), "jobs", "job", 1); err != nil || *got.Used != 1 {
		t.Fatalf("tenant Y = %s %v", describe(got), err)
	}
	// Brand A's quota does not exist in brand B.
	if got := e.svc.CheckEntitlement(ctx, z.Scope(), "jobs"); got.Allowed || got.Reason != billing.ReasonNotInPlan {
		t.Fatalf("brand A's quota in brand B = %s", describe(got))
	}
	// An override of X does not reach Y, and X under brand B is unknown.
	if err := e.override(x.Scope(), "jobs", i64(5), nil); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.CheckEntitlement(ctx, y.Scope(), "jobs"); *got.Limit != 1 {
		t.Fatalf("X's override in Y = %s", describe(got))
	}
	if err := e.override(scope.Scope{Brand: bb.ID, Tenant: x.ID}, "jobs", i64(5), nil); codeOf(err) != "tenant_not_found" {
		t.Fatalf("override of X under brand B: %v", err)
	}
	// Quota changes on a brand whose manifest declares them are refused.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE brands SET manifest = jsonb_set(manifest, '{entitlements}', '[]'::jsonb) WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, _, err := brand.ReplaceQuotas(ctx, tx, a.ID, nil, brand.CatalogueFacts{}, operator, "x", "x")
		return err
	})
	if codeOf(err) != "conflict" {
		t.Fatalf("replace manifest-managed quotas: %v", err)
	}
}

// Gate 6. Each window at limit - 1, limit and limit + 1.
func TestQuotaLimitBoundaries(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-quota-edge",
		brand.Entitlement{Feature: "calls-day", Plan: brand.AnyPlan, Limit: 3, Window: brand.WindowDay, Meter: "api-calls"},
		brand.Entitlement{Feature: "calls-month", Plan: brand.AnyPlan, Limit: 3, Window: brand.WindowMonth, Meter: "api-calls"},
		brand.Entitlement{Feature: "slots", Plan: brand.AnyPlan, Limit: 3, Window: brand.WindowConcurrent})
	tn := e.tenant(b, "edge")
	sc := tn.Scope()
	e.subscribe(b, tn, enginetest.PlanBasic)
	check := func(feature string, used int64, allowed bool) {
		t.Helper()
		got := e.svc.CheckEntitlement(ctx, sc, feature)
		want := max(3-used, 0)
		if got.Allowed != allowed || got.Used == nil || *got.Used != used || *got.Remaining != want || *got.Limit != 3 {
			t.Fatalf("%s at %d = %s, want allowed %v", feature, used, describe(got), allowed)
		}
		if !allowed && got.Reason != billing.ReasonQuotaExceeded {
			t.Fatalf("%s at %d reason = %s", feature, used, got.Reason)
		}
	}
	// Day and month: limit - 1 is allowed, limit and limit + 1 are not.
	e.use(sc, "d-1", 2_000_000)
	check("calls-day", 2, true)
	check("calls-month", 2, true)
	e.use(sc, "d-2", 1_000_000)
	check("calls-day", 3, false)
	check("calls-month", 3, false)
	e.use(sc, "d-3", 1_000_000)
	check("calls-day", 4, false)
	check("calls-month", 4, false)
	// The next day the day window starts again, the month window does not.
	e.now = e.now.Add(24 * time.Hour)
	check("calls-day", 0, true)
	check("calls-month", 4, false)

	// Concurrent: the consumption that reaches limit - 1 and the limit pass,
	// the one past the limit is refused.
	if got, err := e.consume(sc, "slots", "s-1", 2); err != nil || !got.Allowed || *got.Used != 2 {
		t.Fatalf("to limit - 1 = %s %v", describe(got), err)
	}
	if _, err := e.consume(sc, "slots", "s-over", 2); codeOf(err) != "quota_exceeded" {
		t.Fatalf("to limit + 1: %v", err)
	}
	if got, err := e.consume(sc, "slots", "s-2", 1); err != nil || got.Allowed || *got.Used != 3 || *got.Remaining != 0 {
		t.Fatalf("to the limit = %s %v", describe(got), err)
	}
	if _, err := e.consume(sc, "slots", "s-3", 1); codeOf(err) != "quota_exceeded" {
		t.Fatalf("past the limit: %v", err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, "slots"); got.Allowed || *got.Used != 3 {
		t.Fatalf("slots after refusals = %s", describe(got))
	}

	// Consumed day and month windows: the counter admits limit - 1 and the
	// limit, and refuses limit + 1. Day 2 starts with the month at 4 from
	// the recorded usage, so only the day window is consumed here.
	if got, err := e.consume(sc, "calls-day", "c-1", 2); err != nil || !got.Allowed || *got.Used != 2 {
		t.Fatalf("day to limit - 1 = %s %v", describe(got), err)
	}
	if _, err := e.consume(sc, "calls-day", "c-over", 2); codeOf(err) != "quota_exceeded" {
		t.Fatalf("day to limit + 1: %v", err)
	}
	if got, err := e.consume(sc, "calls-day", "c-2", 1); err != nil || got.Allowed || *got.Used != 3 {
		t.Fatalf("day to the limit = %s %v", describe(got), err)
	}
	if _, err := e.consume(sc, "calls-day", "c-3", 1); codeOf(err) != "quota_exceeded" {
		t.Fatalf("day past the limit: %v", err)
	}
	if _, err := e.consume(sc, "calls-month", "m-1", 1); codeOf(err) != "quota_exceeded" {
		t.Fatalf("month past the limit: %v", err)
	}
	check("calls-day", 3, false)
}

// Leases belong to the holder that consumed them: another holder neither
// releases nor extends them, staff with a reason do and are audited. A
// lease that is not extended expires and the sweeper releases it.
func TestLeasesBelongToHolderAndExpire(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-quota-lease", brand.Entitlement{Feature: "jobs", Plan: brand.AnyPlan, Limit: 2, Window: brand.WindowConcurrent})
	tn := e.tenant(b, "lease")
	sc := tn.Scope()
	e.subscribe(b, tn, enginetest.PlanBasic)
	keyCall := quota.Call{Actor: events.Actor{Type: events.ActorAPIKey, ID: "key_a"}, CorrelationID: "c", Holder: "apikey:key_a"}
	ownerCall := quota.Call{Actor: events.Actor{Type: events.ActorUser, ID: "idn_owner"}, CorrelationID: "c", Holder: "membership:mem_owner"}
	in := func(fn func(tx pgx.Tx) error) error { return pgx.BeginFunc(ctx, e.db.Pool, fn) }
	if err := in(func(tx pgx.Tx) error {
		_, err := e.svc.ConsumeFor(ctx, tx, sc, keyCall, "jobs", "job-1", 1, time.Minute)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The owner holds quota:consume but not the key's lease.
	if err := in(func(tx pgx.Tx) error {
		_, err := e.svc.Release(ctx, tx, sc, ownerCall, "jobs", "job-1")
		return err
	}); codeOf(err) != "not_found" {
		t.Fatalf("owner releases the key's lease: %v", err)
	}
	if err := in(func(tx pgx.Tx) error {
		_, err := e.svc.ConsumeFor(ctx, tx, sc, ownerCall, "jobs", "job-1", 1, 0)
		return err
	}); codeOf(err) != "not_found" {
		t.Fatalf("owner consumes under the key's ref: %v", err)
	}
	if err := in(func(tx pgx.Tx) error {
		_, err := e.svc.Extend(ctx, tx, sc, ownerCall, "jobs", "job-1", time.Hour)
		return err
	}); codeOf(err) != "not_found" {
		t.Fatalf("owner extends the key's lease: %v", err)
	}
	// The key extends its own lease past the first expiry.
	if err := in(func(tx pgx.Tx) error {
		_, err := e.svc.Extend(ctx, tx, sc, keyCall, "jobs", "job-1", 5*time.Minute)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := in(func(tx pgx.Tx) error {
		_, err := e.svc.ConsumeFor(ctx, tx, sc, keyCall, "jobs", "job-2", 1, time.Minute)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sweeper := &quota.Sweeper{Service: e.svc}
	e.now = e.now.Add(2 * time.Minute)
	if n, err := sweeper.SweepOnce(ctx); err != nil || n != 1 {
		t.Fatalf("sweep = %d %v", n, err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, "jobs"); *got.Used != 1 {
		t.Fatalf("after job-2 expired = %s", describe(got))
	}
	// Staff without a reason are refused, with one they release.
	staff := quota.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:support"}, CorrelationID: "c", Staff: true}
	if err := in(func(tx pgx.Tx) error {
		_, err := e.svc.Release(ctx, tx, sc, staff, "jobs", "job-1")
		return err
	}); codeOf(err) != "reason_required" {
		t.Fatalf("staff without a reason: %v", err)
	}
	staff.Reason = "stuck job"
	var n int64
	if err := in(func(tx pgx.Tx) error {
		var err error
		n, err = e.svc.Release(ctx, tx, sc, staff, "jobs", "job-1")
		return err
	}); err != nil || n != 1 {
		t.Fatalf("staff release = %d %v", n, err)
	}
	for action, want := range map[string]int{"quota.lease.expired": 1, "quota.lease.staff_override": 1, "quota.lease.extended": 1} {
		var got int
		if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = $2`, tn.ID, action).Scan(&got); err != nil || got != want {
			t.Fatalf("%s audit events = %d %v", action, got, err)
		}
	}
	if got := e.svc.CheckEntitlement(ctx, sc, "jobs"); *got.Used != 0 {
		t.Fatalf("after the releases = %s", describe(got))
	}
}

// Day windows admit atomically under the tenant-feature lock with the
// counter, and usage recorded beyond the counter is drift: the counter is
// raised to it and the drift audited.
func TestWindowConsumeIsAtomicAndDriftAudited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-quota-window", brand.Entitlement{Feature: "calls", Plan: brand.AnyPlan, Limit: 3, Window: brand.WindowDay, Meter: "api-calls"})
	tn := e.tenant(b, "window")
	sc := tn.Scope()
	e.subscribe(b, tn, enginetest.PlanBasic)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
	)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.consume(sc, "calls", fmt.Sprintf("w-%d", i), 1)
			switch codeOf(err) {
			case "ok":
				mu.Lock()
				admitted++
				mu.Unlock()
			case "quota_exceeded":
			default:
				t.Errorf("w-%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if admitted != 3 {
		t.Fatalf("admitted = %d", admitted)
	}
	// A retry of an admitted ref takes nothing more.
	var ref string
	if err := e.db.Pool.QueryRow(ctx, `SELECT ref FROM quota_leases WHERE tenant_id = $1 LIMIT 1`, tn.ID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if got, err := e.consume(sc, "calls", ref, 1); err != nil || *got.Used != 3 {
		t.Fatalf("replay = %s %v", describe(got), err)
	}
	// The next day: usage recorded without consuming passes the counter.
	e.now = e.now.Add(24 * time.Hour)
	e.use(sc, "rec-1", 2_000_000)
	got := e.svc.CheckEntitlement(ctx, sc, "calls")
	if *got.Used != 2 {
		t.Fatalf("after recorded usage = %s", describe(got))
	}
	var counter int64
	day := e.now.Truncate(24 * time.Hour)
	if err := e.db.Pool.QueryRow(ctx, `SELECT value FROM quota_counters WHERE tenant_id = $1 AND feature = 'calls' AND window_start = $2`, tn.ID, day).Scan(&counter); err != nil || counter != 2 {
		t.Fatalf("raised counter = %d %v", counter, err)
	}
	if _, err := e.consume(sc, "calls", "after", 2); codeOf(err) != "quota_exceeded" {
		t.Fatalf("consume past the raised counter: %v", err)
	}
	var drift int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'quota.counter.drift'`, tn.ID).Scan(&drift); err != nil || drift != 1 {
		t.Fatalf("drift audits = %d %v", drift, err)
	}
	// A release never gives a day consumption back.
	if n, err := e.release(sc, "calls", ref); err != nil || n != 0 {
		t.Fatalf("release a day consumption = %d %v", n, err)
	}
}

// An override takes the window of the plan's quota or names its own, and
// applies only to the matching window. A clearing override cuts off the
// ones before it for good.
func TestOverrideWindows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-quota-override", brand.Entitlement{Feature: "calls", Plan: brand.AnyPlan, Limit: 3, Window: brand.WindowDay, Meter: "api-calls"})
	tn := e.tenant(b, "override")
	sc := tn.Scope()
	e.subscribe(b, tn, enginetest.PlanBasic)
	// An override naming another window does not apply to the day quota.
	if err := e.overrideReq(sc, quota.OverrideRequest{Feature: "calls", Limit: i64(50), Window: brand.WindowConcurrent}); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, "calls"); *got.Limit != 3 || got.Window != brand.WindowDay {
		t.Fatalf("mismatched window = %s", describe(got))
	}
	// One without a window takes the day window.
	if err := e.override(sc, "calls", i64(10), nil); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, "calls"); *got.Limit != 10 || got.Window != brand.WindowDay {
		t.Fatalf("override without a window = %s", describe(got))
	}
	// A feature no quota covers needs the window named.
	if err := e.override(sc, "gpus", i64(2), nil); codeOf(err) != "validation_failed" {
		t.Fatalf("override without a quota or a window: %v", err)
	}
	if err := e.overrideReq(sc, quota.OverrideRequest{Feature: "gpus", Limit: i64(2), Window: brand.WindowDay}); codeOf(err) != "validation_failed" {
		t.Fatalf("day override without a meter: %v", err)
	}
	// An override caps a feature the plan grants. It grants nothing.
	if err := e.overrideReq(sc, quota.OverrideRequest{Feature: "gpus", Limit: i64(2), Window: brand.WindowConcurrent}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.consume(sc, "gpus", "g-1", 1); codeOf(err) != "quota_exceeded" {
		t.Fatalf("override of a feature outside the plan: %v", err)
	}
	// The basic product is in the plan: its override names a concurrent
	// window and applies with it.
	if err := e.overrideReq(sc, quota.OverrideRequest{Feature: "core-basic", Limit: i64(2), Window: brand.WindowConcurrent}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.consume(sc, "core-basic", "g-1", 2); err != nil || got.Window != brand.WindowConcurrent || *got.Limit != 2 || got.Allowed {
		t.Fatalf("override with its own window = %s %v", describe(got), err)
	}
	// A clearing override that expires never brings the older ones back.
	soon := e.now.Add(time.Hour)
	if err := e.override(sc, "calls", nil, &soon); err != nil {
		t.Fatal(err)
	}
	if got := e.svc.CheckEntitlement(ctx, sc, "calls"); *got.Limit != 3 {
		t.Fatalf("cleared = %s", describe(got))
	}
	e.now = e.now.Add(2 * time.Hour)
	if got := e.svc.CheckEntitlement(ctx, sc, "calls"); *got.Limit != 3 {
		t.Fatalf("after the clearing override expired = %s", describe(got))
	}
}
