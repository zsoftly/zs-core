package metering

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// RollupResolution is what staff recorded about an unconfirmed rollup.
type RollupResolution struct {
	RollupID   string
	Decision   string
	InvoiceID  string
	ResolvedBy string
	ResolvedAt time.Time
	// UnbilledQuantityMicro is the quantity of the rollup that was lost, for
	// a decision of not_billed.
	UnbilledQuantityMicro int64
}

// The decisions staff can record about an unconfirmed rollup. forwarded says
// Kill Bill holds it: its tracking ID is on a committed invoice of the tenant's
// account. not_billed says it does not: the cancellation is done, the account has
// committed invoices after it, and none carries the tracking ID.
const (
	ResolveDecisionForwarded = "forwarded"
	ResolveDecisionNotBilled = "not_billed"
)

var (
	errNotUnconfirmed = zserrors.New(zserrors.CodeConflict, "Only a rollup whose send Kill Bill never confirmed can be resolved.")
	errNoEvidence     = zserrors.New(zserrors.CodeConflict, "No committed invoice of the tenant's account carries the tracking ID of this rollup.")
	errNotCancelled   = zserrors.New(zserrors.CodeConflict, "The usage can be recorded as not billed once the cancellation of the closure is done.")
	errNoLaterInvoice = zserrors.New(zserrors.CodeConflict, "The account has no committed invoice after the cancellation yet, so it cannot be said that Kill Bill does not bill the record.")
	errCarried        = zserrors.New(zserrors.CodeConflict, "A committed invoice carries the tracking ID of this rollup: confirm it as forwarded.")
	// confirmableReasons are the reasons of a send that started: the usage may
	// be in Kill Bill. A claim that never started sent nothing.
	confirmableReasons = []string{"usage_send_unconfirmed", "usage_duplicate_unconfirmed", "usage_transport_unconfirmed"}
)

type rollupState struct {
	status    string
	protocol  int
	reason    string
	started   bool
	tracking  string
	resolved  *RollupResolution
	rollupUID idgen.UUID
	quantity  int64
	// cancelDay is the UTC day the cancellation of the closure was done, or
	// empty when it was not.
	cancelDay string
}

