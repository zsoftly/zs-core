package tenant

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// ClosurePrepared is what the steps before finance report for one tenant.
type ClosurePrepared struct {
	Cancelled  bool
	Drained    bool
	TargetDate time.Time
}

// ClosureSteps are the closure steps the sweep cannot do itself. The real Kill
// Bill cancellation and account block plug in here. Both steps must be
// idempotent: a pass can be stopped or fail at any point, and the next pass
// calls the same step again, so a step reads what is already done before it
// does anything and does only what is missing.
type ClosureSteps interface {
	// Prepare cancels what must be cancelled and reports whether the
	// cancellation and the usage drain are complete, and the date finance
	// invoices to.
	Prepare(ctx context.Context, db DB, sc scope.Scope) (ClosurePrepared, error)
	// Block blocks the account and reports whether it is blocked.
	Block(ctx context.Context, db DB, sc scope.Scope) (bool, error)
}

const (
	closureScanBudget = 500
	closureWorkBudget = 50
	// closureWaitingRetry is how long a tenant that waits for a person or for
	// a step waits before the sweep looks at it again, so it is not looked at
	// on every pass.
	closureWaitingRetry = 5 * time.Minute
	closureRetryFirst   = 10 * time.Second
	closureRetryMax     = time.Hour
)

type closureOutcome int

const (
	closureAdvanced closureOutcome = iota
	closureWaiting
	closureManualReviewOutcome
	closureFailed
	closureDone
)

type closureCandidate struct {
	scope      scope.Scope
	eligibleAt time.Time
	attempts   int
}

// settleContext is a short context of its own for what a pass records after
// its own context is spent.
func settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), lifecycleCleanupLimit)
}

// errClosurePassOver says the pass budget ended while a statement was in
// flight. The statement failed, so the lifecycle capability of the pass is
// spent, and the whole sweep pass ends cleanly: the rest of it would only
// meet a released capability.
var errClosurePassOver = errors.New("the closure pass budget ended")

// passError is what a pass returns when err stopped it. A pass that the caller
// stopped, as on shutdown, returns that stop. A pass whose own budget ran out
// is over, which is not a failure, when the error is the context's own, and a
// caller sees that as errClosurePassOver. Anything else is a real failure, even
// if it happened as the budget ended.
func passError(ctx, passCtx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if stop := ctx.Err(); stop != nil {
		return stop
	}
	if passCtx.Err() != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		return errClosurePassOver
	}
	return err
}

// passEnded reports whether the pass must stop before it starts anything new,
// and with what. The caller's stop is returned as it is. A spent budget is a
// clean end: nothing was in flight, so the capability is intact.
func passEnded(ctx, passCtx context.Context) (bool, error) {
	if stop := ctx.Err(); stop != nil {
		return true, stop
	}
	return passCtx.Err() != nil, nil
}

func (s *Sweeper) passBudget() time.Duration {
	if s.PassBudget <= 0 {
		return 30 * time.Second
	}
	return s.PassBudget
}

func (s *Sweeper) callBudget() time.Duration {
	if s.CallBudget <= 0 {
		return 5 * time.Second
	}
	return s.CallBudget
}

func (s *Sweeper) closureScan() int {
	if s.ClosureScan <= 0 {
		return closureScanBudget
	}
	return s.ClosureScan
}

func (s *Sweeper) closureWork() int {
	if s.ClosureWork <= 0 {
		return closureWorkBudget
	}
	return s.ClosureWork
}

