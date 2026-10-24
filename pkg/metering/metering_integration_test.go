//go:build integration

package metering_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	db    *postgrestest.DB
	fake  *enginetest.Fake
	eng   tenant.Engines
	clock *clock
	svc   *metering.Service
	call  metering.Call
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := postgrestest.New(t)
	fake := enginetest.New(t)
	e := &env{t: t, db: db, fake: fake, clock: &clock{now: time.Now().UTC().Truncate(time.Second)},
		eng:  tenant.Engines{Keycloak: fake.Keycloak(t), KillBill: fake.KillBill(t), Secrets: fake.Secrets()},
		call: metering.Call{Actor: events.Actor{Type: events.ActorService, ID: "scheduler"}, CorrelationID: "corr-usage"}}
	bill := &billing.Service{DB: db.Pool, KillBill: fake.KillBill(t), Secrets: fake.Secrets(), Now: e.clock.Now}
	e.svc = &metering.Service{DB: db.Pool, Billing: bill, Now: e.clock.Now}
	return e
}

// brand inserts a brand with the meter api-calls on the unit api-call.
func (e *env) brand(slug string) enginetest.Brand {
	e.t.Helper()
	b := e.fake.InsertBrand(e.t, e.db.Pool, slug)
	facts := brand.FactsFromCatalog([]killbill.Catalog{enginetest.TestCatalog()})
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		_, _, err := brand.ReplaceMeters(context.Background(), tx, b.ID, []brand.Meter{
			{Slug: "api-calls", Unit: "request", KillBillUnit: enginetest.UnitAPICall, Dimensions: []string{"region"}},
			{Slug: "old", Unit: "count", KillBillUnit: enginetest.UnitAPICall},
		}, facts, events.Actor{Type: events.ActorStaff, ID: "sub:op"}, "test", "test")
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

// subscribe puts the tenant on a plan in the fake Kill Bill.
func (e *env) subscribe(b enginetest.Brand, tn tenant.Tenant, plan string) string {
	e.t.Helper()
	id, err := e.fake.KillBill(e.t).CreateSubscription(context.Background(), killbill.TenantCredentials{APIKey: b.Slug, APISecret: "kb-secret-" + b.Slug},
		killbill.CallContext{CreatedBy: "test"}, tn.KillBillAccountID, plan, "sub-"+idgen.MustNewV7().String())
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) record(sc scope.Scope, batch ...metering.Event) ([]metering.Usage, []bool, error) {
	e.t.Helper()
	var (
		out     []metering.Usage
		replays []bool
	)
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		out, replays, err = e.svc.RecordUsageBatch(context.Background(), tx, sc, e.call, batch)
		return err
	})
	return out, replays, err
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

