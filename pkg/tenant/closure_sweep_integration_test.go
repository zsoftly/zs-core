//go:build integration

package tenant_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// fakeClosureSteps stands in for the Kill Bill cancellation and block.
type fakeClosureSteps struct {
	prepared tenant.ClosurePrepared
	// prepareErr, when set, is what Prepare returns.
	prepareErr error
	// onPrepare, when set, runs inside Prepare, for a step that is slow or stuck.
	onPrepare func(ctx context.Context, sc scope.Scope)
	// onBlock, when set, runs inside Block.
	onBlock func(ctx context.Context)
	// ignoreContext makes Prepare return its answer even when its context has
	// ended, as a step that finished just as the budget ran out does.
	ignoreContext bool
	block         bool
	mu            sync.Mutex
	prepare       []scope.Scope
	blocks        atomic.Int64
}

func (f *fakeClosureSteps) Prepare(ctx context.Context, _ tenant.DB, sc scope.Scope) (tenant.ClosurePrepared, error) {
	f.mu.Lock()
	f.prepare = append(f.prepare, sc)
	hook := f.onPrepare
	f.mu.Unlock()
	if hook != nil {
		hook(ctx, sc)
	}
	if err := ctx.Err(); err != nil && !f.ignoreContext {
		return tenant.ClosurePrepared{}, err
	}
	if f.prepareErr != nil {
		return tenant.ClosurePrepared{}, f.prepareErr
	}
	return f.prepared, nil
}

func (f *fakeClosureSteps) Block(ctx context.Context, _ tenant.DB, _ scope.Scope) (bool, error) {
	f.blocks.Add(1)
	if f.onBlock != nil {
		f.onBlock(ctx)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
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

// threeClosing creates three closing tenants waiting at cancelling.
func threeClosing(t *testing.T, e *env) []tenant.Tenant {
	t.Helper()
	ctx := context.Background()
	var out []tenant.Tenant
	for i := 0; i < 3; i++ {
		tn := e.mustOrg(e.person("owner"+string(rune('a'+i))+"@example.test"), "Tenant "+string(rune('A'+i)))
		if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status) VALUES ($1, $2, now(), 'cancelling')`, tn.BrandID, tn.ID); err != nil {
			t.Fatal(err)
		}
		out = append(out, tn)
	}
	return out
}

func prepareOrder(f *fakeClosureSteps) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, sc := range f.prepare {
		ids = append(ids, sc.TenantID())
	}
	return ids
}

// The pass budget ends the pass. A tenant whose step uses it up is the one
// that pays: it is backed off and the cursor moves past it, so the next pass
// reaches the next tenant and a stuck tenant cannot starve the others.
func TestClosureSweepPassBudgetEndsThePassAndMovesPastTheTenantThatUsedIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenants := threeClosing(t, e)
	steps := &fakeClosureSteps{onPrepare: func(ctx context.Context, _ scope.Scope) { <-ctx.Done() }}
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, PassBudget: 400 * time.Millisecond}
	seen := map[string]bool{}
	for pass := 1; pass <= 3; pass++ {
		began := time.Now()
		report, err := sweeper.SweepOnce(ctx)
		if elapsed := time.Since(began); err != nil || elapsed > 3*time.Second || report.ClosuresFailed != 1 {
			t.Fatalf("pass %d = %+v %v after %v, want one failed tenant and the pass ended near its budget", pass, report, err, elapsed)
		}
		order := prepareOrder(steps)
		last := order[len(order)-1]
		if seen[last] {
			t.Fatalf("pass %d reached %s again; order %v", pass, last, order)
		}
		seen[last] = true
	}
	for _, tn := range tenants {
		var attempts int
		var lastErr string
		if err := e.db.Pool.QueryRow(ctx, `SELECT attempts, coalesce(last_error, '') FROM tenant_closure_settlements WHERE tenant_id = $1`, tn.ID).Scan(&attempts, &lastErr); err != nil || attempts != 1 || lastErr != "step_timeout" {
			t.Fatalf("tenant %s: attempts = %d last_error = %q, %v, want 1 and step_timeout", tn.ID, attempts, lastErr, err)
		}
	}
}

// A closure step has its own bound. One that overruns it fails that tenant
// with a timeout and the pass goes on to the next tenant.
func TestClosureSweepCallBudgetFailsOneTenantAndGoesOn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	threeClosing(t, e)
	steps := &fakeClosureSteps{onPrepare: func(ctx context.Context, _ scope.Scope) { <-ctx.Done() }}
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, CallBudget: 100 * time.Millisecond}
	began := time.Now()
	report, err := sweeper.SweepOnce(ctx)
	if err != nil || report.ClosuresChecked != 3 || report.ClosuresFailed != 3 || time.Since(began) > 5*time.Second {
		t.Fatalf("pass = %+v %v after %v, want three tenants each failed by its call bound", report, err, time.Since(began))
	}
	var timeouts int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_settlements WHERE last_error = 'step_timeout' AND attempts = 1`).Scan(&timeouts); err != nil || timeouts != 3 {
		t.Fatalf("step_timeout settlements = %d, %v", timeouts, err)
	}
}

// A pass stopped by its caller, as on shutdown, is not a failure of the tenant
// it was in. The tenant keeps its place: after the restart it is the first one
// looked at again.
func TestClosureSweepStoppedByItsCallerResumesAtTheSameTenant(t *testing.T) {
	e := newEnv(t)
	threeClosing(t, e)
	stopped, stop := context.WithCancel(context.Background())
	defer stop()
	steps := &fakeClosureSteps{onPrepare: func(ctx context.Context, _ scope.Scope) {
		stop()
		<-ctx.Done()
	}}
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}
	began := time.Now()
	report, err := sweeper.SweepOnce(stopped)
	if err == nil || time.Since(began) > 5*time.Second || report.ClosuresFailed != 0 {
		t.Fatalf("stopped pass = %+v %v after %v, want an error, no failed tenant and a prompt return", report, err, time.Since(began))
	}
	interrupted := prepareOrder(steps)
	if len(interrupted) != 1 {
		t.Fatalf("the stopped pass reached %d tenants, want 1", len(interrupted))
	}
	var attempts int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT coalesce(max(attempts), 0) FROM tenant_closure_settlements`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts after the stop = %d, %v, want none recorded", attempts, err)
	}
	// The restart: nothing is stuck now, and the first tenant looked at is the
	// one that was interrupted.
	steps.mu.Lock()
	steps.onPrepare = nil
	steps.mu.Unlock()
	if _, err := sweeper.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	order := prepareOrder(steps)
	if len(order) < 2 || order[1] != interrupted[0] {
		t.Fatalf("after the restart the first tenant was %v, want %s again; order %v", order, interrupted[0], order)
	}
}

func delaySeconds(t *testing.T, e *env, tn tenant.Tenant) (attempts int, delay float64, lastError string) {
	t.Helper()
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT attempts, extract(epoch FROM next_attempt_at - now()), coalesce(last_error, '')
FROM tenant_closure_settlements WHERE tenant_id = $1`, tn.ID).Scan(&attempts, &delay, &lastError); err != nil {
		t.Fatal(err)
	}
	return attempts, delay, lastError
}

