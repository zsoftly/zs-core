package tenant

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// ClosureStart describes one StartClosure call.
type ClosureStart struct {
	// Tenant is the tenant as it is after the call.
	Tenant Tenant
	// CutoffAt is the cutoff of the closure: usage up to it is invoiced.
	CutoffAt time.Time
	// Started is false when the closure had already started.
	Started bool
}

// StartClosure starts the closure of a tenant that is active or suspended: it
// moves the tenant to closing and creates its closure settlement at
// cancelling, with the cutoff at now, in one transaction under the lock of the
// tenant row, audited as tenant.closure_started with the reason. The sweeper
// takes it from there. A closure that has started is left as it is and Started
// is false, so a retry or a second caller is harmless. A closed tenant is
// refused as tenant_closed. The start takes no step against Kill Bill or
// Keycloak: the cancellation, the invoice and the account block are the steps
// of the settlement.
func StartClosure(ctx context.Context, db DB, sc scope.Scope, actor events.Actor, reason, correlationID string) (_ ClosureStart, err error) {
	if _, ok := db.(pgx.Tx); ok {
		return ClosureStart{}, zserrors.New(zserrors.CodeNotReady, "Closure start is not ready.")
	}
	if _, err := sc.Bind(); err != nil {
		return ClosureStart{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 1000 {
		return ClosureStart{}, zserrors.New(zserrors.CodeReasonRequired, "")
	}
	// The start holds the lifecycle capability of the tenant, as Suspend,
	// Reactivate and Close do from their read to their last write, so it is
	// refused as busy while one of them runs and one is refused while it runs.
	db, done, err := underTenantCapability(ctx, db, sc)
	if err != nil {
		return ClosureStart{}, err
	}
	defer done(&err)
	var result ClosureStart
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		locked, err := lockTenant(ctx, tx, sc)
		if err != nil {
			return err
		}
		switch locked.Status {
		case StatusClosed:
			return errTenantClosed()
		case StatusClosing:
			var cutoff time.Time
			err := tx.QueryRow(ctx, `SELECT cutoff_at FROM tenant_closure_settlements WHERE brand_id = $1 AND tenant_id = $2`, sc.Brand, sc.Tenant).Scan(&cutoff)
			if errors.Is(err, pgx.ErrNoRows) {
				return zserrors.New(zserrors.CodeNotReady, "The tenant is closing without a settlement.")
			}
			if err != nil {
				return unavailable(err)
			}
			result = ClosureStart{Tenant: locked, CutoffAt: cutoff}
			return nil
		}
		// A settlement that exists for a tenant that is not closing is
		// answered before the insert: a failed statement would end the
		// capability, and this is an answer, not a fault.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_closure_settlements WHERE brand_id = $1 AND tenant_id = $2)`, sc.Brand, sc.Tenant).Scan(&exists); err != nil {
			return unavailable(err)
		}
		if exists {
			return zserrors.New(zserrors.CodeConflict, "A closure settlement already exists for this tenant.")
		}
		var cutoff time.Time
		if err := tx.QueryRow(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at) VALUES ($1, $2, now()) RETURNING cutoff_at`,
			sc.Brand, sc.Tenant).Scan(&cutoff); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return zserrors.New(zserrors.CodeConflict, "A closure settlement already exists for this tenant.")
			}
			return unavailable(err)
		}
		args, err := sc.Bind()
		if err != nil {
			return err
		}
		updated, err := scanTenant(tx.QueryRow(ctx, `UPDATE tenants SET status = 'closing', updated_at = now() WHERE brand_id = $1 AND id = $2 RETURNING `+tenantColumns, args...))
		if err != nil {
			return unavailable(err)
		}
		if _, err := audit.Write(ctx, tx, sc, audit.Entry{
			Actor: actor, Action: "tenant.closure_started", Reason: reason, Outcome: audit.OutcomeSuccess, CorrelationID: correlationID,
			Details: map[string]any{"status": map[string]any{"before": string(locked.Status), "after": string(StatusClosing)}, "cutoff_at": cutoff.UTC().Format(time.RFC3339)},
		}); err != nil {
			return err
		}
		result = ClosureStart{Tenant: updated, CutoffAt: cutoff, Started: true}
		return nil
	})
	if err != nil {
		return ClosureStart{}, err
	}
	return result, nil
}