// completeClosures drives the closing tenants that are due, a page at a time
// from a durable cursor, each under its own lifecycle capability, until the
// scan or the work budget of the pass is spent. A tenant whose capability
// another owner holds is skipped. The cursor moves past every tenant looked
// at, and wraps when none is left, so every closing tenant is reached in turn.
func (s *Sweeper) completeClosures(ctx context.Context, guard *LifecycleSweep, correlationID string, report *SweepReport) error {
	passCtx, cancel := context.WithTimeout(ctx, s.passBudget())
	defer cancel()
	passCutoff := time.Now().UTC()
	eligibleAt, tenantID, err := s.closureCursor(passCtx, guard)
	if err != nil {
		return passError(ctx, passCtx, err)
	}
	work, scanned := 0, 0
	for scanned < s.closureScan() && work < s.closureWork() {
		if ended, err := passEnded(ctx, passCtx); ended {
			return err
		}
		limit := s.batch()
		if remaining := s.closureScan() - scanned; limit > remaining {
			limit = remaining
		}
		due, err := s.closurePage(passCtx, guard, passCutoff, eligibleAt, tenantID, limit)
		if err != nil {
			return passError(ctx, passCtx, err)
		}
		if len(due) == 0 {
			return passError(ctx, passCtx, s.advanceClosureCursor(passCtx, guard, closureCandidate{}, true))
		}
		for _, c := range due {
			if scanned >= s.closureScan() || work >= s.closureWork() {
				break
			}
			if ended, err := passEnded(ctx, passCtx); ended {
				return err
			}
			scanned++
			report.ClosuresChecked++
			child, got, err := guard.TryTenant(passCtx, c.scope)
			if err != nil {
				return passError(ctx, passCtx, err)
			}
			settle, budgetSpent, settleCancel := passCtx, false, func() {}
			if got {
				work++
				outcome, driveErr := s.driveClosure(passCtx, child, c, correlationID)
				if releaseErr := child.Release(); driveErr == nil && releaseErr != nil {
					driveErr = releaseErr
				}
				if driveErr != nil {
					if ctx.Err() != nil {
						// The caller stopped the pass, as on shutdown. That is not
						// this tenant's failure: nothing is recorded and the cursor
						// stays, so after the restart it is looked at first.
						return ctx.Err()
					}
					outcome = closureFailed
					if passCtx.Err() != nil {
						// This tenant used up the pass budget. It pays for it, with
						// a backoff, and the cursor moves past it, so a tenant that
						// is stuck cannot keep the others from being reached. The
						// pass ends, and what it records uses a short context of its
						// own, because the pass context is spent.
						settle, settleCancel = settleContext(ctx)
						budgetSpent = true
					}
					s.log().Warn("closure step failed", "tenant_id", c.scope.TenantID(), "error", driveErr.Error())
				}
				// The schedule is written with a short context of its own, so
				// that a pass context that ends right now cannot lose it.
				scheduleCtx, scheduleCancel := settleContext(ctx)
				s.recordClosureOutcome(scheduleCtx, guard, c, outcome, driveErr)
				scheduleCancel()
				switch outcome {
				case closureAdvanced, closureDone:
					report.ClosuresAdvanced++
				case closureWaiting:
					report.ClosuresWaiting++
				case closureManualReviewOutcome:
					report.ClosuresManualReview++
				case closureFailed:
					report.ClosuresFailed++
				}
			} else {
				report.ClosuresBusy++
			}
			if !budgetSpent && passCtx.Err() != nil && ctx.Err() == nil {
				// The step returned just as the pass budget ended. The tenant
				// was fully handled, so the cursor moves past it with a short
				// context of its own, and the pass ends cleanly.
				settle, settleCancel = settleContext(ctx)
				budgetSpent = true
			}
			if budgetSpent {
				err := s.advanceClosureCursor(settle, guard, c, false)
				settleCancel()
				return err
			}
			if err := s.advanceClosureCursor(passCtx, guard, c, false); err != nil {
				return passError(ctx, passCtx, err)
			}
			settleCancel()
			at := c.eligibleAt
			eligibleAt, tenantID = &at, c.scope.Tenant
		}
	}
	return nil
}