func count(t *testing.T, db *postgrestest.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRecordUsageIsIdempotentAndValidated(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-meter")
	tn := e.tenant(b, "Usage Co")
	sc := tn.Scope()
	ev := metering.Event{Meter: "api-calls", EventID: "evt-1", QuantityMicro: 2_500_000, Dimensions: map[string]string{"region": "yow"}}
	got, replays, err := e.record(sc, ev)
	if err != nil || replays[0] || !strings.HasPrefix(got[0].ID, "usg_") || got[0].ForwardedAt != nil {
		t.Fatalf("record = %+v %v %v", got, replays, err)
	}
	// The same payload again is a replay; occurred_at was set to now, so
	// the replay sends it back.
	ev.OccurredAt = got[0].OccurredAt
	again, replays, err := e.record(sc, ev)
	if err != nil || !replays[0] || again[0].ID != got[0].ID {
		t.Fatalf("replay = %+v %v %v", again, replays, err)
	}
	ev.QuantityMicro = 3_000_000
	if _, _, err := e.record(sc, ev); codeOf(err) != "usage_event_conflict" {
		t.Fatalf("other payload: %v", err)
	}
	for name, c := range map[string]struct {
		ev   metering.Event
		want string
	}{
		"unknown meter":       {metering.Event{Meter: "gpu", EventID: "x", QuantityMicro: 1}, "meter_not_found"},
		"undeclared key":      {metering.Event{Meter: "api-calls", EventID: "x", QuantityMicro: 1, Dimensions: map[string]string{"zone": "a"}}, "validation_failed"},
		"empty value":         {metering.Event{Meter: "api-calls", EventID: "x", QuantityMicro: 1, Dimensions: map[string]string{"region": ""}}, "validation_failed"},
		"zero quantity":       {metering.Event{Meter: "api-calls", EventID: "x"}, "validation_failed"},
		"too old":             {metering.Event{Meter: "api-calls", EventID: "x", QuantityMicro: 1, OccurredAt: e.clock.Now().Add(-8 * 24 * time.Hour)}, "usage_out_of_window"},
		"too far ahead":       {metering.Event{Meter: "api-calls", EventID: "x", QuantityMicro: 1, OccurredAt: e.clock.Now().Add(6 * time.Minute)}, "usage_out_of_window"},
		"bad event ID":        {metering.Event{Meter: "api-calls", EventID: "has space", QuantityMicro: 1}, "validation_failed"},
		"bad subscription ID": {metering.Event{Meter: "api-calls", EventID: "x", QuantityMicro: 1, Subscription: "nope"}, "validation_failed"},
	} {
		if _, _, err := e.record(sc, c.ev); codeOf(err) != c.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Within the window is accepted.
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "old-ok", QuantityMicro: 1, OccurredAt: e.clock.Now().Add(-6 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// A batch is all or nothing.
	batch := []metering.Event{{Meter: "api-calls", EventID: "b-1", QuantityMicro: 1}, {Meter: "api-calls", EventID: "b-2", QuantityMicro: 1, Dimensions: map[string]string{"zone": "x"}}}
	if _, _, err := e.record(sc, batch...); codeOf(err) != "validation_failed" {
		t.Fatalf("bad batch: %v", err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_event_keys WHERE event_id = 'b-1'`); n != 0 {
		t.Fatalf("a refused batch recorded %d events", n)
	}
	// A library caller that keeps its transaction after a refused batch,
	// and commits it, finds nothing of the batch: it ran in a savepoint.
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		if _, _, err := e.svc.RecordUsageBatch(ctx, tx, sc, e.call, batch); codeOf(err) != "validation_failed" {
			t.Fatalf("bad batch in a kept transaction: %v", err)
		}
		_, _, err := e.svc.RecordUsageBatch(ctx, tx, sc, e.call, []metering.Event{{Meter: "api-calls", EventID: "after", QuantityMicro: 1}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_event_keys WHERE event_id IN ('b-1', 'after')`); n != 1 {
		t.Fatalf("events after a kept transaction = %d", n)
	}
	big := make([]metering.Event, metering.MaxBatch+1)
	for i := range big {
		big[i] = metering.Event{Meter: "api-calls", EventID: "big-" + idgen.MustNewV7().String(), QuantityMicro: 1}
	}
	if _, _, err := e.record(sc, big...); codeOf(err) != "validation_failed" {
		t.Fatalf("1001 events: %v", err)
	}
	full, replays, err := e.record(sc, big[:metering.MaxBatch]...)
	if err != nil || len(full) != metering.MaxBatch || replays[0] {
		t.Fatalf("1000 events = %d %v", len(full), err)
	}

	// A retired meter takes nothing new.
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, _, err := brand.ReplaceMeters(ctx, tx, b.ID, []brand.Meter{{Slug: "api-calls", Unit: "request", KillBillUnit: enginetest.UnitAPICall, Dimensions: []string{"region"}}},
			brand.FactsFromCatalog([]killbill.Catalog{enginetest.TestCatalog()}), events.Actor{Type: events.ActorStaff, ID: "sub:op"}, "retire", "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.record(sc, metering.Event{Meter: "old", EventID: "r-1", QuantityMicro: 1}); codeOf(err) != "meter_not_found" {
		t.Fatalf("retired meter: %v", err)
	}

	// Summaries add up and a rebuild from the events gives the same rows.
	// The event recorded after the refused batch is in them.
	from := e.clock.Now().AddDate(0, 0, -10)
	to := e.clock.Now().AddDate(0, 0, 2)
	sum, err := metering.GetUsageSummary(ctx, e.db.Pool, sc, "api-calls", from, to)
	if err != nil || len(sum.Totals) != 1 || sum.Totals[0].QuantityMicro != 2_500_000+2+metering.MaxBatch || sum.Totals[0].Events != int64(3+metering.MaxBatch) {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error { return metering.RebuildSummaries(ctx, tx, sc, from, to) }); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := metering.GetUsageSummary(ctx, e.db.Pool, sc, "api-calls", from, to)
	if err != nil || mustJSON(rebuilt) != mustJSON(sum) {
		t.Fatalf("rebuilt = %s, want %s (%v)", mustJSON(rebuilt), mustJSON(sum), err)
	}

	// Events are append-only.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE usage_events SET quantity_micro = 1 WHERE brand_id = $1`, b.ID); err == nil {
		t.Fatal("an event's quantity changed")
	}
	if _, err := e.db.Pool.Exec(ctx, `DELETE FROM usage_events WHERE brand_id = $1`, b.ID); err == nil {
		t.Fatal("an event was deleted")
	}
	// One audit event and one outbox event per recording call that
	// recorded something.
	if n := count(t, e.db, `SELECT count(*) FROM outbox WHERE event_type = 'metering.usage.recorded' AND tenant_id = $1`, tn.ID); n != 4 {
		t.Fatalf("outbox events = %d", n)
	}
	if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE action = 'metering.usage.recorded' AND tenant_id = $1`, tn.ID); n != 4 {
		t.Fatalf("audit events = %d", n)
	}

	// A summary that would pass the int64 range is refused.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE usage_summaries SET quantity_micro = 9223372036854775000 WHERE brand_id = $1 AND tenant_id = $2`, b.ID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "overflow", QuantityMicro: 5_000, Dimensions: map[string]string{"region": "yow"}}); codeOf(err) != "usage_overflow" {
		t.Fatalf("overflow: %v", err)
	}
}

func TestUsageIsolation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, other := e.brand("zs-meter-a"), e.fake.InsertBrand(t, e.db.Pool, "zs-meter-b")
	x, y := e.tenant(a, "Tenant X"), e.tenant(a, "Tenant Y")
	if _, _, err := e.record(x.Scope(), metering.Event{Meter: "api-calls", EventID: "x-1", QuantityMicro: 7}); err != nil {
		t.Fatal(err)
	}
	if list, _, err := metering.ListUsageEvents(ctx, e.db.Pool, y.Scope(), metering.Filter{}, 50, ""); err != nil || len(list) != 0 {
		t.Fatalf("tenant Y reads X's usage = %v %v", list, err)
	}
	if list, _, err := metering.ListUsageEvents(ctx, e.db.Pool, x.Scope(), metering.Filter{}, 50, ""); err != nil || len(list) != 1 {
		t.Fatalf("tenant X = %v %v", list, err)
	}
	// The same event ID in another tenant is another event.
	if _, replays, err := e.record(y.Scope(), metering.Event{Meter: "api-calls", EventID: "x-1", QuantityMicro: 9}); err != nil || replays[0] {
		t.Fatalf("same event ID in another tenant = %v %v", replays, err)
	}
	// Brand B has no meter api-calls, and X is not a tenant of brand B.
	z := e.tenant(other, "Tenant Z")
	if _, _, err := e.record(z.Scope(), metering.Event{Meter: "api-calls", EventID: "z-1", QuantityMicro: 1}); codeOf(err) != "meter_not_found" {
		t.Fatalf("brand A's meter in brand B: %v", err)
	}
	if _, _, err := e.record(scope.Scope{Brand: other.ID, Tenant: x.ID}, metering.Event{Meter: "api-calls", EventID: "x-2", QuantityMicro: 1}); codeOf(err) != "tenant_not_found" {
		t.Fatalf("tenant X under brand B: %v", err)
	}
	if list, _, err := metering.ListUsageEvents(ctx, e.db.Pool, scope.Scope{Brand: other.ID, Tenant: x.ID}, metering.Filter{}, 50, ""); err != nil || len(list) != 0 {
		t.Fatalf("X's usage under brand B = %v %v", list, err)
	}
}

