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
}

// ResolveDecisionForwarded says Kill Bill holds the rollup: its tracking ID is
// on an invoice of the tenant's account.
const ResolveDecisionForwarded = "forwarded"

var (
	errNotUnconfirmed = zserrors.New(zserrors.CodeConflict, "Only a rollup whose send Kill Bill never confirmed can be confirmed.")
	errNoEvidence     = zserrors.New(zserrors.CodeConflict, "No invoice of the tenant's account carries the tracking ID of this rollup.")
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
}

func readRollupState(ctx context.Context, tx pgx.Tx, sc scope.Scope, rollupID string) (rollupState, error) {
	r, err := getRollup(ctx, tx, sc, rollupID)
	if err != nil {
		return rollupState{}, err
	}
	st := rollupState{status: r.Status, tracking: r.TrackingID, rollupUID: r.id}
	args, _ := sc.Bind(r.id)
	if err := tx.QueryRow(ctx, `SELECT send_protocol, coalesce(closure_unresolved_reason, ''), send_started_at IS NOT NULL FROM usage_rollups WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, args...).
		Scan(&st.protocol, &st.reason, &st.started); err != nil {
		return rollupState{}, unavailable(err)
	}
	var res RollupResolution
	var id idgen.UUID
	err = tx.QueryRow(ctx, `SELECT rollup_id, decision, invoice_id::text, resolved_by, resolved_at FROM usage_rollup_resolutions WHERE rollup_id = $1`, r.id).
		Scan(&id, &res.Decision, &res.InvoiceID, &res.ResolvedBy, &res.ResolvedAt)
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

// ResolveUnconfirmedRollup records that Kill Bill holds a rollup whose send
// it never confirmed. The evidence is Kill Bill's: the tracking ID of the rollup
// has to be on an invoice of the tenant's account, which is read here, and a
// rollup without that evidence is refused as a conflict. The confirmation is
// kept beside the rollup and does not change it, is audited as
// metering.rollup_resolved with the staff reason and the invoice, and is
// idempotent: the second call returns the first with replayed true. A tenant
// that is closing is drained once all its unconfirmed rollups are confirmed.
// Nothing here sends usage again.
func (s *Service) ResolveUnconfirmedRollup(ctx context.Context, sc scope.Scope, call StaffCall, rollupID string) (RollupResolution, bool, error) {
	reason, err := call.check()
	if err != nil {
		return RollupResolution{}, false, err
	}
	if s.DB == nil || !s.Billing.Ready() {
		return RollupResolution{}, false, zserrors.New(zserrors.CodeNotReady, "Confirming usage needs the Kill Bill admin settings.")
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
	acct, err := s.Billing.TenantAccount(ctx, s.DB, sc)
	if err != nil {
		return RollupResolution{}, false, err
	}
	invoices, err := acct.Invoices(ctx)
	if err != nil {
		return RollupResolution{}, false, err
	}
	invoiceID := ""
	for _, in := range invoices {
		if in.AccountID != "" && in.AccountID != acct.ID() {
			continue
		}
		if slices.Contains(in.TrackingIDs, st.tracking) {
			invoiceID = in.InvoiceID
			break
		}
	}
	if invoiceID == "" {
		return RollupResolution{}, false, errNoEvidence
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
		args, _ := sc.Bind(cur.rollupUID, ResolveDecisionForwarded, invoiceID, actorKey(call), reason)
		if err := tx.QueryRow(ctx, `INSERT INTO usage_rollup_resolutions (brand_id, tenant_id, rollup_id, decision, invoice_id, resolved_by, note)
VALUES ($1, $2, $3, $4, $5::uuid, $6, $7) RETURNING rollup_id, decision, invoice_id::text, resolved_by, resolved_at`, args...).
			Scan(&cur.rollupUID, &out.Decision, &out.InvoiceID, &out.ResolvedBy, &out.ResolvedAt); err != nil {
			return unavailable(err)
		}
		out.RollupID, out.ResolvedAt = idgen.Format(idgen.Rollup, cur.rollupUID), out.ResolvedAt.UTC()
		_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "metering.rollup_resolved", Reason: reason,
			Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: map[string]any{
				"rollup_id": out.RollupID, "decision": ResolveDecisionForwarded, "killbill_invoice_id": invoiceID,
				"killbill_tracking_id": cur.tracking, "unresolved_reason": cur.reason}})
		return err
	})
	return out, replayed, err
}

func actorKey(call StaffCall) string { return string(call.Actor.Type) + ":" + call.Actor.ID }