func makeDue(t *testing.T, e *env, tn tenant.Tenant) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE tenant_closure_settlements SET next_attempt_at = now() - interval '1 second' WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
}

// A failing step backs the tenant off: ten seconds, then twice as long each
// time, up to an hour, and the settlement keeps the name of the error and
// never its text.
func TestClosureSweepBacksOffAFailingTenantAndKeepsOnlyTheErrorName(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	closing := threeClosing(t, e)
	tn := closing[0]
	// Only this tenant is due: the others wait a day, so the one pass that
	// drives one tenant always drives this one.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET next_attempt_at = now() + interval '1 day' WHERE tenant_id <> $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	steps := &fakeClosureSteps{prepareErr: errors.New("secret-marker: Kill Bill said the token is hunter2")}
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, ClosureWork: 1}
	within := func(what string, got, low, high float64) {
		t.Helper()
		if got < low || got > high {
			t.Fatalf("%s = %.1f seconds, want %.0f to %.0f", what, got, low, high)
		}
	}
	for i, want := range []float64{10, 20, 40} {
		makeDue(t, e, tn)
		// ClosureWork 1 and the cursor: reach the tenant under test first.
		if _, err := e.db.Pool.Exec(ctx, `DELETE FROM sweep_cursor WHERE kind = 'closure'`); err != nil {
			t.Fatal(err)
		}
		report, err := sweeper.SweepOnce(ctx)
		if err != nil || report.ClosuresFailed != 1 {
			t.Fatalf("pass %d = %+v %v", i+1, report, err)
		}
		attempts, delay, lastError := delaySeconds(t, e, tn)
		if attempts != i+1 || lastError != "internal_error" {
			t.Fatalf("pass %d: attempts = %d last_error = %q", i+1, attempts, lastError)
		}
		within("backoff after attempt "+string(rune('1'+i)), delay, want-3, want+1)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET attempts = 20, next_attempt_at = now() - interval '1 second' WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `DELETE FROM sweep_cursor WHERE kind = 'closure'`); err != nil {
		t.Fatal(err)
	}
	if _, err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, delay, _ := delaySeconds(t, e, tn)
	within("the capped backoff", delay, 3590, 3601)
	var leaked bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_closure_settlements WHERE last_error ILIKE '%hunter2%' OR last_error ILIKE '%secret-marker%')`).Scan(&leaked); err != nil || leaked {
		t.Fatalf("the error text reached the settlement: %v %v", leaked, err)
	}
}

// A tenant that waits for a step, or for a person, is looked at again after
// five minutes, not on the next pass.
func TestClosureSweepRechecksAWaitingTenantAfterFiveMinutes(t *testing.T) {
	m := &kbMode{noClaim: true, startCancelling: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	steps := &fakeClosureSteps{}
	if report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}).SweepOnce(context.Background()); err != nil || report.ClosuresWaiting != 1 {
		t.Fatalf("pass = %+v %v", report, err)
	}
	attempts, delay, _ := delaySeconds(t, e, tn)
	if attempts != 0 || delay < 290 || delay > 301 {
		t.Fatalf("waiting: attempts = %d recheck in %.0f seconds, want 0 and about 300", attempts, delay)
	}
	// A pass right after does not look at it again.
	if report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}).SweepOnce(context.Background()); err != nil || report.ClosuresChecked != 0 {
		t.Fatalf("second pass = %+v %v, want none due", report, err)
	}
}

// Manual review waits five minutes too.
func TestClosureSweepRechecksAManualReviewTenantAfterFiveMinutes(t *testing.T) {
	m := &kbMode{noClaim: true, startCancelling: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	addLegacyEvent(t, e, tn)
	steps := &fakeClosureSteps{prepared: ready(), block: true}
	if report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}).SweepOnce(context.Background()); err != nil || report.ClosuresManualReview != 1 {
		t.Fatalf("pass = %+v %v", report, err)
	}
	attempts, delay, _ := delaySeconds(t, e, tn)
	if attempts != 0 || delay < 290 || delay > 301 {
		t.Fatalf("manual review: attempts = %d recheck in %.0f seconds, want 0 and about 300", attempts, delay)
	}
}

// The scan budget stops a pass, and one pass looks at no tenant twice.
func TestClosureSweepScanBudgetAndNoTenantTwiceInAPass(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenants := threeClosing(t, e)
	steps := &fakeClosureSteps{}
	scanned, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, ClosureScan: 2}).SweepOnce(ctx)
	if err != nil || scanned.ClosuresChecked != 2 || len(prepareOrder(steps)) != 2 {
		t.Fatalf("pass with a scan budget of 2 = %+v %v, steps %v", scanned, err, prepareOrder(steps))
	}
	// A plain pass reaches the rest, each tenant once.
	if _, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}).SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	order := prepareOrder(steps)
	seen := map[string]int{}
	for _, id := range order {
		seen[id]++
	}
	if len(order) != 3 || len(seen) != 3 {
		t.Fatalf("steps ran %v, want each of the %d tenants once", order, len(tenants))
	}
}

// When no tenant is due, the cursor wraps to the start.
func TestClosureSweepCursorWrapsWhenNoTenantIsDue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	threeClosing(t, e)
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: &fakeClosureSteps{}, ClosureWork: 1}
	for i := 0; i < 3; i++ {
		if _, err := sweeper.SweepOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var positioned bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_tenant_id IS NOT NULL FROM sweep_cursor WHERE kind = 'closure'`).Scan(&positioned); err != nil || !positioned {
		t.Fatalf("the cursor after three passes: positioned = %v, %v", positioned, err)
	}
	if report, err := sweeper.SweepOnce(ctx); err != nil || report.ClosuresChecked != 0 {
		t.Fatalf("pass with none due = %+v %v", report, err)
	}
	var wrapped bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_tenant_id IS NULL AND last_eligible_at IS NULL FROM sweep_cursor WHERE kind = 'closure'`).Scan(&wrapped); err != nil || !wrapped {
		t.Fatalf("the cursor after an empty page: wrapped = %v, %v", wrapped, err)
	}
}

// A step that returns just as the pass budget ends is not a failure of the
// pass. The tenant was handled, the cursor moves past it, and the pass ends
// without an error.
func TestClosureSweepEndsCleanlyWhenTheBudgetRunsOutAtATenantBoundary(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	threeClosing(t, e)
	steps := &fakeClosureSteps{ignoreContext: true, onPrepare: func(ctx context.Context, _ scope.Scope) { <-ctx.Done() }}
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, PassBudget: 300 * time.Millisecond}
	report, err := sweeper.SweepOnce(ctx)
	if err != nil || report.ClosuresChecked != 1 || report.ClosuresWaiting != 1 {
		t.Fatalf("pass = %+v %v, want one waiting tenant and no error", report, err)
	}
	first := prepareOrder(steps)
	if _, err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	order := prepareOrder(steps)
	if len(order) != 2 || order[1] == first[0] {
		t.Fatalf("the next pass reached %v, want a tenant other than %s", order, first[0])
	}
}

// A budget that is already spent when the pass starts is a pass that does
// nothing, not an error.
func TestClosureSweepWithASpentBudgetEndsCleanly(t *testing.T) {
	e := newEnv(t)
	threeClosing(t, e)
	steps := &fakeClosureSteps{}
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, PassBudget: time.Nanosecond}).SweepOnce(context.Background())
	if err != nil || len(prepareOrder(steps)) != 0 {
		t.Fatalf("pass = %+v %v steps %v, want no error and no step", report, err, prepareOrder(steps))
	}
}

// The closing step has the call bound too. One that overruns it fails the
// tenant with a timeout, and the tenant stays at blocking.
func TestClosureSweepCallBudgetBoundsTheBlockStep(t *testing.T) {
	m := &kbMode{noClaim: true, startCancelling: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	steps := &fakeClosureSteps{prepared: ready(), block: true, onBlock: func(ctx context.Context) { <-ctx.Done() }}
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, CallBudget: 200 * time.Millisecond}).SweepOnce(context.Background())
	if err != nil || report.ClosuresFailed != 1 {
		t.Fatalf("pass = %+v %v, want the tenant failed by the call bound", report, err)
	}
	attempts, _, lastError := delaySeconds(t, e, tn)
	if got := settlementStatus(t, e, tn); got != "blocking" || attempts != 1 || lastError != "step_timeout" {
		t.Fatalf("status = %q attempts = %d last_error = %q, want blocking, 1 and step_timeout", got, attempts, lastError)
	}
}

// When the caller stops the pass right after a step returned, the schedule of
// the tenant is still written, because it is written with a context of its own.
func TestClosureSweepWritesTheScheduleAfterTheCallerStopped(t *testing.T) {
	e := newEnv(t)
	threeClosing(t, e)
	stopped, stop := context.WithCancel(context.Background())
	defer stop()
	steps := &fakeClosureSteps{ignoreContext: true, onPrepare: func(context.Context, scope.Scope) { stop() }}
	if _, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps, ClosureWork: 1}).SweepOnce(stopped); err == nil {
		t.Fatal("a pass the caller stopped returned no error")
	}
	first := prepareOrder(steps)
	if len(first) != 1 {
		t.Fatalf("steps = %v, want one", first)
	}
	var rescheduled int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM tenant_closure_settlements WHERE attempts = 0 AND next_attempt_at > now() + interval '290 seconds' AND next_attempt_at < now() + interval '301 seconds'`).Scan(&rescheduled); err != nil || rescheduled != 1 {
		t.Fatalf("tenants rescheduled five minutes ahead after the stop = %d, %v, want 1", rescheduled, err)
	}
}