// driveClosure takes one tenant as far as its conditions allow, one state at a
// time: the steps before finance, the invoice, the move to blocking, and, once
// the account is blocked, completion. It stops at the first state that must
// wait, and a database error ends the capability, so it returns the error and
// a later pass resumes from what is durable.
func (s *Sweeper) driveClosure(ctx context.Context, child *LifecycleGuard, c closureCandidate, correlationID string) (closureOutcome, error) {
	advanced := false
	for step := 0; step < 4; step++ {
		var state ClosureState
		if err := child.QueryRow(ctx, `SELECT status FROM tenant_closure_settlements WHERE brand_id = $1 AND tenant_id = $2`, c.scope.Brand, c.scope.Tenant).Scan(&state); err != nil {
			return closureFailed, unavailable(err)
		}
		switch state {
		case ClosureCancelling:
			if s.Closure == nil {
				return waitOrAdvanced(advanced), nil
			}
			callCtx, cancelCall := context.WithTimeout(ctx, s.callBudget())
			prepared, err := s.Closure.Prepare(callCtx, child, c.scope)
			cancelCall()
			if err != nil {
				return closureFailed, err
			}
			if !prepared.Cancelled || !prepared.Drained || prepared.TargetDate.IsZero() {
				return waitOrAdvanced(advanced), nil
			}
			if err := recordClosurePrepared(ctx, child, c.scope, prepared, correlationID); err != nil {
				return closureFailed, err
			}
			if _, err := AdvanceClosureSettlement(ctx, child, c.scope, SystemActor, ClosureFinancePending, correlationID); err != nil {
				return closureFailed, err
			}
			advanced = true
		case ClosureFinancePending:
			run, err := runClosure(ctx, child, s.Engines, c.scope, correlationID)
			if err != nil {
				return closureFailed, err
			}
			if !run.Execution.Evidenced && !run.Execution.NoInvoice {
				if run.ManualReview {
					return closureManualReviewOutcome, nil
				}
				return waitOrAdvanced(advanced), nil
			}
			if _, err := AdvanceClosureSettlement(ctx, child, c.scope, SystemActor, ClosureBlocking, correlationID); err != nil {
				return closureFailed, err
			}
			advanced = true
		case ClosureBlocking:
			if s.Closure == nil {
				return waitOrAdvanced(advanced), nil
			}
			callCtx, cancelCall := context.WithTimeout(ctx, s.callBudget())
			blocked, err := s.Closure.Block(callCtx, child, c.scope)
			cancelCall()
			if err != nil {
				return closureFailed, err
			}
			if !blocked {
				return waitOrAdvanced(advanced), nil
			}
			if _, err := MarkClosureBlocked(ctx, child, c.scope, SystemActor, correlationID); err != nil {
				return closureFailed, err
			}
			if _, err := AdvanceClosureSettlement(ctx, child, c.scope, SystemActor, ClosureCompleted, correlationID); err != nil {
				if zserrors.IsCode(err, zserrors.CodeNotReady) {
					// The review guard refused: legacy usage waits for staff.
					return closureManualReviewOutcome, nil
				}
				return closureFailed, err
			}
			return closureDone, nil
		default:
			return closureDone, nil
		}
	}
	return waitOrAdvanced(advanced), nil
}

func waitOrAdvanced(advanced bool) closureOutcome {
	if advanced {
		return closureAdvanced
	}
	return closureWaiting
}

