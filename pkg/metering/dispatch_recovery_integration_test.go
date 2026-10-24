//go:build integration

package metering_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// failFinalize makes the update that marks an event forwarded fail, the way
// a database failure after a successful send does. fail is how many
// updates fail (-1 for every one).
func failFinalize(t *testing.T, e *env, fail int) {
	t.Helper()
	ctx := context.Background()
	body := strings.ReplaceAll(`CREATE FUNCTION f5_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
  IF OLD.forwarded_at IS NULL AND NEW.forwarded_at IS NOT NULL AND (FAILN < 0 OR nextval('f5_fail_seq') <= FAILN) THEN RAISE EXCEPTION 'f5 injected failure'; END IF;
  RETURN NEW; END; $$`, "FAILN", itoa(fail))
	for _, sql := range []string{`CREATE SEQUENCE f5_fail_seq`, body,
		`CREATE TRIGGER a_f5_fail BEFORE UPDATE ON usage_events FOR EACH ROW EXECUTE FUNCTION f5_fail()`} {
		if _, err := e.db.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
}

func itoa(n int) string {
	if n < 0 {
		return "-1"
	}
	return string(rune('0' + n))
}

func clearFinalizeFailure(t *testing.T, e *env) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), `DROP TRIGGER a_f5_fail ON usage_events`); err != nil {
		t.Fatal(err)
	}
}

func setupOneRollup(t *testing.T, slug string) (*env, string, idgen.UUID) {
	t.Helper()
	e := newEnv(t)
	b := e.brand(slug)
	tn := e.tenant(b, slug+" Co")
	e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "r-a", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	return e, b.Slug, tn.ID
}

// Kill Bill holds the usage but the answer is a 503: the rollup is kept as
// unconfirmed and no second POST is made, now or later.
func TestLostAnswerIsUnconfirmedAndNeverResent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-lost")
	tn := e.tenant(b, "Lost Co")
	sub := e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "l-a", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.RollUp(ctx, tn.Scope()); err != nil || n != 1 {
		t.Fatalf("rollup = %d %v", n, err)
	}
	var tracking string
	if err := e.db.Pool.QueryRow(ctx, `SELECT tracking_id FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&tracking); err != nil {
		t.Fatal(err)
	}
	// Kill Bill recorded it, and the answer to the forwarder is lost.
	if err := e.fake.KillBill(t).RecordUsage(ctx, killbill.TenantCredentials{APIKey: b.Slug, APISecret: "kb-secret-" + b.Slug}, killbill.CallContext{CreatedBy: "test"},
		killbill.SubscriptionUsageRecord{SubscriptionID: sub, TrackingID: tracking, UnitUsageRecords: []killbill.UnitUsageRecords{{UnitType: enginetest.UnitAPICall,
			UsageRecords: []killbill.UsageRecord{{RecordDate: e.clock.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Amount: "2"}}}}}); err != nil {
		t.Fatal(err)
	}
	e.fake.FailWrites("/1.0/kb/usages", 1, 503)
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Unconfirmed != 1 || report.Forwarded != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var reason string
	if err := e.db.Pool.QueryRow(ctx, `SELECT closure_unresolved_reason FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&reason); err != nil || reason != "usage_send_unconfirmed" {
		t.Fatalf("reason = %q %v", reason, err)
	}
	posts := usagePosts(e)
	e.clock.advance(24 * time.Hour)
	if _, err := e.svc.Forward(ctx); err != nil || usagePosts(e) != posts {
		t.Fatalf("a second POST was made: %d -> %d %v", posts, usagePosts(e), err)
	}
	if recs := e.fake.UsageRecords(b.Slug); len(recs) != 1 {
		t.Fatalf("Kill Bill holds %d records, want exactly 1", len(recs))
	}
}

