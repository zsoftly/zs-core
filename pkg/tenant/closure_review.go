package tenant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Closure review kinds, and the reason staff see for each. A closing tenant
// that holds usage accepted before dispatch protocol 1 needs a manual review
// of it before closure can complete: that usage cannot be rolled up, sent or
// marked billed by the forwarder, and nothing here does it either.
const (
	ClosureReviewLegacyUsageEvents  = "legacy_usage_events"
	ClosureReviewLegacyUsageRollups = "legacy_usage_rollups"

	closureReasonLegacyUsageUnbilled = "legacy_usage_unbilled"
	closureReasonLegacyRollupUnsent  = "legacy_rollup_unsent"

	closureReviewNoteMax = 500
)

// ClosureReview is a manual review item that closure requires.
type ClosureReview struct {
	ID         idgen.UUID
	Kind       string
	Reason     string
	Count      int64
	RecordedAt time.Time
	Resolved   bool
	ResolvedBy string
	Note       string
}

// ClosureReviewResult describes one RecordClosureReviews pass.
type ClosureReviewResult struct {
	Recorded int
}

// RecordClosureReviews records a required manual review item for each kind of
// legacy usage a closing tenant holds that was never forwarded: raw events in
// no rollup, and rollups still pending or rejected. It is idempotent, audits each item it
// records as tenant.closure_review_required, and never changes the usage.
func RecordClosureReviews(ctx context.Context, db DB, sc scope.Scope, actor events.Actor, correlationID string) (ClosureReviewResult, error) {
	if _, ok := db.(pgx.Tx); ok {
		return ClosureReviewResult{}, zserrors.New(zserrors.CodeNotReady, "Closure review is not ready.")
	}
	if _, err := sc.Bind(); err != nil {
		return ClosureReviewResult{}, err
	}
	var result ClosureReviewResult
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT t.status FROM tenant_closure_settlements s
JOIN tenants t ON t.brand_id = s.brand_id AND t.id = s.tenant_id
WHERE s.brand_id = $1 AND s.tenant_id = $2 FOR UPDATE OF s`, sc.Brand, sc.Tenant).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "closing") {
			return zserrors.New(zserrors.CodeNotReady, "Closure review is not ready.")
		}
		if err != nil {
			return unavailable(err)
		}
		for _, k := range []struct{ kind, reason, count string }{
			{ClosureReviewLegacyUsageEvents, closureReasonLegacyUsageUnbilled, `SELECT count(*), coalesce(sum(quantity_micro), 0) FROM usage_events
WHERE brand_id = $1 AND tenant_id = $2 AND acceptance_protocol = 0 AND reservation_id IS NULL AND rollup_id IS NULL`},
			{ClosureReviewLegacyUsageRollups, closureReasonLegacyRollupUnsent, `SELECT count(*), coalesce(sum(quantity_micro), 0) FROM usage_rollups
WHERE brand_id = $1 AND tenant_id = $2 AND send_protocol = 0 AND status IN ('pending', 'rejected')`},
		} {
			var count, quantity int64
			if err := tx.QueryRow(ctx, k.count, sc.Brand, sc.Tenant).Scan(&count, &quantity); err != nil {
				return unavailable(err)
			}
			if count == 0 {
				continue
			}
			itemID := idgen.MustNewV7()
			// An item already covers the usage held now when one exists for at
			// least this count. Usage that grew since needs a new item.
			tag, err := tx.Exec(ctx, `INSERT INTO tenant_closure_review_items (brand_id, tenant_id, id, kind, reason, item_count, evidence, correlation_id)
SELECT $1, $2, $3, $4, $5, $6::bigint, jsonb_build_object('count', $6::bigint, 'quantity_micro', $7::bigint), $8
WHERE NOT EXISTS (SELECT 1 FROM tenant_closure_review_items WHERE brand_id = $1 AND tenant_id = $2 AND kind = $4 AND item_count >= $6::bigint)
ON CONFLICT (brand_id, tenant_id, kind, item_count) DO NOTHING`, sc.Brand, sc.Tenant, itemID, k.kind, k.reason, count, quantity, correlationID)
			if err != nil {
				return unavailable(err)
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: actor, Action: "tenant.closure_review_required", Outcome: audit.OutcomeSuccess,
				CorrelationID: correlationID, Details: map[string]any{"review_id": itemID.String(), "kind": k.kind, "reason": k.reason,
					"count": count, "quantity_micro": quantity}}); err != nil {
				return err
			}
			result.Recorded++
		}
		return nil
	})
	if err != nil {
		return ClosureReviewResult{}, err
	}
	return result, nil
}

// ResolveClosureReview records that staff reviewed an item, with a note. It
// changes no usage: nothing is discarded, sent or marked billed. It is
// audited as tenant.closure_review_resolved and is idempotent.
func ResolveClosureReview(ctx context.Context, db DB, sc scope.Scope, id idgen.UUID, actor events.Actor, note, correlationID string) error {
	if _, ok := db.(pgx.Tx); ok {
		return zserrors.New(zserrors.CodeNotReady, "Closure review is not ready.")
	}
	if _, err := sc.Bind(); err != nil {
		return err
	}
	note = strings.TrimSpace(note)
	if note == "" || len(note) > closureReviewNoteMax {
		return validation("A review needs a note of 1 to 500 characters.")
	}
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var kind, reason string
		err := tx.QueryRow(ctx, `SELECT kind, reason FROM tenant_closure_review_items WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 FOR UPDATE`,
			sc.Brand, sc.Tenant, id).Scan(&kind, &reason)
		if errors.Is(err, pgx.ErrNoRows) {
			return zserrors.New(zserrors.CodeNotFound, "")
		}
		if err != nil {
			return unavailable(err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO tenant_closure_review_resolutions (item_id, resolved_by, note) VALUES ($1, $2, $3)
ON CONFLICT (item_id) DO NOTHING`, id, actor.ID, note)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: actor, Action: "tenant.closure_review_resolved", Reason: note, Outcome: audit.OutcomeSuccess, CorrelationID: correlationID,
			Details: map[string]any{"review_id": id.String(), "kind": kind, "reason": reason}})
		return err
	})
}

// ListClosureReviews returns the tenant's closure review items, oldest first,
// with who resolved each and the note.
func ListClosureReviews(ctx context.Context, q Querier, sc scope.Scope) ([]ClosureReview, error) {
	args, err := sc.Bind()
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT i.id, i.kind, i.reason, i.item_count, i.recorded_at,
    r.item_id IS NOT NULL, coalesce(r.resolved_by, ''), coalesce(r.note, '')
FROM tenant_closure_review_items i
LEFT JOIN tenant_closure_review_resolutions r ON r.item_id = i.id
WHERE i.brand_id = $1 AND i.tenant_id = $2 ORDER BY i.recorded_at, i.kind`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ClosureReview, error) {
		var it ClosureReview
		err := r.Scan(&it.ID, &it.Kind, &it.Reason, &it.Count, &it.RecordedAt, &it.Resolved, &it.ResolvedBy, &it.Note)
		return it, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}