func TestForwardingWithDeterministicTrackingIDs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-forward")
	tn := e.tenant(b, "Forward Co")
	sc := tn.Scope()
	sub := e.subscribe(b, tn, enginetest.PlanMetered)
	// Recording works with Kill Bill down; forwarding waits.
	e.fake.SetDown(false, true)
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "f-1", QuantityMicro: 1_500_000},
		metering.Event{Meter: "api-calls", EventID: "f-2", QuantityMicro: 2_000_000, Dimensions: map[string]string{"region": "yul"}}); err != nil {
		t.Fatal(err)
	}
	report, err := e.svc.Forward(ctx)
	if err != nil || report.RolledUp != 1 || report.Forwarded != 0 || report.Failed != 1 || report.Unconfirmed != 0 {
		t.Fatalf("forward with Kill Bill down = %+v %v", report, err)
	}
	var tracking, lastErr string
	if err := e.db.Pool.QueryRow(ctx, `SELECT tracking_id, last_error FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&tracking, &lastErr); err != nil || lastErr != "usage_subscription_unconfirmed" {
		t.Fatalf("rollup = %q %q %v", tracking, lastErr, err)
	}
	var ids []idgen.UUID
	rows, _ := e.db.Pool.Query(ctx, `SELECT id FROM usage_events WHERE tenant_id = $1`, tn.ID)
	ids, _ = pgx.CollectRows(rows, pgx.RowTo[idgen.UUID])
	if want := metering.TrackingID(sc, enginetest.UnitAPICall, "", e.clock.Now(), ids); want != tracking {
		t.Fatalf("tracking ID %s is not the digest of the rollup %s", tracking, want)
	}
	// Kill Bill back: the rollup goes once its backoff ends.
	e.fake.SetDown(false, false)
	if report, err := e.svc.Forward(ctx); err != nil || report.Forwarded != 0 {
		t.Fatalf("forward during backoff = %+v %v", report, err)
	}
	e.clock.advance(11 * time.Second)
	if report, err := e.svc.Forward(ctx); err != nil || report.Forwarded != 1 || report.Duplicates != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	recs := e.fake.UsageRecords(b.Slug)
	if len(recs) != 1 || recs[0].TrackingID != tracking || recs[0].SubscriptionID != sub || recs[0].AmountMicro != 3_500_000 || recs[0].Unit != enginetest.UnitAPICall {
		t.Fatalf("Kill Bill usage = %+v", recs)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND forwarded_at IS NOT NULL AND killbill_tracking_id = $2`, tn.ID, tracking); n != 2 {
		t.Fatalf("forwarded events = %d", n)
	}
	billed, err := e.svc.BilledUsage(ctx, sc, sub, e.clock.Now().AddDate(0, 0, -1), e.clock.Now().AddDate(0, 0, 1))
	if err != nil || len(billed) != 1 || len(billed[0].Units) != 1 || billed[0].Units[0].QuantityMicro != 3_500_000 {
		t.Fatalf("billed = %+v %v", billed, err)
	}
	// Kill Bill already holds a rollup's tracking ID, as after an answer lost
	// on the way back: the send is answered as held, nothing more is
	// recorded, and the rollup is forwarded as a duplicate.
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "f-held", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.RollUp(ctx, sc); err != nil || n != 1 {
		t.Fatalf("rollup of the held usage = %d %v", n, err)
	}
	var heldTracking string
	if err := e.db.Pool.QueryRow(ctx, `SELECT tracking_id FROM usage_rollups WHERE tenant_id = $1 AND status = 'pending'`, tn.ID).Scan(&heldTracking); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.KillBill(t).RecordUsage(ctx, killbill.TenantCredentials{APIKey: b.Slug, APISecret: "kb-secret-" + b.Slug}, killbill.CallContext{CreatedBy: "test"},
		killbill.SubscriptionUsageRecord{SubscriptionID: sub, TrackingID: heldTracking, UnitUsageRecords: []killbill.UnitUsageRecords{{UnitType: enginetest.UnitAPICall,
			UsageRecords: []killbill.UsageRecord{{RecordDate: e.clock.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Amount: "1"}}}}}); err != nil {
		t.Fatal(err)
	}
	if report, err := e.svc.Forward(ctx); err != nil || report.Duplicates != 1 || report.Forwarded != 1 || report.Unconfirmed != 0 {
		t.Fatalf("forward of held usage = %+v %v", report, err)
	}
	if recs := e.fake.UsageRecords(b.Slug); len(recs) != 2 {
		t.Fatalf("Kill Bill usage after a held send = %+v", recs)
	}
	// New events make a new rollup with another tracking ID.
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "f-3", QuantityMicro: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ForwardTenant(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if recs := e.fake.UsageRecords(b.Slug); len(recs) != 3 || recs[2].TrackingID == tracking {
		t.Fatalf("second rollup = %+v", recs)
	}
	if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'metering.usage.forwarded'`, tn.ID); n != 3 {
		t.Fatalf("forward audit events = %d", n)
	}
}

func TestForwardingNeverReachesAnotherAccount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-forward-x")
	x, y := e.tenant(b, "X"), e.tenant(b, "Y")
	ySub := e.subscribe(b, y, enginetest.PlanMetered)
	// X names Y's subscription: Core checks the owner and never sends it.
	if _, _, err := e.record(x.Scope(), metering.Event{Meter: "api-calls", EventID: "steal", QuantityMicro: 1, Subscription: ySub}); err != nil {
		t.Fatal(err)
	}
	// Y without a subscription that takes the unit waits.
	if report, err := e.svc.Forward(ctx); err != nil || report.Failed != 1 || report.Forwarded != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	if recs := e.fake.UsageRecords(b.Slug); len(recs) != 0 {
		t.Fatalf("usage on another tenant's subscription = %+v", recs)
	}
	var lastErr string
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_error FROM usage_rollups WHERE tenant_id = $1`, x.ID).Scan(&lastErr); err != nil || lastErr != "usage_subscription_unconfirmed" {
		t.Fatalf("last error = %q %v", lastErr, err)
	}
	// A tenant without a plan that prices the unit waits too.
	z := e.tenant(b, "Z")
	e.subscribe(b, z, enginetest.PlanBasic)
	if _, _, err := e.record(z.Scope(), metering.Event{Meter: "api-calls", EventID: "z", QuantityMicro: 1}); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(time.Minute)
	if report, err := e.svc.Forward(ctx); err != nil || report.Forwarded != 0 {
		t.Fatalf("forward without a usage plan = %+v %v", report, err)
	}
}