// recordClosurePrepared records what the steps before finance reported.
func recordClosurePrepared(ctx context.Context, db DB, sc scope.Scope, p ClosurePrepared, correlationID string) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE tenant_closure_settlements SET cancellation_completed_at = coalesce(cancellation_completed_at, now()),
    drain_completed_at = coalesce(drain_completed_at, now()), finance_target_date = coalesce(finance_target_date, $3::date), updated_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'cancelling'`, sc.Brand, sc.Tenant, p.TargetDate.UTC().Format(time.DateOnly)); err != nil {
			return unavailable(err)
		}
		_, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: SystemActor, Action: "tenant.closure_prepared", Outcome: audit.OutcomeSuccess, CorrelationID: correlationID,
			Details: map[string]any{"target_date": p.TargetDate.UTC().Format(time.DateOnly)}})
		return err
	})
}

// recordClosureOutcome sets when the tenant is due again. A tenant that moved
// is due at once; one that waits is looked at again after a pause; a failure
// backs off, and the settlement keeps the error name, never its text.
func (s *Sweeper) recordClosureOutcome(ctx context.Context, guard *LifecycleSweep, c closureCandidate, outcome closureOutcome, cause error) {
	delay, attempts, lastError := time.Duration(0), c.attempts, ""
	switch outcome {
	case closureWaiting, closureManualReviewOutcome:
		delay, attempts = closureWaitingRetry, 0
	case closureFailed:
		delay = closureRetryFirst
		for i := 0; i < c.attempts && delay < closureRetryMax; i++ {
			delay *= 2
		}
		delay = min(delay, closureRetryMax)
		attempts++
		lastError = "internal_error"
		var zsErr *zserrors.Error
		if errors.Is(cause, context.DeadlineExceeded) {
			lastError = "step_timeout"
		} else if errors.As(cause, &zsErr) {
			lastError = zserrors.Lookup(zsErr.Code).Name
		}
	default:
		attempts = 0
	}
	tx, err := guard.Begin(ctx)
	if err != nil {
		s.log().Warn("closure schedule not recorded", "tenant_id", c.scope.TenantID(), "error", err.Error())
		return
	}
	var last any
	if lastError != "" {
		last = lastError
	}
	if _, err := tx.Exec(ctx, `UPDATE tenant_closure_settlements SET next_attempt_at = now() + $3::interval, attempts = $4, last_error = $5, updated_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND status <> 'completed'`, c.scope.Brand, c.scope.Tenant, delay.String(), attempts, last); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		s.log().Warn("closure schedule not recorded", "tenant_id", c.scope.TenantID(), "error", err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.log().Warn("closure schedule not recorded", "tenant_id", c.scope.TenantID(), "error", err.Error())
	}
}

func (s *Sweeper) closureCursor(ctx context.Context, guard *LifecycleSweep) (*time.Time, idgen.UUID, error) {
	var at *time.Time
	var id *idgen.UUID
	err := guard.QueryRow(ctx, `SELECT last_eligible_at, last_tenant_id FROM sweep_cursor WHERE kind = 'closure'`).Scan(&at, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, idgen.Nil, nil
	}
	if err != nil {
		return nil, idgen.Nil, err
	}
	if id == nil {
		return nil, idgen.Nil, nil
	}
	return at, *id, nil
}

func (s *Sweeper) closurePage(ctx context.Context, guard *LifecycleSweep, cutoff time.Time, afterAt *time.Time, afterID idgen.UUID, limit int) ([]closureCandidate, error) {
	tx, err := guard.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '5s'`); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT s.brand_id, s.tenant_id, s.next_attempt_at, s.attempts
FROM tenant_closure_settlements s JOIN tenants t ON t.brand_id = s.brand_id AND t.id = s.tenant_id
WHERE t.status = 'closing' AND s.status <> 'completed' AND s.next_attempt_at <= $1
  AND ($2::timestamptz IS NULL OR (s.next_attempt_at, s.tenant_id) > ($2, $3))
ORDER BY s.next_attempt_at, s.tenant_id LIMIT $4`, cutoff, afterAt, afterID, limit)
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, unavailable(err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (closureCandidate, error) {
		var c closureCandidate
		err := r.Scan(&c.scope.Brand, &c.scope.Tenant, &c.eligibleAt, &c.attempts)
		return c, err
	})
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable(err)
	}
	return list, nil
}

func (s *Sweeper) advanceClosureCursor(ctx context.Context, guard *LifecycleSweep, c closureCandidate, wrap bool) error {
	tx, err := guard.Begin(ctx)
	if err != nil {
		return err
	}
	if wrap {
		_, err = tx.Exec(ctx, `INSERT INTO sweep_cursor (kind, last_eligible_at, last_tenant_id, updated_at)
VALUES ('closure', NULL, NULL, now()) ON CONFLICT (kind) DO UPDATE SET last_eligible_at = NULL, last_tenant_id = NULL, updated_at = now()`)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO sweep_cursor (kind, last_eligible_at, last_tenant_id, updated_at)
VALUES ('closure', $1, $2, now()) ON CONFLICT (kind) DO UPDATE SET last_eligible_at = EXCLUDED.last_eligible_at, last_tenant_id = EXCLUDED.last_tenant_id, updated_at = now()`, c.eligibleAt, c.scope.Tenant)
	}
	if err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}