func readRollupState(ctx context.Context, tx pgx.Tx, sc scope.Scope, rollupID string) (rollupState, error) {
	r, err := getRollup(ctx, tx, sc, rollupID)
	if err != nil {
		return rollupState{}, err
	}
	st := rollupState{status: r.Status, tracking: r.TrackingID, rollupUID: r.id, quantity: r.QuantityMicro}
	args, _ := sc.Bind(r.id)
	if err := tx.QueryRow(ctx, `SELECT send_protocol, coalesce(closure_unresolved_reason, ''), send_started_at IS NOT NULL FROM usage_rollups WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, args...).
		Scan(&st.protocol, &st.reason, &st.started); err != nil {
		return rollupState{}, unavailable(err)
	}
	if err := tx.QueryRow(ctx, `SELECT coalesce((cancellation_completed_at AT TIME ZONE 'UTC')::date::text, '') FROM tenant_closure_settlements WHERE brand_id = $1 AND tenant_id = $2`, sc.Brand, sc.Tenant).
		Scan(&st.cancelDay); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return rollupState{}, unavailable(err)
	}
	var res RollupResolution
	var id idgen.UUID
	err = tx.QueryRow(ctx, `SELECT rollup_id, decision, coalesce(invoice_id::text, ''), coalesce(unbilled_quantity_micro, 0), resolved_by, resolved_at FROM usage_rollup_resolutions WHERE rollup_id = $1`, r.id).
		Scan(&id, &res.Decision, &res.InvoiceID, &res.UnbilledQuantityMicro, &res.ResolvedBy, &res.ResolvedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return rollupState{}, unavailable(err)
	default:
		res.RollupID, res.ResolvedAt = idgen.Format(idgen.Rollup, id), res.ResolvedAt.UTC()
		st.resolved = &res
	}
	return st, nil
}

func (st rollupState) confirmable() bool {
	return st.protocol == 1 && st.status == RollupPending && st.started && slices.Contains(confirmableReasons, st.reason)
}

// ResolveUnconfirmedRollup records what became of a rollup whose send started and
// whose outcome Kill Bill never confirmed, so that the closure of its tenant can
// complete. The evidence is Kill Bill's, read here:
//   - forwarded: the tracking ID of the rollup is on a committed invoice of the
//     tenant's account. An invoice that is not committed is no evidence. This
//     shows that Kill Bill billed the record, not its quantity.
//   - not_billed: the cancellation of the closure is done, the account has
//     committed invoices dated on or after the day of it, and none of the
//     committed invoices carries the tracking ID. The resolution records the
//     quantity of the frozen rollup that was lost, for finance.
//
// Without the evidence the answer is a conflict. The decision is kept beside the
// rollup and does not change it, is audited as metering.rollup_resolved with the
// staff reason, and is idempotent: the second call returns the first with
// replayed true. Nothing here sends usage again.
func (s *Service) ResolveUnconfirmedRollup(ctx context.Context, sc scope.Scope, call StaffCall, rollupID, decision string) (RollupResolution, bool, error) {
	reason, err := call.check()
	if err != nil {
		return RollupResolution{}, false, err
	}
	if decision != ResolveDecisionForwarded && decision != ResolveDecisionNotBilled {
		return RollupResolution{}, false, validation("The decision must be forwarded or not_billed.")
	}
	if s.DB == nil || !s.Billing.Ready() {
		return RollupResolution{}, false, zserrors.New(zserrors.CodeNotReady, "Resolving usage needs the Kill Bill admin settings.")
	}
	var st rollupState
	if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		st, err = readRollupState(ctx, tx, sc, rollupID)
		return err
	}); err != nil {
		return RollupResolution{}, false, err
	}
	if st.resolved != nil {
		return *st.resolved, true, nil
	}
	if !st.confirmable() {
		return RollupResolution{}, false, errNotUnconfirmed
	}
	if decision == ResolveDecisionNotBilled && st.cancelDay == "" {
		return RollupResolution{}, false, errNotCancelled
	}
	acct, err := s.Billing.TenantAccount(ctx, s.DB, sc)
	if err != nil {
		return RollupResolution{}, false, err
	}
	invoices, err := acct.Invoices(ctx)
	if err != nil {
		return RollupResolution{}, false, err
	}
	carrying, later := "", false
	for _, in := range invoices {
		if in.Status != "COMMITTED" || (in.AccountID != "" && in.AccountID != acct.ID()) {
			continue
		}
		if slices.Contains(in.TrackingIDs, st.tracking) && carrying == "" {
			carrying = in.InvoiceID
		}
		if st.cancelDay != "" && len(in.InvoiceDate) >= 10 && in.InvoiceDate[:10] >= st.cancelDay {
			later = true
		}
	}
	invoiceID, unbilled := "", int64(0)
	switch decision {
	case ResolveDecisionForwarded:
		if carrying == "" {
			return RollupResolution{}, false, errNoEvidence
		}
		invoiceID = carrying
	case ResolveDecisionNotBilled:
		if carrying != "" {
			return RollupResolution{}, false, errCarried
		}
		if !later {
			return RollupResolution{}, false, errNoLaterInvoice
		}
		unbilled = st.quantity
	}
	var out RollupResolution
	replayed := false
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		cur, err := readRollupState(ctx, tx, sc, rollupID)
		if err != nil {
			return err
		}
		if cur.resolved != nil {
			out, replayed = *cur.resolved, true
			return nil
		}
		if !cur.confirmable() {
			return errNotUnconfirmed
		}
		args, _ := sc.Bind(cur.rollupUID, decision, nullable(invoiceID), nullableQuantity(unbilled), actorKey(call), reason)
		if err := tx.QueryRow(ctx, `INSERT INTO usage_rollup_resolutions (brand_id, tenant_id, rollup_id, decision, invoice_id, unbilled_quantity_micro, resolved_by, note)
VALUES ($1, $2, $3, $4, $5::uuid, $6, $7, $8) RETURNING rollup_id, decision, coalesce(invoice_id::text, ''), coalesce(unbilled_quantity_micro, 0), resolved_by, resolved_at`, args...).
			Scan(&cur.rollupUID, &out.Decision, &out.InvoiceID, &out.UnbilledQuantityMicro, &out.ResolvedBy, &out.ResolvedAt); err != nil {
			return unavailable(err)
		}
		out.RollupID, out.ResolvedAt = idgen.Format(idgen.Rollup, cur.rollupUID), out.ResolvedAt.UTC()
		details := map[string]any{"rollup_id": out.RollupID, "decision": decision, "killbill_tracking_id": cur.tracking, "unresolved_reason": cur.reason}
		if invoiceID != "" {
			details["killbill_invoice_id"] = invoiceID
		}
		if unbilled > 0 {
			details["unbilled_quantity_micro"] = unbilled
		}
		_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "metering.rollup_resolved", Reason: reason,
			Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: details})
		return err
	})
	return out, replayed, err
}

func actorKey(call StaffCall) string { return string(call.Actor.Type) + ":" + call.Actor.ID }

func nullableQuantity(q int64) any {
	if q == 0 {
		return nil
	}
	return q
}
