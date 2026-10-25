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
)

// ClosureFindingMax is how many findings of one kind ClosureFindings lists. A
// kind with more adds one warning that says how many are left out.
var ClosureFindingMax = 100

// ClosureFindings lists what an operator has to look at for tenant closures:
// closures that have not finished after an hour, closure review items nobody has
// resolved, usage sends whose outcome Kill Bill never confirmed, usage rollups
// that were rejected, and usage claims whose forwarder is gone. Each is a
// warning, not a failure, because it is work waiting for a person and not a
// stack that does not match its release. It reads and changes nothing, and a
// finding names a tenant, a state and a reason code, never usage or an address.
// now is the time the ages are taken against. A kind with more than
// ClosureFindingMax findings lists that many and then says how many more there
// are.
func ClosureFindings(ctx context.Context, db tenant.Querier, now time.Time) ([]Finding, error) {
	var findings []Finding
	// section runs the query, which has no LIMIT, for its first rows and counts
	// all of them with the same arguments.
	section := func(check, sql string, args []any, row func(pgx.Rows) (string, error)) error {
		rows, err := db.Query(ctx, sql+" LIMIT "+fmt.Sprint(ClosureFindingMax), args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			msg, err := row(rows)
			if err != nil {
				rows.Close()
				return err
			}
			findings = append(findings, Finding{"core", check, Warn, msg})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var total int
		if err := db.QueryRow(ctx, "SELECT count(*) FROM ("+sql+") q", args...).Scan(&total); err != nil {
			return err
		}
		if total > ClosureFindingMax {
			findings = append(findings, Finding{"core", check, Warn, fmt.Sprintf("and %d more: only the first %d are listed", total-ClosureFindingMax, ClosureFindingMax)})
		}
		return nil
	}

	if err := section("closure_stuck", `SELECT s.tenant_id, s.status, s.attempts, coalesce(s.last_error, ''), extract(epoch FROM $1::timestamptz - s.created_at)::bigint
FROM tenant_closure_settlements s JOIN tenants t ON t.brand_id = s.brand_id AND t.id = s.tenant_id
WHERE s.status <> 'completed' AND s.created_at < $1::timestamptz - make_interval(secs => $2)
ORDER BY s.created_at, s.tenant_id`, []any{now, closureStuckAfter.Seconds()}, func(r pgx.Rows) (string, error) {
		var id idgen.UUID
		var status, lastErr string
		var attempts int
		var age int64
		if err := r.Scan(&id, &status, &attempts, &lastErr, &age); err != nil {
			return "", err
		}
		msg := fmt.Sprintf("closure of %s has not finished after %s, at %s, %d attempts", idgen.Format(idgen.Tenant, id), humanAge(age), status, attempts)
		if lastErr != "" {
			msg += ", last error " + lastErr
		}
		return msg, nil
	}); err != nil {
		return nil, err
	}

	if err := section("closure_reviews", `SELECT i.tenant_id, count(*) FROM tenant_closure_review_items i
LEFT JOIN tenant_closure_review_resolutions r ON r.item_id = i.id WHERE r.item_id IS NULL GROUP BY i.tenant_id ORDER BY i.tenant_id`, nil, func(r pgx.Rows) (string, error) {
		var id idgen.UUID
		var n int
		if err := r.Scan(&id, &n); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d unresolved closure review items for %s; staff resolve them before the closure completes", n, idgen.Format(idgen.Tenant, id)), nil
	}); err != nil {
		return nil, err
	}

	if err := section("usage_unconfirmed", `SELECT tenant_id, closure_unresolved_reason, count(*) FROM usage_rollups u
WHERE status = 'pending' AND closure_unresolved_reason IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM usage_rollup_resolutions x WHERE x.rollup_id = u.id)
GROUP BY tenant_id, closure_unresolved_reason ORDER BY tenant_id, closure_unresolved_reason`, nil, func(r pgx.Rows) (string, error) {
		var id idgen.UUID
		var reason string
		var n int
		if err := r.Scan(&id, &reason, &n); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d usage rollups of %s are unconfirmed (%s): staff confirm them against Kill Bill, and the tenant is not drained until they do", n, idgen.Format(idgen.Tenant, id), reason), nil
	}); err != nil {
		return nil, err
	}

	if err := section("usage_rejected", `SELECT tenant_id, count(*) FROM usage_rollups
WHERE send_protocol = 1 AND status = 'rejected' GROUP BY tenant_id ORDER BY tenant_id`, nil, func(r pgx.Rows) (string, error) {
		var id idgen.UUID
		var n int
		if err := r.Scan(&id, &n); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d usage rollups of %s were rejected, which Core will not send to Kill Bill: staff reassign or discard them, and a closing tenant is not drained until they do", n, idgen.Format(idgen.Tenant, id)), nil
	}); err != nil {
		return nil, err
	}

	if err := section("usage_claims_stranded", `SELECT tenant_id, count(*) FROM usage_rollups
WHERE status = 'pending' AND claim_token IS NOT NULL AND claimed_until < $1::timestamptz - make_interval(secs => $2)
GROUP BY tenant_id ORDER BY tenant_id`, []any{now, claimStrandedAfter.Seconds()}, func(r pgx.Rows) (string, error) {
		var id idgen.UUID
		var n int
		if err := r.Scan(&id, &n); err != nil {
			return "", err
		}
		return fmt.Sprintf("%d usage rollups of %s are claimed by a forwarder that has gone: the forwarder marks them unconfirmed on its next pass, and then they are reported as usage_unconfirmed", n, idgen.Format(idgen.Tenant, id)), nil
	}); err != nil {
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
