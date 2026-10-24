package metering

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Rollup is a set of usage events sent, or to be sent, to Kill Bill as one
// usage record.
type Rollup struct {
	ID                    string     `json:"id"`
	KillBillUnit          string     `json:"killbill_unit"`
	RequestedSubscription string     `json:"requested_subscription,omitempty"`
	Subscription          string     `json:"subscription,omitempty"`
	Day                   string     `json:"day"`
	PeriodStart           time.Time  `json:"period_start"`
	RecordAt              time.Time  `json:"record_at"`
	QuantityMicro         int64      `json:"quantity_micro"`
	Events                int        `json:"events"`
	TrackingID            string     `json:"killbill_tracking_id"`
	Status                string     `json:"status"`
	RejectReason          string     `json:"reject_reason,omitempty"`
	Attempts              int        `json:"attempts"`
	LastError             string     `json:"last_error,omitempty"`
	ForwardedAt           *time.Time `json:"forwarded_at"`
	RejectedAt            *time.Time `json:"rejected_at"`
	DiscardedAt           *time.Time `json:"discarded_at"`

	id          idgen.UUID
	periodStart time.Time
}

const rollupColumns = `id, killbill_unit, coalesce(requested_subscription_id::text, ''), coalesce(subscription_id::text, ''), day::text,
       period_start, record_at, quantity_micro, events, tracking_id, status, coalesce(reject_reason, ''), attempts, coalesce(last_error, ''),
       forwarded_at, rejected_at, discarded_at`

func scanRollup(row pgx.Row) (Rollup, error) {
	var r Rollup
	err := row.Scan(&r.id, &r.KillBillUnit, &r.RequestedSubscription, &r.Subscription, &r.Day, &r.PeriodStart, &r.RecordAt, &r.QuantityMicro,
		&r.Events, &r.TrackingID, &r.Status, &r.RejectReason, &r.Attempts, &r.LastError, &r.ForwardedAt, &r.RejectedAt, &r.DiscardedAt)
	if err != nil {
		return Rollup{}, err
	}
	r.ID = idgen.Format(idgen.Rollup, r.id)
	r.PeriodStart, r.RecordAt = r.PeriodStart.UTC(), r.RecordAt.UTC()
	r.periodStart = r.PeriodStart
	for _, t := range []**time.Time{&r.ForwardedAt, &r.RejectedAt, &r.DiscardedAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return r, nil
}

var errRollupNotFound = zserrors.New(zserrors.CodeNotFound, "")

// getRollup reads a rollup of the tenant by its public ID, locked for the
// rest of tx.
func getRollup(ctx context.Context, tx pgx.Tx, sc scope.Scope, publicID string) (Rollup, error) {
	id, err := idgen.ParseAs(idgen.Rollup, publicID)
	if err != nil {
		return Rollup{}, errRollupNotFound
	}
	args, err := sc.Bind(id)
	if err != nil {
		return Rollup{}, err
	}
	r, err := scanRollup(tx.QueryRow(ctx, `SELECT `+rollupColumns+` FROM usage_rollups WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 FOR UPDATE`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Rollup{}, errRollupNotFound
	}
	if err != nil {
		return Rollup{}, unavailable(err)
	}
	return r, nil
}

// StaffCall is the staff member who resolves a rejected rollup, why, and
// the correlation ID of the request.
type StaffCall struct {
	Call
	Reason string
}

func (c StaffCall) check() (string, error) {
	if err := c.Call.validate(); err != nil {
		return "", err
	}
	reason := strings.TrimSpace(c.Reason)
	if reason == "" {
		return "", zserrors.New(zserrors.CodeReasonRequired, "")
	}
	if utf8.RuneCountInString(reason) > audit.MaxReasonLength {
		return "", validation("The reason is too long.")
	}
	return reason, nil
}

var errNotRejected = zserrors.New(zserrors.CodeConflict, "Only a rejected rollup can be reassigned or discarded.")

// ReassignRollup sends a rejected rollup to another subscription of the
// tenant. The subscription must be active, price the rollup's unit and
// have started by the start of the rollup's period. The rollup is pending
// again and the forwarder sends it on its next pass. It is audited as
// metering.rollup_reassigned with the reason.
func (s *Service) ReassignRollup(ctx context.Context, sc scope.Scope, call StaffCall, rollupID, subscription string) (Rollup, error) {
	reason, err := call.check()
	if err != nil {
		return Rollup{}, err
	}
	if s.DB == nil || !s.Billing.Ready() {
		return Rollup{}, zserrors.New(zserrors.CodeNotReady, "Reassigning usage needs the Kill Bill admin settings.")
	}
	if !killBillIDPattern.MatchString(subscription) {
		return Rollup{}, validation("The subscription must be a Kill Bill subscription ID.")
	}
	var current Rollup
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		current, err = getRollup(ctx, tx, sc, rollupID)
		return err
	}); err != nil {
		return Rollup{}, err
	}
	if current.Status != RollupRejected {
		return Rollup{}, errNotRejected
	}
	acct, err := s.Billing.TenantAccount(ctx, s.DB, sc)
	if err != nil {
		return Rollup{}, err
	}
	if _, err := acct.UsageSubscriptionAt(ctx, subscription, current.KillBillUnit, current.periodStart); err != nil {
		return Rollup{}, err
	}
	var out Rollup
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		r, err := getRollup(ctx, tx, sc, rollupID)
		if err != nil {
			return err
		}
		if r.Status != RollupRejected {
			return errNotRejected
		}
		args, _ := sc.Bind(r.id, subscription, s.now())
		out, err = scanRollup(tx.QueryRow(ctx, `UPDATE usage_rollups SET subscription_id = $4::uuid, status = 'pending', reject_reason = NULL,
    rejected_at = NULL, attempts = 0, next_attempt_at = $5, last_error = NULL, target_endpoint_fingerprint = NULL,
    target_killbill_tenant_id = NULL, target_killbill_account_id = NULL
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'rejected' RETURNING `+rollupColumns, args...))
		if err != nil {
			return unavailable(err)
		}
		_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "metering.rollup_reassigned", Reason: reason,
			Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: map[string]any{"rollup_id": r.ID,
				"from_subscription_id": r.Subscription, "killbill_subscription_id": subscription, "reject_reason": r.RejectReason,
				"quantity_micro": r.QuantityMicro, "events": r.Events, "killbill_unit": r.KillBillUnit}})
		return err
	})
	return out, err
}