// A database failure after a successful POST is retried; the usage is
// forwarded once and never sent again.
func TestFinalizeRetriesATransientDatabaseFailureAfterASuccessfulSend(t *testing.T) {
	e, slug, tenantID := setupOneRollup(t, "zs-finalize")
	ctx := context.Background()
	failFinalize(t, e, 2)
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Forwarded != 1 || report.Failed != 0 || report.Unconfirmed != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollups WHERE tenant_id = $1 AND status = 'forwarded'`, tenantID); n != 1 {
		t.Fatalf("forwarded rollups = %d", n)
	}
	if recs := e.fake.UsageRecords(slug); len(recs) != 1 || usagePosts(e) != 1 {
		t.Fatalf("records = %d posts = %d, want 1 each", len(recs), usagePosts(e))
	}
}

// The sweep never resends. An expired claim that never started and a started
// send nobody finished both become unresolved, with an audit row.
func TestSweepResolvesExpiredClaimAndStrandedSend(t *testing.T) {
	// A claim that ran out before any send started.
	e, _, tenantID := setupOneRollup(t, "zs-sweep-c")
	ctx := context.Background()
	e.svc.ClaimTTL = time.Second
	sc := scopeOf(t, e, tenantID)
	if n, err := e.svc.RollUp(ctx, sc); err != nil || n != 1 {
		t.Fatalf("rollup = %d %v", n, err)
	}
	if due, err := e.svc.ClaimDue(ctx, &sc); err != nil || len(due) != 1 {
		t.Fatalf("claim = %d %v", len(due), err)
	}
	if n, err := e.svc.ResolveStranded(ctx); err != nil || n != 0 {
		t.Fatalf("sweep of a live claim = %d %v", n, err)
	}
	time.Sleep(1500 * time.Millisecond)
	if n, err := e.svc.ResolveStranded(ctx); err != nil || n != 1 {
		t.Fatalf("sweep of an expired claim = %d %v", n, err)
	}
	var reason string
	if err := e.db.Pool.QueryRow(ctx, `SELECT closure_unresolved_reason FROM usage_rollups WHERE tenant_id = $1`, tenantID).Scan(&reason); err != nil || reason != "usage_claim_unconfirmed" {
		t.Fatalf("reason = %q %v", reason, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'metering.usage.send_unconfirmed'`, tenantID); n != 1 {
		t.Fatalf("audit rows = %d", n)
	}
	if n, err := e.svc.ResolveStranded(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep = %d %v", n, err)
	}
	if due, err := e.svc.ClaimDue(ctx, &sc); err != nil || len(due) != 0 || usagePosts(e) != 0 {
		t.Fatalf("an unconfirmed claim was taken again: %d %v posts=%d", len(due), err, usagePosts(e))
	}

	// A send that started, whose finish never reached the database.
	e2, slug, tenant2 := setupOneRollup(t, "zs-sweep-s")
	failFinalize(t, e2, -1)
	if report, err := e2.svc.Forward(ctx); err != nil || report.Failed != 1 || report.Forwarded != 0 || len(e2.fake.UsageRecords(slug)) != 1 {
		t.Fatalf("forward with the finish failing = %+v %v, records %d", report, err, len(e2.fake.UsageRecords(slug)))
	}
	clearFinalizeFailure(t, e2)
	if n, err := e2.svc.ResolveStranded(ctx); err != nil || n != 0 {
		t.Fatalf("sweep before the deadline = %d %v", n, err)
	}
	e2.clock.advance(time.Hour)
	if n, err := e2.svc.ResolveStranded(ctx); err != nil || n != 1 {
		t.Fatalf("sweep after the deadline = %d %v", n, err)
	}
	if err := e2.db.Pool.QueryRow(ctx, `SELECT closure_unresolved_reason FROM usage_rollups WHERE tenant_id = $1`, tenant2).Scan(&reason); err != nil || reason != "usage_send_unconfirmed" {
		t.Fatalf("reason = %q %v", reason, err)
	}
	posts := usagePosts(e2)
	if _, err := e2.svc.Forward(ctx); err != nil || usagePosts(e2) != posts || len(e2.fake.UsageRecords(slug)) != 1 {
		t.Fatalf("usage was sent again: posts %d -> %d", posts, usagePosts(e2))
	}
}

func scopeOf(t *testing.T, e *env, tenantID idgen.UUID) scope.Scope {
	t.Helper()
	var sc scope.Scope
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT brand_id, id FROM tenants WHERE id = $1`, tenantID).Scan(&sc.Brand, &sc.Tenant); err != nil {
		t.Fatal(err)
	}
	return sc
}
