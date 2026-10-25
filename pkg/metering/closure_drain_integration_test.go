//go:build integration

package metering_test

import (
	"context"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// closing marks the tenant closing the way a started closure does.
func (e *env) closing(tn tenant.Tenant) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at) VALUES ($1, $2, now())`, tn.BrandID, tn.ID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE id = $1`, tn.ID); err != nil {
		e.t.Fatal(err)
	}
}

// Drained sends what the tenant still has to Kill Bill and says it is drained
// once nothing is left: its events are in rollups and its rollups are with Kill
// Bill.
func TestDrainedForwardsAClosingTenantsUsageAndReportsDone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-drain")
	tn := e.tenant(b, "Drain Co")
	sub := e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "d-1", QuantityMicro: 1_500_000}, metering.Event{Meter: "api-calls", EventID: "d-2", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	e.closing(tn)
	done, err := e.svc.Drained(ctx, tn.Scope())
	if err != nil || !done {
		t.Fatalf("drained = %v %v", done, err)
	}
	recs := e.fake.UsageRecords(b.Slug)
	if len(recs) != 1 || recs[0].SubscriptionID != sub || recs[0].AmountMicro != 3_500_000 {
		t.Fatalf("usage in Kill Bill = %+v", recs)
	}
	var unrolled int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND rollup_id IS NULL`, tn.ID).Scan(&unrolled); err != nil || unrolled != 0 {
		t.Fatalf("unrolled events = %d, %v", unrolled, err)
	}
	// A second call changes nothing.
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || !done || len(e.fake.UsageRecords(b.Slug)) != 1 {
		t.Fatalf("second drained = %v %v, usage records %d", done, err, len(e.fake.UsageRecords(b.Slug)))
	}
}

// While Kill Bill cannot take a rollup, and while the rollup waits out its
// backoff, the tenant is not drained.
func TestDrainedWaitsForKillBillAndTheBackoff(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-drain-wait")
	tn := e.tenant(b, "Drain Wait")
	e.subscribe(b, tn, enginetest.PlanMetered)
	e.fake.SetDown(false, true)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "w-1", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	e.closing(tn)
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || done {
		t.Fatalf("drained with Kill Bill down = %v %v, want false", done, err)
	}
	e.fake.SetDown(false, false)
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || done {
		t.Fatalf("drained during the backoff = %v %v, want false", done, err)
	}
	e.clock.advance(11 * time.Second)
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || !done {
		t.Fatalf("drained after the backoff = %v %v, want true", done, err)
	}
}

// An open reservation holds the drain until it is settled or released.
func TestDrainedWaitsForAnOpenReservation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-drain-res")
	tn := e.tenant(b, "Drain Reservation")
	e.closing(tn)
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO reservations (id, brand_id, tenant_id, currency, external_ref, attempt, amount_minor, lease_expires_at, created_by_actor, actor_type, correlation_id)
VALUES ($1, $2, $3, 'CAD', 'held-1', 0, 100, now() + interval '1 hour', 'test', 'service', 'c')`, idgen.MustNewV7(), tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || done {
		t.Fatalf("drained with a held reservation = %v %v, want false", done, err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE reservations SET status = 'released', released_at = now() WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || !done {
		t.Fatalf("drained after the release = %v %v, want true", done, err)
	}
}

// Usage accepted before dispatch protocol 1 is read-only and never rolled up:
// it does not hold the drain, and the closure review takes it to a person.
func TestDrainedLeavesLegacyUsageToTheReview(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-drain-legacy")
	tn := e.tenant(b, "Drain Legacy")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_events (id, brand_id, tenant_id, meter_id, event_id, killbill_unit, quantity_micro, dimensions_key, occurred_at, received_at)
SELECT gen_random_uuid(), $1, $2, id, 'legacy-1', $3, 1, 'k', now(), now() FROM meters WHERE brand_id = $1 AND slug = 'api-calls'`, tn.BrandID, tn.ID, enginetest.UnitAPICall); err != nil {
		t.Fatal(err)
	}
	e.closing(tn)
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || !done {
		t.Fatalf("drained with legacy usage = %v %v, want true", done, err)
	}
	if n, err := e.svc.LegacyUnrolled(ctx, tn.Scope()); err != nil || n != 1 {
		t.Fatalf("legacy unrolled = %d %v, want 1", n, err)
	}
}

// Raw usage is refused for a tenant whose closure has started, and for a closed
// one: what the closure invoices is what was accepted before it.
func TestClosingTenantRefusesNewUsage(t *testing.T) {
	e := newEnv(t)
	b := e.brand("zs-drain-refuse")
	tn := e.tenant(b, "Drain Refuse")
	e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "r-1", QuantityMicro: 1}); err != nil {
		t.Fatalf("usage of an active tenant = %v", err)
	}
	e.closing(tn)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "r-2", QuantityMicro: 1}); !isNotFound(err) {
		t.Fatalf("usage of a closing tenant = %v, want tenant_not_found", err)
	}
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE tenants SET status = 'closed' WHERE id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "r-3", QuantityMicro: 1}); !isNotFound(err) {
		t.Fatalf("usage of a closed tenant = %v, want tenant_not_found", err)
	}
}

func isNotFound(err error) bool {
	return err != nil && codeOf(err) == "tenant_not_found"
}

// The whole closure with the real steps and the real drain against the fake
// Kill Bill: the usage is forwarded before the settlement moves on, the
// subscriptions are cancelled, the account is blocked, and the tenant closes.
func TestSweeperClosesATenantAfterDrainingItsUsage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-drain-e2e")
	tn := e.tenant(b, "Drain E2E")
	e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "e-1", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.StartClosure(ctx, e.db.Pool, tn.Scope(), tenant.SystemActor, tenant.ClosureRequest{Reason: "left", RequestID: "closure-e2e-0001"}, "c"); err != nil {
		t.Fatal(err)
	}
	steps := tenant.EngineClosureSteps{Engines: e.eng, Drain: e.svc, Today: func() string { return enginetest.CancelDay }}
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}
	var status string
	for pass := 1; pass <= 5; pass++ {
		if _, err := sweeper.SweepOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if err := e.db.Pool.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tn.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "closed" {
			break
		}
		if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET next_attempt_at = now() - interval '1 second' WHERE tenant_id = $1`, tn.ID); err != nil {
			t.Fatal(err)
		}
	}
	if status != "closed" {
		var state, lastErr string
		_ = e.db.Pool.QueryRow(ctx, `SELECT status, coalesce(last_error, '') FROM tenant_closure_settlements WHERE tenant_id = $1`, tn.ID).Scan(&state, &lastErr)
		t.Fatalf("tenant %q, settlement %q last_error %q, want closed", status, state, lastErr)
	}
	if recs := e.fake.UsageRecords(b.Slug); len(recs) != 1 || recs[0].AmountMicro != 2_000_000 {
		t.Fatalf("usage in Kill Bill = %+v, want the drained usage once", recs)
	}
	if a, _ := e.fake.Account(b.Slug, tn.PublicID()); a.Closed {
		t.Fatal("the Kill Bill account was closed")
	}
}

// One call of Drained does a bounded amount of work. When the events are more
// than its rounds roll up, it says the tenant is not drained and the next call
// goes on; with enough rounds it says drained.
func TestDrainedIsBoundedByItsRounds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-drain-rounds")
	tn := e.tenant(b, "Drain Rounds")
	e.subscribe(b, tn, enginetest.PlanMetered)
	for i := 0; i < 5; i++ {
		if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "round-" + string(rune('a'+i)), QuantityMicro: 1_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	e.closing(tn)
	restore := metering.SetDrainLimitsForTest(2, 1)
	t.Cleanup(restore)
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || done {
		t.Fatalf("drained after one round of two events out of five = %v %v, want false", done, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND rollup_id IS NULL`, tn.ID); n != 3 {
		t.Fatalf("unrolled events after one round = %d, want 3", n)
	}
	metering.SetDrainLimitsForTest(2, 3)
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || !done {
		t.Fatalf("drained with three rounds = %v %v, want true", done, err)
	}
}
