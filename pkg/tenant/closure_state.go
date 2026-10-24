package tenant

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// ClosureState is the state of a closure settlement.
type ClosureState string

// The states of a closure settlement, in the order it moves through them.
const (
	ClosureCancelling     ClosureState = "cancelling"
	ClosureFinancePending ClosureState = "finance_pending"
	ClosureBlocking       ClosureState = "blocking"
	ClosureCompleted      ClosureState = "completed"
)

var closureRank = map[ClosureState]int{ClosureCancelling: 0, ClosureFinancePending: 1, ClosureBlocking: 2, ClosureCompleted: 3}

// ClosureAdvance describes one AdvanceClosureSettlement call.
type ClosureAdvance struct {
	From, To ClosureState
	Changed  bool
}

// AdvanceClosureSettlement moves a closing tenant's settlement to the next
// state, which must be the one right after the current state. It is audited as
// tenant.closure_state_changed. A settlement already at the state, or past it,
// is left as it is and Changed is false, so a retry is harmless. A move that
// would skip a state, or whose conditions are not met, is refused as not ready.
// The conditions are checked again by a database trigger of migration 27, which
// also refuses to skip a state or go back:
//   - finance_pending needs the cancellation and the usage drain done and a
//     finance target date;
//   - blocking needs a settled invoice, evidenced or nothing to invoice, so an
//     unknown or unfinished invoice outcome waits for a person;
//   - completed needs the account blocked, and goes through the review guard
//     of migration 26.
func AdvanceClosureSettlement(ctx context.Context, db DB, sc scope.Scope, actor events.Actor, to ClosureState, correlationID string) (ClosureAdvance, error) {
	if _, ok := db.(pgx.Tx); ok {
		return ClosureAdvance{}, zserrors.New(zserrors.CodeNotReady, "Closure settlement is not ready.")
	}
	if _, err := sc.Bind(); err != nil {
		return ClosureAdvance{}, err
	}
	if _, ok := closureRank[to]; !ok || to == ClosureCancelling {
		return ClosureAdvance{}, validation("A closure settlement moves to finance_pending, blocking or completed.")
	}
	var result ClosureAdvance
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		from, err := lockClosureSettlement(ctx, tx, sc)
		if err != nil {
			return err
		}
		result = ClosureAdvance{From: from, To: to}
		if closureRank[from] >= closureRank[to] {
			return nil
		}
		if closureRank[to]-closureRank[from] != 1 {
			return zserrors.New(zserrors.CodeNotReady, "A closure settlement cannot skip a state.")
		}
		var update string
		switch to {
		case ClosureFinancePending:
			update = `UPDATE tenant_closure_settlements SET status = 'finance_pending',
    cancellation_completed_at = coalesce(cancellation_completed_at, now()), updated_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'cancelling'`
		case ClosureBlocking:
			update = `UPDATE tenant_closure_settlements SET status = 'blocking',
    finance_completed_at = coalesce(finance_completed_at, now()),
    finance_invoice_id = (SELECT o.invoice_id FROM tenant_closure_invoice_outcomes o
        JOIN tenant_closure_invoice_attempts a ON a.id = o.attempt_id
        WHERE a.brand_id = $1 AND a.tenant_id = $2 AND o.kind = 'invoice_id' LIMIT 1),
    updated_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'finance_pending'`
		case ClosureCompleted:
			update = `UPDATE tenant_closure_settlements SET status = 'completed', completed_at = coalesce(completed_at, now()), updated_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'blocking'`
		}
		if _, err := tx.Exec(ctx, update, sc.Brand, sc.Tenant); err != nil {
			return closureStateError(err)
		}
		if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: actor, Action: "tenant.closure_state_changed", Outcome: audit.OutcomeSuccess,
			CorrelationID: correlationID, Details: map[string]any{"from": string(from), "to": string(to)}}); err != nil {
			return err
		}
		result.Changed = true
		return nil
	})
	if err != nil {
		return ClosureAdvance{}, err
	}
	return result, nil
}

// MarkClosureBlocked records that the closing tenant's account is blocked,
// which completion needs. The settlement stays in the blocking state. It
// reports false when it was already recorded, and is audited as
// tenant.closure_blocked when it is new.
func MarkClosureBlocked(ctx context.Context, db DB, sc scope.Scope, actor events.Actor, correlationID string) (bool, error) {
	if _, ok := db.(pgx.Tx); ok {
		return false, zserrors.New(zserrors.CodeNotReady, "Closure settlement is not ready.")
	}
	if _, err := sc.Bind(); err != nil {
		return false, err
	}
	changed := false
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		state, err := lockClosureSettlement(ctx, tx, sc)
		if err != nil {
			return err
		}
		if state != ClosureBlocking && state != ClosureCompleted {
			return zserrors.New(zserrors.CodeNotReady, "The account can be marked blocked once the settlement is in the blocking state.")
		}
		tag, err := tx.Exec(ctx, `UPDATE tenant_closure_settlements SET blocked_at = now(), updated_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND blocked_at IS NULL`, sc.Brand, sc.Tenant)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		changed = true
		_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: actor, Action: "tenant.closure_blocked", Outcome: audit.OutcomeSuccess, CorrelationID: correlationID})
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// lockClosureSettlement reads the settlement state under its row lock, for a
// tenant that is closing.
func lockClosureSettlement(ctx context.Context, tx pgx.Tx, sc scope.Scope) (ClosureState, error) {
	var state ClosureState
	var tenantStatus string
	err := tx.QueryRow(ctx, `SELECT s.status, t.status FROM tenant_closure_settlements s
JOIN tenants t ON t.brand_id = s.brand_id AND t.id = s.tenant_id
WHERE s.brand_id = $1 AND s.tenant_id = $2 FOR UPDATE OF s`, sc.Brand, sc.Tenant).Scan(&state, &tenantStatus)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && tenantStatus != "closing") {
		return "", zserrors.New(zserrors.CodeNotReady, "Closure settlement is not ready.")
	}
	if err != nil {
		return "", unavailable(err)
	}
	return state, nil
}

// closureStateError maps a refusal of the database trigger to not ready.
func closureStateError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return zserrors.Wrap(zserrors.CodeNotReady, "The closure settlement cannot move to that state yet.", err)
	}
	return unavailable(err)
}