// DiscardRollup gives up a rejected rollup: it is never sent, and its usage
// is not billed. It is audited as metering.rollup_discarded with the
// reason, the quantity and the number of events.
func (s *Service) DiscardRollup(ctx context.Context, sc scope.Scope, call StaffCall, rollupID string) (Rollup, error) {
	reason, err := call.check()
	if err != nil {
		return Rollup{}, err
	}
	if s.DB == nil {
		return Rollup{}, zserrors.New(zserrors.CodeNotReady, "")
	}
	var out Rollup
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		r, err := getRollup(ctx, tx, sc, rollupID)
		if err != nil {
			return err
		}
		if r.Status != RollupRejected {
			return errNotRejected
		}
		args, _ := sc.Bind(r.id, s.now())
		out, err = scanRollup(tx.QueryRow(ctx, `UPDATE usage_rollups SET status = 'discarded', discarded_at = $4
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+rollupColumns, args...))
		if err != nil {
			return unavailable(err)
		}
		_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "metering.rollup_discarded", Reason: reason,
			Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: map[string]any{"rollup_id": r.ID,
				"reject_reason": r.RejectReason, "quantity_micro": r.QuantityMicro, "events": r.Events, "killbill_unit": r.KillBillUnit,
				"killbill_subscription_id": r.Subscription}})
		return err
	})
	return out, err
}

// RejectedRollups returns the tenant's rejected rollups, oldest first.
func RejectedRollups(ctx context.Context, q Querier, sc scope.Scope) ([]Rollup, error) {
	args, err := sc.Bind()
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+rollupColumns+` FROM usage_rollups WHERE brand_id = $1 AND tenant_id = $2 AND status = 'rejected'
ORDER BY rejected_at, id`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Rollup, error) { return scanRollup(r) })
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}
