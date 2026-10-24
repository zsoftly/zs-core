//go:build integration

package tenant_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// addLegacyEvent gives the tenant one raw event accepted before dispatch
// protocol 1, which no rollup holds.
func addLegacyEvent(t *testing.T, e *env, tn tenant.Tenant) {
	t.Helper()
	ctx := context.Background()
	meterID := idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO meters (id, brand_id, slug, unit, killbill_unit) VALUES ($1, $2, 'legacy-meter', 'count', 'api-call')`, meterID, tn.BrandID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_events (id, brand_id, tenant_id, meter_id, event_id, killbill_unit, quantity_micro,
    dimensions, dimensions_key, occurred_at, received_at) VALUES ($1, $2, $3, $4, 'legacy-a', 'api-call', 7, '{}'::jsonb, '{}', now(), now())`,
		idgen.MustNewV7(), tn.BrandID, tn.ID, meterID); err != nil {
		t.Fatal(err)
	}
}

// One pass records the reviews the tenant needs, claims the invoice and
// executes it, in that order. Legacy usage that no one has reviewed does not
// stop the invoice, but it is reported as manual review, because it stops
// closure from completing.
func TestRunClosureInvoiceRecordsReviewsThenClaimsThenExecutes(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	addLegacyEvent(t, e, tn)
	ctx := context.Background()
	result, err := tenant.RunClosureInvoice(ctx, e.db.Pool, e.eng, tn.Scope(), "corr-run")
	if err != nil {
		t.Fatalf("run = %+v %v", result, err)
	}
	if result.Busy || result.ReviewsRecorded != 1 || result.UnresolvedReviews != 1 || !result.Claimed || !result.Execution.Posted || !result.Execution.Evidenced || !result.ManualReview {
		t.Fatalf("result = %+v", result)
	}
	var reviewedFirst bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT bool_and(i.recorded_at < o.created_at) FROM tenant_closure_review_items i, tenant_closure_invoice_outcomes o
WHERE o.kind = 'started'`).Scan(&reviewedFirst); err != nil || !reviewedFirst {
		t.Fatalf("the review was not recorded before the invoice started: %v %v", reviewedFirst, err)
	}
	if posts.Load() != 1 {
		t.Fatalf("posts = %d", posts.Load())
	}
	// A second pass does nothing new and still reports the review.
	again, err := tenant.RunClosureInvoice(ctx, e.db.Pool, e.eng, tn.Scope(), "corr-run-2")
	if err != nil || again.ReviewsRecorded != 0 || again.UnresolvedReviews != 1 || !again.ManualReview || posts.Load() != 1 {
		t.Fatalf("second pass = %+v %v posts=%d", again, err, posts.Load())
	}
}

// A tenant with no legacy usage needs no review.
func TestRunClosureInvoiceNeedsNoReviewWithoutLegacyUsage(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	result, err := tenant.RunClosureInvoice(context.Background(), e.db.Pool, e.eng, tn.Scope(), "corr-run")
	if err != nil || result.ReviewsRecorded != 0 || result.UnresolvedReviews != 0 || result.ManualReview || !result.Execution.Evidenced {
		t.Fatalf("result = %+v %v", result, err)
	}
}

// An unknown outcome of the invoice POST is surfaced as manual review by the
// pass that records it, with no error, so a worker does not retry for nothing.
func TestRunClosureInvoiceSurfacesAnUnknownOutcomeAsManualReviewAtOnce(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	m.post.Store(3)
	e, tn := claimedClosure(t, m, &posts, &lookups)
	ctx := context.Background()
	first, err := tenant.RunClosureInvoice(ctx, e.db.Pool, e.eng, tn.Scope(), "corr-run")
	if err != nil || !first.ManualReview || first.Execution.Evidenced || posts.Load() != 1 {
		t.Fatalf("the pass that met the unknown outcome = %+v %v posts=%d, want manual review and no error", first, err, posts.Load())
	}
	again, err := tenant.RunClosureInvoice(ctx, e.db.Pool, e.eng, tn.Scope(), "corr-run-2")
	if err != nil || !again.ManualReview || again.Execution.Evidenced || posts.Load() != 1 {
		t.Fatalf("second pass = %+v %v posts=%d", again, err, posts.Load())
	}
}

// Another owner of the tenant's lifecycle capability makes the pass busy:
// it returns at once and calls nothing.
func TestRunClosureInvoiceIsBusyWhileAnotherOwnerHoldsTheTenant(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	ctx := context.Background()
	guard, got, err := tenant.AcquireLifecycleGuard(ctx, e.db.Pool, tn.Scope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	defer func() { _ = guard.Release() }()
	result, err := tenant.RunClosureInvoice(ctx, e.db.Pool, e.eng, tn.Scope(), "corr-run")
	if err != nil || !result.Busy || result.Claimed || posts.Load() != 0 || lookups.Load() != 0 {
		t.Fatalf("result = %+v %v posts=%d lookups=%d", result, err, posts.Load(), lookups.Load())
	}
}

// While Kill Bill is being called, the pass holds the tenant capability, an
// advisory lock, and nothing else: no open transaction and no row lock on the
// attempt or the settlement.
func TestRunClosureInvoiceHoldsNoRowLockDuringTheKillBillCalls(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	var checks atomic.Int64
	var e *env
	var tn tenant.Tenant
	m.inFlight = func() {
		ctx := context.Background()
		checks.Add(1)
		var open int
		if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND xact_start IS NOT NULL`).Scan(&open); err != nil || open != 0 {
			t.Errorf("open transactions during the Kill Bill call = %d, %v", open, err)
		}
		var advisory int
		if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&advisory); err != nil || advisory == 0 {
			t.Errorf("the tenant capability is not held during the Kill Bill call: %d, %v", advisory, err)
		}
		tx, err := e.db.Pool.Begin(ctx)
		if err != nil {
			t.Errorf("observer: %v", err)
			return
		}
		defer tx.Rollback(ctx)
		for _, table := range []string{"tenant_closure_invoice_attempts", "tenant_closure_settlements"} {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM `+table+` WHERE tenant_id = $1 FOR UPDATE NOWAIT`, tn.ID); err != nil {
				t.Errorf("a row of %s is locked during the Kill Bill call: %v", table, err)
			}
		}
	}
	e, tn = claimedClosure(t, m, &posts, &lookups)
	result, err := tenant.RunClosureInvoice(context.Background(), e.db.Pool, e.eng, tn.Scope(), "corr-run")
	if err != nil || !result.Execution.Evidenced {
		t.Fatalf("result = %+v %v", result, err)
	}
	if checks.Load() < 2 {
		t.Fatalf("the checks ran %d times, want the dry run and the POST", checks.Load())
	}
}

// Two passes that start together run the invoice once: one gets the tenant,
// the other is busy, and the POST is made once.
func TestRunClosureInvoiceTwoConcurrentPassesPostOnce(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	m.post.Store(2)
	e, tn := claimedClosure(t, m, &posts, &lookups)
	ctx := context.Background()
	type pass struct {
		result tenant.ClosureRunResult
		err    error
	}
	results := make(chan pass, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, err := tenant.RunClosureInvoice(ctx, e.db.Pool, e.eng, tn.Scope(), "corr-concurrent")
			results <- pass{r, err}
		}()
	}
	var busy, executed int
	for i := 0; i < 2; i++ {
		p := <-results
		if p.err != nil {
			t.Fatalf("pass = %+v %v", p.result, p.err)
		}
		if p.result.Busy {
			busy++
		}
		if p.result.Execution.Evidenced {
			executed++
		}
	}
	if busy != 1 || executed != 1 || posts.Load() != 1 {
		t.Fatalf("busy = %d executed = %d posts = %d, want 1, 1 and 1", busy, executed, posts.Load())
	}
}

// A pass stopped while the POST is in flight leaves the invoice ID recorded,
// and the next pass recovers it by reading, with no second POST.
func TestRunClosureInvoiceStoppedMidPassResumesWithoutAnotherPost(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	m.post.Store(2)
	e, tn := claimedClosure(t, m, &posts, &lookups)
	ctx := context.Background()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for cctx.Err() == nil {
			var started int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'started'`).Scan(&started); err == nil && started == 1 {
				time.Sleep(150 * time.Millisecond)
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	_, _ = tenant.RunClosureInvoice(cctx, e.db.Pool, e.eng, tn.Scope(), "corr-stopped")
	var recorded int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_closure_invoice_outcomes WHERE kind = 'invoice_id'`).Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("invoice_id outcomes after the stop = %d, %v", recorded, err)
	}
	resumed, err := tenant.RunClosureInvoice(ctx, e.db.Pool, e.eng, tn.Scope(), "corr-resumed")
	if err != nil || !resumed.Execution.Evidenced || resumed.Execution.Posted || posts.Load() != 1 {
		t.Fatalf("resumed pass = %+v %v posts=%d", resumed, err, posts.Load())
	}
}

// The review functions run on the capability too: their statements are
// accepted by its scope check, and a review recorded and resolved under it is
// visible afterwards.
func TestClosureReviewFunctionsRunOnTheLifecycleGuard(t *testing.T) {
	m := &kbMode{noClaim: true}
	var posts, lookups atomic.Int64
	e, tn := claimedClosure(t, m, &posts, &lookups)
	addLegacyEvent(t, e, tn)
	ctx := context.Background()
	guard, got, err := tenant.AcquireLifecycleGuard(ctx, e.db.Pool, tn.Scope())
	if err != nil || !got {
		t.Fatalf("guard = %v %v", got, err)
	}
	if rec, err := tenant.RecordClosureReviews(ctx, guard, tn.Scope(), reviewActor, "corr-guard"); err != nil || rec.Recorded != 1 {
		t.Fatalf("record on the guard = %+v %v", rec, err)
	}
	items, err := tenant.ListClosureReviews(ctx, guard, tn.Scope())
	if err != nil || len(items) != 1 {
		t.Fatalf("list on the guard = %+v %v", items, err)
	}
	if err := tenant.ResolveClosureReview(ctx, guard, tn.Scope(), items[0].ID, reviewActor, "reviewed on the guard", "corr-guard"); err != nil {
		t.Fatalf("resolve on the guard = %v", err)
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	after, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(after) != 1 || !after[0].Resolved {
		t.Fatalf("after release = %+v %v", after, err)
	}
}
