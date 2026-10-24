package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// The thresholds of the closure findings.
const (
	closureStuckAfter  = time.Hour
	claimStrandedAfter = 15 * time.Minute
	closureFindingMax  = 100
)

// ClosureFindings lists what an operator has to look at for tenant closures:
// closures that have not finished after an hour, closure review items nobody has
// resolved, usage sends whose outcome Kill Bill never confirmed, usage rollups
// that were rejected, and usage claims whose forwarder is gone. Each is a warning, not a failure, because it
// is work waiting for a person and not a stack that does not match its release.
// It reads and changes nothing, and a finding names a tenant, a state and a
// reason code, never usage or an address. now is the time the ages are taken
// against.
func ClosureFindings(ctx context.Context, db tenant.Querier, now time.Time) ([]Finding, error) {
	var findings []Finding
	add := func(check, message string) {
		findings = append(findings, Finding{"core", check, Warn, message})
	}

	rows, err := db.Query(ctx, `SELECT s.tenant_id, s.status, s.attempts, coalesce(s.last_error, ''), extract(epoch FROM $1::timestamptz - s.created_at)::bigint
FROM tenant_closure_settlements s JOIN tenants t ON t.brand_id = s.brand_id AND t.id = s.tenant_id
WHERE s.status <> 'completed' AND s.created_at < $1::timestamptz - make_interval(secs => $2)
ORDER BY s.created_at LIMIT $3`, now, closureStuckAfter.Seconds(), closureFindingMax)
	if err != nil {
		return nil, err
	}
	type stuck struct {
		tenant   idgen.UUID
		status   string
		attempts int
		lastErr  string
		age      int64
	}
	stuckList, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (stuck, error) {
		var s stuck
		err := r.Scan(&s.tenant, &s.status, &s.attempts, &s.lastErr, &s.age)
		return s, err
	})
	if err != nil {
		return nil, err
	}
	for _, s := range stuckList {
		msg := fmt.Sprintf("closure of %s has not finished after %s, at %s, %d attempts", idgen.Format(idgen.Tenant, s.tenant), humanAge(s.age), s.status, s.attempts)
		if s.lastErr != "" {
			msg += ", last error " + s.lastErr
		}
		add("closure_stuck", msg)
	}

	rows, err = db.Query(ctx, `SELECT i.tenant_id, count(*) FROM tenant_closure_review_items i
LEFT JOIN tenant_closure_review_resolutions r ON r.item_id = i.id WHERE r.item_id IS NULL GROUP BY i.tenant_id ORDER BY i.tenant_id LIMIT $1`, closureFindingMax)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id idgen.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		add("closure_reviews", fmt.Sprintf("%d unresolved closure review items for %s; staff resolve them before the closure completes", n, idgen.Format(idgen.Tenant, id)))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(ctx, `SELECT tenant_id, closure_unresolved_reason, count(*) FROM usage_rollups
WHERE status = 'pending' AND closure_unresolved_reason IS NOT NULL GROUP BY tenant_id, closure_unresolved_reason ORDER BY tenant_id LIMIT $1`, closureFindingMax)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id idgen.UUID
		var reason string
		var n int
		if err := rows.Scan(&id, &reason, &n); err != nil {
			rows.Close()
			return nil, err
		}
		add("usage_unconfirmed", fmt.Sprintf("%d usage rollups of %s are unconfirmed (%s): staff resolve them against Kill Bill, and the tenant is not drained until they do", n, idgen.Format(idgen.Tenant, id), reason))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(ctx, `SELECT tenant_id, count(*) FROM usage_rollups
WHERE send_protocol = 1 AND status = 'rejected' GROUP BY tenant_id ORDER BY tenant_id LIMIT $1`, closureFindingMax)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id idgen.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		add("usage_rejected", fmt.Sprintf("%d usage rollups of %s were rejected, so Kill Bill will not bill them: staff reassign or discard them, and a closing tenant is not drained until they do", n, idgen.Format(idgen.Tenant, id)))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query(ctx, `SELECT tenant_id, count(*) FROM usage_rollups
WHERE status = 'pending' AND claim_token IS NOT NULL AND claimed_until < $1::timestamptz - make_interval(secs => $2)
GROUP BY tenant_id ORDER BY tenant_id LIMIT $3`, now, claimStrandedAfter.Seconds(), closureFindingMax)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id idgen.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		add("usage_claims_stranded", fmt.Sprintf("%d usage rollups of %s are claimed by a forwarder that has gone: the usage forwarder resolves them on its next pass", n, idgen.Format(idgen.Tenant, id)))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(findings) == 0 {
		findings = append(findings, Finding{"core", "closures", OK, "no closure is stuck, no closure review item is open and no usage send is unconfirmed, rejected or stranded"})
	}
	return findings, nil
}

// humanAge renders seconds as hours when there are any, else minutes.
func humanAge(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
