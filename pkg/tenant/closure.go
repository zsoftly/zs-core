package tenant

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// ClosureProcessResult describes one closure processor pass.
type ClosureProcessResult struct {
	Claimed   bool
	Attempted bool
}

// ProcessClosureInvoiceOnce commits at most one immutable D1a closure intent.
// It does not resolve secrets, call engines, or change closure completion.
func ProcessClosureInvoiceOnce(ctx context.Context, db DB, _ Engines, sc scope.Scope) (ClosureProcessResult, error) {
	if _, ok := db.(pgx.Tx); ok {
		return ClosureProcessResult{}, zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
	}
	if _, err := sc.Bind(); err != nil {
		return ClosureProcessResult{}, err
	}
	rec, err := brand.Get(ctx, db, sc.Brand)
	if err != nil {
		return ClosureProcessResult{}, err
	}
	kbTenant, err := idgen.ParseUUID(rec.KillBillTenantID)
	if err != nil {
		return ClosureProcessResult{}, zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
	}
	attemptID := idgen.MustNewV7()
	claimed := false
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var (
			status, settlement, accountID string
			targetDate                    time.Time
			cancelled, drained            *time.Time
			requested                     *time.Time
			legacyAttempt, legacyInvoice  *idgen.UUID
		)
		err := tx.QueryRow(ctx, `SELECT t.status, s.status, t.killbill_account_id::text, s.finance_target_date,
    s.cancellation_completed_at, s.drain_completed_at, s.finance_requested_at, s.finance_attempt_id, s.finance_invoice_id
FROM tenants t JOIN tenant_closure_settlements s ON s.brand_id = t.brand_id AND s.tenant_id = t.id
WHERE t.brand_id = $1 AND t.id = $2 FOR UPDATE`, sc.Brand, sc.Tenant).Scan(&status, &settlement, &accountID, &targetDate,
			&cancelled, &drained, &requested, &legacyAttempt, &legacyInvoice)
		if errors.Is(err, pgx.ErrNoRows) {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
		}
		if err != nil {
			return unavailable(err)
		}
		if status != "closing" || settlement != "finance_pending" || cancelled == nil || drained == nil || requested != nil || legacyAttempt != nil || legacyInvoice != nil {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
		}
		kbAccount, err := idgen.ParseUUID(accountID)
		if err != nil {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts
    (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (brand_id, tenant_id) DO NOTHING`, attemptID, sc.Brand, sc.Tenant, kbTenant, kbAccount, targetDate)
		if err != nil {
			return unavailable(err)
		}
		claimed = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return ClosureProcessResult{}, err
	}
	return ClosureProcessResult{Claimed: claimed}, nil
}