// The closure pass comes after the Organization sync, so a closure pass that
// ends at once by its budget does not keep the sync from happening.
func TestSweepSyncsOrganizationsEvenWhenTheClosureBudgetIsSpent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := e.mustOrg(e.person("sync-first@example.test"), "Sync first")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET name = 'Sync first renamed' WHERE id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: &fakeClosureSteps{}, PassBudget: time.Nanosecond}).SweepOnce(ctx)
	if err != nil || report.OrganizationsUpdated != 1 {
		t.Fatalf("pass = %+v %v, want the Organization synced", report, err)
	}
	if org, _, _ := e.fake.Organization(e.brand.Realm, tn.PublicID()); org.Name != "Sync first renamed" {
		t.Fatalf("the Organization is named %q", org.Name)
	}
}

// When a closure step fails and the release of the tenant capability fails
// too, the step failure stays the classified cause, and the release failure is
// logged on its own and counted.
func TestClosureSweepLogsAndCountsAReleaseFailureBesideAStepFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := threeClosing(t, e)[0]
	_ = tn
	tracer := &sweepUnlockCancelTracer{child: true}
	cfg, err := pgxpool.ParseConfig(e.db.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	logs := &lockedBuffer{}
	// The release is cancelled only from the closure step on, so the sync
	// before it runs as usual.
	steps := &fakeClosureSteps{prepareErr: errors.New("step failed"), onPrepare: func(context.Context, scope.Scope) { tracer.enabled.Store(true) }}
	report, _ := (&tenant.Sweeper{DB: pool, Engines: e.eng, Closure: steps, ClosureWork: 1, Log: slog.New(slog.NewTextHandler(logs, nil))}).SweepOnce(ctx)
	if tracer.hits.Load() == 0 {
		t.Fatal("the child release was never cancelled")
	}
	if report.Errors != 1 || !strings.Contains(logs.String(), "closure capability release failed") {
		t.Fatalf("report = %+v, log = %q, want one counted error and the release failure logged", report, logs.String())
	}
	if strings.Contains(logs.String(), "closure step failed") && !strings.Contains(logs.String(), "step failed") {
		t.Fatal("the step failure lost its own entry")
	}
}
