package tenant

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// closureActor is who the closure pipeline acts as in the audit trail.
var closureActor = events.Actor{Type: events.ActorService, ID: "tenant-closure"}

// ClosureRunResult describes one RunClosureInvoice pass.
type ClosureRunResult struct {
	// Busy is true when another owner holds the tenant's lifecycle capability.
	// The pass then did nothing.
	Busy bool
	// ReviewsRecorded counts the review items this pass recorded.
	ReviewsRecorded int
	// UnresolvedReviews counts the review items that still stop closure from
	// completing.
	UnresolvedReviews int
	// Claimed is true when this pass committed the invoice intent.
	Claimed   bool
	Execution ClosureExecutionResult
	// ManualReview is true when a person must act before closure can
	// complete: a review item is unresolved, or the invoice outcome is unknown
	// or did not match.
	ManualReview bool
}

// RunClosureInvoice runs the closure invoice for one closing tenant under the
// tenant's lifecycle capability, in a fixed order: it records the review items
// the tenant needs, commits the invoice intent, and executes the invoice. It
// never completes the closure.
//
// Every database step is a short transaction of the guard, so no transaction
// and no row lock is open while Kill Bill is called; only the guard's advisory
// lock is held. The guard treats any statement error as the end of its
// capability, so a database failure ends the pass. The pass returns the error
// and the next pass resumes, because every outcome is already durable and the
// invoice is never posted twice.
//
// Unresolved legacy usage does not stop the invoice, but it stops completion,
// and the result reports it as manual review, as it does an invoice outcome
// that is unknown or did not match, at once and without an error.
//
// A caller that loops on the error must back off: an error is a database or
// Kill Bill failure, and the next pass resumes from what is durable, but a
// loop that retries without waiting only adds load to the thing that failed.
func RunClosureInvoice(ctx context.Context, pool *pgxpool.Pool, eng Engines, sc scope.Scope, correlationID string) (result ClosureRunResult, err error) {
	guard, got, err := AcquireLifecycleGuard(ctx, pool, sc)
	if err != nil {
		return result, err
	}
	if !got {
		result.Busy = true
		return result, nil
	}
	defer func() {
		if releaseErr := guard.Release(); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()
	reviews, err := RecordClosureReviews(ctx, guard, sc, closureActor, correlationID)
	if err != nil {
		return result, err
	}
	result.ReviewsRecorded = reviews.Recorded
	claim, err := ProcessClosureInvoiceOnce(ctx, guard, eng, sc)
	if err != nil {
		return result, err
	}
	result.Claimed = claim.Claimed
	execution, execErr := ExecuteClosureInvoiceOnce(ctx, guard, eng, sc)
	result.Execution = execution
	if execErr != nil && !IsClosureManualReview(execErr) {
		// An invoice POST whose outcome is unknown has just been recorded as
		// post_unknown, and so has a readback that did not match. Both wait for
		// a person and are never retried, so the pass that records them reports
		// manual review at once and no worker retries for nothing.
		review, reviewErr := closureOutcomeNeedsReview(ctx, guard, sc)
		if reviewErr != nil || !review {
			return result, execErr
		}
		execErr = errClosureManualReview
	}
	items, err := ListClosureReviews(ctx, guard, sc)
	if err != nil {
		return result, err
	}
	result.UnresolvedReviews = blockingClosureReviews(items)
	result.ManualReview = execErr != nil || result.UnresolvedReviews > 0
	return result, nil
}

// blockingClosureReviews counts the unresolved items that still gate closure.
// An unresolved item whose count a resolved item of the same kind covers has
// been superseded: it stays listed but no longer gates.
func blockingClosureReviews(items []ClosureReview) int {
	covered := map[string]int64{}
	for _, it := range items {
		if it.Resolved && it.Count > covered[it.Kind] {
			covered[it.Kind] = it.Count
		}
	}
	blocking := 0
	for _, it := range items {
		if !it.Resolved && it.Count > covered[it.Kind] {
			blocking++
		}
	}
	return blocking
}

// closureOutcomeNeedsReview reports whether the tenant's closure invoice
// attempt has an outcome that only a person can resolve.
func closureOutcomeNeedsReview(ctx context.Context, q Querier, sc scope.Scope) (bool, error) {
	args, err := sc.Bind()
	if err != nil {
		return false, err
	}
	var review bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_closure_invoice_outcomes o
JOIN tenant_closure_invoice_attempts a ON a.id = o.attempt_id
WHERE a.brand_id = $1 AND a.tenant_id = $2 AND o.kind IN ('post_unknown', 'read_mismatch'))`, args...).Scan(&review); err != nil {
		return false, unavailable(err)
	}
	return review, nil
}
