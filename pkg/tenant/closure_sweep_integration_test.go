//go:build integration

package tenant_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// fakeClosureSteps stands in for the Kill Bill cancellation and block.
type fakeClosureSteps struct {
	prepared tenant.ClosurePrepared
	block    bool
	mu       sync.Mutex
	prepare  []scope.Scope
	blocks   atomic.Int64
}

func (f *fakeClosureSteps) Prepare(_ context.Context, _ tenant.DB, sc scope.Scope) (tenant.ClosurePrepared, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepare = append(f.prepare, sc)
	return f.prepared, nil
}

func (f *fakeClosureSteps) Block(_ context.Context, _ tenant.DB, _ scope.Scope) (bool, error) {
	f.blocks.Add(1)
	return f.block, nil
}

func settlementStatus(t *testing.T, e *env, tn tenant.Tenant) string {
	t.Helper()
	var status string
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT status FROM tenant_closure_settlements WHERE tenant_id = $1`, tn.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func ready() tenant.ClosurePrepared {
	return tenant.ClosurePrepared{Cancelled: true, Drained: true, TargetDate: time.Now().UTC()}
}

// One pass drives a closing tenant as far as its conditions allow: the steps
// before finance, the invoice, and the move to blocking, where it waits for
// the account block.
func TestClosureSweepDrivesATenantFromCancellingToBlocking(t *testing.T) {
	m := &kbMode{noClaim: true, startCancelling: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	steps := &fakeClosureSteps{prepared: ready()}
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}).SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := settlementStatus(t, e, tn); got != "blocking" || posts.Load() != 1 {
		t.Fatalf("status = %q posts = %d, want blocking and 1", got, posts.Load())
	}
	if report.ClosuresChecked != 1 || report.ClosuresAdvanced != 1 || report.ClosuresWaiting != 0 {
		t.Fatalf("report = %+v", report)
	}
	var changes int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_state_changed'`, tn.ID).Scan(&changes); err != nil || changes != 2 {
		t.Fatalf("state changes audited = %d, %v", changes, err)
	}
	// The next pass waits for the account block and does not post again.
	again, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}).SweepOnce(context.Background())
	if err != nil || settlementStatus(t, e, tn) != "blocking" || posts.Load() != 1 {
		t.Fatalf("second pass = %+v %v status = %q posts = %d", again, err, settlementStatus(t, e, tn), posts.Load())
	}
}

// With the account blocked, the tenant completes, unless legacy usage is
// still unreviewed: then completion is manual review.
func TestClosureSweepCompletesAfterBlockAndStopsAtUnreviewedUsage(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		m := &kbMode{noClaim: true, startCancelling: true}
		var posts, lookups atomic.Int64
		e, tn := claimedClosure(t, m, &posts, &lookups)
		if legacy {
			addLegacyEvent(t, e, tn)
		}
		steps := &fakeClosureSteps{prepared: ready(), block: true}
		report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}).SweepOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		want, manual := "completed", 0
		if legacy {
			want, manual = "blocking", 1
		}
		if got := settlementStatus(t, e, tn); got != want || report.ClosuresManualReview != manual {
			t.Fatalf("legacy=%v: status = %q manual review = %d, want %q and %d (report %+v)", legacy, got, report.ClosuresManualReview, want, manual, report)
		}
	}
}

// A tenant whose capability another owner holds is skipped, and nothing is
// called for it.
func TestClosureSweepSkipsABusyTenant(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	guard, got, err := tenant.AcquireLifecycleGuard(context.Background(), e.db.Pool, tn.Scope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	defer func() { _ = guard.Release() }()
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(context.Background())
	if err != nil || report.ClosuresBusy != 1 || report.ClosuresAdvanced != 0 || posts.Load() != 0 || lookups.Load() != 0 || settlementStatus(t, e, tn) != "finance_pending" {
		t.Fatalf("report = %+v %v posts=%d lookups=%d", report, err, posts.Load(), lookups.Load())
	}
}

// Without steps configured, a tenant at cancelling waits, and one at finance
// is still driven.
func TestClosureSweepWithoutStepsWaitsAtCancelling(t *testing.T) {
	waiting := &kbMode{noClaim: true, startCancelling: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, waiting, &posts, &lookups)
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(context.Background())
	if err != nil || report.ClosuresWaiting != 1 || report.ClosuresAdvanced != 0 || settlementStatus(t, e, tn) != "cancelling" {
		t.Fatalf("cancelling = %+v %v status %q", report, err, settlementStatus(t, e, tn))
	}
	driven := &kbMode{noClaim: true}
	var posts2, lookups2 atomic.Int64
	e2, tn2 := claimedClosure(t, driven, &posts2, &lookups2)
	report, err = (&tenant.Sweeper{DB: e2.db.Pool, Engines: e2.eng}).SweepOnce(context.Background())
	if err != nil || report.ClosuresAdvanced != 1 || settlementStatus(t, e2, tn2) != "blocking" {
		t.Fatalf("finance_pending = %+v %v status %q", report, err, settlementStatus(t, e2, tn2))
	}
}

// A pass drives at most ClosureWork tenants, and the next pass resumes after
// the last one it looked at, so every closing tenant is reached in turn.
func TestClosureSweepResumesFromItsCursorWithinTheWorkBudget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var tenants []tenant.Tenant
	for i := 0; i < 3; i++ {
		p := e.person("owner" + string(rune('a'+i)) + "@example.test")
		tn := e.mustOrg(p, "Tenant "+string(rune('A'+i)))
		if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status) VALUES ($1, $2, now(), 'cancelling')`, tn.BrandID, tn.ID); err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, tn)
	}
	steps := &fakeClosureSteps{}
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, ClosureWork: 1}
	seen := map[string]bool{}
	for pass := 1; pass <= 3; pass++ {
		report, err := sweeper.SweepOnce(ctx)
		if err != nil || report.ClosuresChecked != 1 || report.ClosuresWaiting != 1 {
			t.Fatalf("pass %d = %+v %v, want one tenant checked", pass, report, err)
		}
		steps.mu.Lock()
		last := steps.prepare[len(steps.prepare)-1]
		steps.mu.Unlock()
		if seen[last.TenantID()] {
			t.Fatalf("pass %d looked at %s again", pass, last.TenantID())
		}
		seen[last.TenantID()] = true
	}
	var cursor bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) = 1 FROM sweep_cursor WHERE kind = 'closure'`).Scan(&cursor); err != nil || !cursor {
		t.Fatalf("the closure cursor row = %v, %v", cursor, err)
	}
	if report, err := sweeper.SweepOnce(ctx); err != nil || report.ClosuresChecked != 0 {
		t.Fatalf("pass 4 = %+v %v, want none due", report, err)
	}
	_ = tenants
}
