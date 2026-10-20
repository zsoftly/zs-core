package wallet

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// View is a tenant's wallet: what Kill Bill holds as account credit, what
// Kill Bill will invoice at the end of the period and has not yet, what
// Core holds against it, and what is left. AvailableMinor is Available of
// these and the brand's overdraft allowance, the one number Reserve uses.
// When Kill Bill cannot rate the unbilled amount, UnbilledMinor and
// AvailableMinor are nil. Core stores none of these amounts.
type View struct {
	Currency                 string          `json:"currency"`
	KillBillCreditMinor      int64           `json:"killbill_credit_minor"`
	UnbilledMinor            *int64          `json:"unbilled_minor"`
	ReservedMinor            int64           `json:"reserved_minor"`
	AvailableMinor           *int64          `json:"available_minor"`
	OverdraftMinor           int64           `json:"overdraft_minor"`
	PendingSettlements       int             `json:"pending_settlements"`
	Overdue                  billing.Overdue `json:"overdue"`
	OnHold                   bool            `json:"on_hold"`
	HoldReasons              []string        `json:"hold_reasons"`
	LowBalance               bool            `json:"low_balance"`
	LowBalanceThresholdMinor int64           `json:"low_balance_threshold_minor"`
}

// View reads the tenant's wallet. The unbilled amount, the credit, the
// overdue state and the holds come from Kill Bill on every call, in that
// order, never from the cache, and the reservations from Core. When the
// wallet is below the brand's low-balance threshold, wallet.low_balance is
// emitted, at most once per LowBalanceEvery for the tenant.
func (s *Service) View(ctx context.Context, sc scope.Scope) (View, error) {
	if !s.Ready() || s.DB == nil {
		return View{}, errNotReady
	}
	acct, err := s.Billing.TenantAccount(ctx, s.DB, sc)
	if err != nil {
		return View{}, err
	}
	// The version is read before Kill Bill, so a cache entry is never newer
	// than its version says.
	version, err := walletVersion(ctx, s.DB, sc)
	if err != nil {
		return View{}, err
	}
	var unbilled *int64
	if u, err := acct.Unbilled(ctx); err != nil {
		s.log().WarnContext(ctx, "unbilled usage unknown", "tenant_id", sc.TenantID(), "error", err.Error())
	} else {
		unbilled = &u
	}
	snap, err := acct.Snapshot(ctx)
	if err != nil {
		return View{}, err
	}
	if unbilled != nil {
		s.remember(sc, snap.CreditMinor, *unbilled, version, s.now())
	}
	held, err := heldMinor(ctx, s.DB, sc)
	if err != nil {
		return View{}, err
	}
	args, err := sc.Bind()
	if err != nil {
		return View{}, err
	}
	var pending int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE brand_id = $1 AND tenant_id = $2 AND status = 'held' AND settlement_pending`,
		args...).Scan(&pending); err != nil {
		return View{}, unavailable(err)
	}
	policy := acct.Brand().Manifest.Billing
	v := View{Currency: snap.Currency, KillBillCreditMinor: snap.CreditMinor, ReservedMinor: held, OverdraftMinor: policy.Overdraft(),
		PendingSettlements: pending, Overdue: snap.Overdue, OnHold: len(snap.HoldReasons) > 0, HoldReasons: snap.HoldReasons,
		LowBalanceThresholdMinor: policy.LowBalance()}
	if v.HoldReasons == nil {
		v.HoldReasons = []string{}
	}
	if unbilled == nil {
		return v, nil
	}
	v.UnbilledMinor = unbilled
	available := Available(snap.CreditMinor, *unbilled, held, v.OverdraftMinor)
	v.AvailableMinor = &available
	v.LowBalance = v.LowBalanceThresholdMinor > 0 && available < v.LowBalanceThresholdMinor
	if v.LowBalance {
		call := Call{Actor: SweeperActor, CorrelationID: "wallet-view-" + sc.TenantID()}
		err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
			return s.lowBalance(ctx, tx, sc, call, acct, available, snap.Currency)
		})
		if err != nil {
			// The view is Kill Bill's answer. A missed alert is retried on
			// the next read.
			s.log().WarnContext(ctx, "low balance event not written", "tenant_id", sc.TenantID(), "error", err.Error())
		}
	}
	return v, nil
}