func TestBacklogAlertAndRelay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-backlog")
	tn := e.tenant(b, "Backlog Co")
	e.subscribe(b, tn, enginetest.PlanMetered)
	e.fake.SetDown(false, true)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "late", QuantityMicro: 1}); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(25 * time.Hour)
	if report, err := e.svc.Forward(ctx); err != nil || report.Backlogs != 1 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	if report, err := e.svc.Forward(ctx); err != nil || report.Backlogs != 0 {
		t.Fatalf("second alert within a day = %+v %v", report, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM outbox WHERE event_type = 'metering.forward_backlog' AND tenant_id = $1`, tn.ID); n != 1 {
		t.Fatalf("backlog events = %d", n)
	}
	// The relay of metering.usage.recorded forwards at once when Kill
	// Bill answers.
	e.fake.SetDown(false, false)
	e.clock.advance(time.Hour)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "prompt", QuantityMicro: 1}); err != nil {
		t.Fatal(err)
	}
	relay := outbox.NewRelay("test", outbox.NewPostgresReader(e.db.Pool).WithEventTypes(metering.EventUsageRecorded), e.svc.RelayHandler(), 0, 0, nil)
	if n, err := relay.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("relay = %d %v", n, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND forwarded_at IS NULL`, tn.ID); n != 0 {
		t.Fatalf("unforwarded events after the relay = %d", n)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Partitions: the sweeper creates this month and the next three under its
// lock, a pass that finds them changes nothing, and no partition, the
// default one included, can be truncated. An event key must name an event.
func TestPartitionsAndSchemaGuards(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	far := time.Date(2031, 2, 10, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := metering.EnsurePartitions(ctx, e.db.Pool, far); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"usage_events_2031_02", "usage_events_2031_03", "usage_events_2031_04", "usage_events_2031_05"} {
		if n := count(t, e.db, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = current_schema() AND c.relname = $1`, name); n != 1 {
			t.Fatalf("partition %s = %d", name, n)
		}
	}
	if n := count(t, e.db, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = current_schema() AND c.relname = 'usage_events_2028_12'`); n != 1 {
		t.Fatal("the migration did not create the partitions through 2028-12")
	}
	for _, table := range []string{"usage_events", "usage_events_2031_03", "usage_events_2027_01", "usage_events_default"} {
		if _, err := e.db.Pool.Exec(ctx, `TRUNCATE `+table); err == nil {
			t.Fatalf("%s was truncated", table)
		}
	}
	b := e.brand("zs-schema")
	tn := e.tenant(b, "Schema Co")
	var meterID idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM meters WHERE brand_id = $1 AND slug = 'api-calls'`, b.ID).Scan(&meterID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_event_keys (brand_id, tenant_id, meter_id, event_id, usage_id, occurred_at, fingerprint)
VALUES ($1, $2, $3, 'orphan', gen_random_uuid(), now(), 'x')`, b.ID, tn.ID, meterID); err == nil {
		t.Fatal("an event key without its event was stored")
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_events (id, brand_id, tenant_id, meter_id, event_id, killbill_unit, quantity_micro,
    dimensions_key, occurred_at, received_at, reservation_id) VALUES (gen_random_uuid(), $1, $2, $3, 'r', 'api-call', 1, 'k', now(), now(), gen_random_uuid())`,
		b.ID, tn.ID, meterID); err == nil {
		t.Fatal("an event naming no reservation was stored")
	}
}

// Major 7. Usage dated before its subscription started, and usage on a
// cancelled subscription, are never sent and never silent: the rollup is
// rejected with its reason, audited, and reported by the reconciliation
// with its quantity and events until staff reassign or discard it.
func TestForwarderRejectsUnbillableUsage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-reject")
	tn := e.tenant(b, "Reject Co")
	sc := tn.Scope()
	late := e.subscribe(b, tn, enginetest.PlanMetered)
	e.fake.SetSubscriptionStart(b.Slug, late, e.clock.Now().Add(time.Hour))
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "early", QuantityMicro: 4_000_000}); err != nil {
		t.Fatal(err)
	}
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Rejected != 1 || report.Forwarded != 0 || report.Failed != 0 {
		t.Fatalf("forward before the start = %+v %v", report, err)
	}
	if recs := e.fake.UsageRecords(b.Slug); len(recs) != 0 {
		t.Fatalf("usage before the start reached Kill Bill: %+v", recs)
	}
	var reason string
	var rollupID idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id, reject_reason FROM usage_rollups WHERE tenant_id = $1 AND status = 'rejected'`, tn.ID).Scan(&rollupID, &reason); err != nil || reason != metering.RejectBeforeStart {
		t.Fatalf("rejected rollup = %q %v", reason, err)
	}
	// A subscription named by the events, then cancelled.
	other := e.tenant(b, "Cancel Co")
	gone := e.subscribe(b, other, enginetest.PlanMetered)
	if err := e.fake.KillBill(t).CancelSubscription(ctx, killbill.TenantCredentials{APIKey: b.Slug, APISecret: "kb-secret-" + b.Slug},
		killbill.CallContext{CreatedBy: "test"}, gone, killbill.PolicyImmediate); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.record(other.Scope(), metering.Event{Meter: "api-calls", EventID: "on-cancelled", QuantityMicro: 1_000_000, Subscription: gone}); err != nil {
		t.Fatal(err)
	}
	if report, err := e.svc.Forward(ctx); err != nil || report.Rejected != 1 {
		t.Fatalf("forward on a cancelled subscription = %+v %v", report, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollups WHERE tenant_id = $1 AND status = 'rejected' AND reject_reason = $2`, other.ID, metering.RejectCancelled); n != 1 {
		t.Fatalf("cancelled rejection = %d", n)
	}
	if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE action = $1`, metering.EventRollupRejected); n != 2 {
		t.Fatalf("rejection audits = %d", n)
	}
	// The reconciliation reports both, with their amounts and events.
	w := &wallet.Service{DB: e.db.Pool, Billing: e.svc.Billing, Metering: e.svc, Now: e.clock.Now}
	for _, tt := range []struct {
		sc       scope.Scope
		quantity float64
	}{{sc, 4_000_000}, {other.Scope(), 1_000_000}} {
		if _, err := w.Reconcile(ctx, tt.sc); err != nil {
			t.Fatal(err)
		}
		var details []byte
		if err := e.db.Pool.QueryRow(ctx, `SELECT details FROM wallet_reconciliation_findings WHERE tenant_id = $1 AND kind = 'rejected_usage'`,
			tt.sc.Tenant).Scan(&details); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		_ = json.Unmarshal(details, &d)
		if d["quantity_micro"] != tt.quantity || d["events"] != float64(1) {
			t.Fatalf("rejected_usage = %v", d)
		}
	}
	// Staff reassign the first to a subscription that started in time.
	staff := metering.StaffCall{Call: metering.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:support"}, CorrelationID: "c"}, Reason: "moved to the right plan"}
	public := idgen.Format(idgen.Rollup, rollupID)
	if _, err := e.svc.ReassignRollup(ctx, sc, staff, public, late); codeOf(err) != "billing_rejected" {
		t.Fatalf("reassign to the late subscription: %v", err)
	}
	right := e.subscribe(b, tn, enginetest.PlanMetered)
	if r, err := e.svc.ReassignRollup(ctx, sc, staff, public, right); err != nil || r.Status != metering.RollupPending || r.Subscription != right {
		t.Fatalf("reassign = %+v %v", r, err)
	}
	if report, err := e.svc.Forward(ctx); err != nil || report.Forwarded != 1 {
		t.Fatalf("forward after the reassignment = %+v %v", report, err)
	}
	if recs := e.fake.UsageRecords(b.Slug); len(recs) != 1 || recs[0].SubscriptionID != right || recs[0].AmountMicro != 4_000_000 {
		t.Fatalf("reassigned usage = %+v", recs)
	}
	// Staff discard the second, with a reason.
	var otherID idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM usage_rollups WHERE tenant_id = $1`, other.ID).Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.DiscardRollup(ctx, other.Scope(), metering.StaffCall{Call: staff.Call}, idgen.Format(idgen.Rollup, otherID)); codeOf(err) != "reason_required" {
		t.Fatalf("discard without a reason: %v", err)
	}
	if r, err := e.svc.DiscardRollup(ctx, other.Scope(), staff, idgen.Format(idgen.Rollup, otherID)); err != nil || r.Status != metering.RollupDiscarded {
		t.Fatalf("discard = %+v %v", r, err)
	}
	if _, err := e.svc.DiscardRollup(ctx, sc, staff, public); codeOf(err) != "conflict" {
		t.Fatalf("discard a forwarded rollup: %v", err)
	}
	for _, s := range []scope.Scope{sc, other.Scope()} {
		if _, err := w.Reconcile(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, e.db, `SELECT count(*) FROM wallet_reconciliation_findings WHERE kind = 'rejected_usage'`); n != 0 {
		t.Fatalf("rejected_usage left = %d", n)
	}
	for action, want := range map[string]int{"metering.rollup_reassigned": 1, "metering.rollup_discarded": 1} {
		if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE action = $1`, action); n != want {
			t.Fatalf("%s = %d", action, n)
		}
	}
}

// Major 6. Two forwarders run at once against a slow Kill Bill whose
// sends outlast the claim of a batch: every rollup is sent once. A
// forwarder renews its claim before each send and skips a rollup another
// forwarder took over.
func TestTwoForwardersNeverSendTwice(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-forward-race")
	const tenants = 12
	for i := 0; i < tenants; i++ {
		tn := e.tenant(b, "Race "+strings.Repeat("x", i+1))
		e.subscribe(b, tn, enginetest.PlanMetered)
		if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "e", QuantityMicro: 1_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	e.fake.SetWriteDelay("/1.0/kb/usages", 30*time.Millisecond)
	// Each send takes 70 seconds by the forwarders' clock: two sends
	// outlast the two minute claim of a batch.
	second := make(chan struct{}, 1)
	sends := 0
	e.fake.AfterWrite(func(method, path string) {
		if strings.HasPrefix(path, "/1.0/kb/usages") {
			e.clock.advance(70 * time.Second)
			sends++
			if sends == 2 {
				second <- struct{}{}
			}
		}
	})
	var wg sync.WaitGroup
	var errs [2]error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = e.svc.Forward(ctx)
	}()
	go func() {
		defer wg.Done()
		<-second
		_, errs[1] = e.svc.Forward(ctx)
	}()
	wg.Wait()
	e.fake.AfterWrite(nil)
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Anything left waits for its claim to end, then goes once.
	e.clock.advance(5 * time.Minute)
	if _, err := e.svc.Forward(ctx); err != nil {
		t.Fatal(err)
	}
	posts := 0
	for _, w := range e.fake.Writes() {
		if strings.HasPrefix(w.Path, "/1.0/kb/usages") {
			posts++
		}
	}
	if posts != tenants || len(e.fake.UsageRecords(b.Slug)) != tenants {
		t.Fatalf("sends = %d, records = %d, want %d each", posts, len(e.fake.UsageRecords(b.Slug)), tenants)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollups WHERE brand_id = $1 AND status = 'forwarded'`, b.ID); n != tenants {
		t.Fatalf("forwarded rollups = %d", n)
	}
}
