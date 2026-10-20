package billing

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// Hold reasons: why billing changes, and for some of them entitlement, are
// on hold for a tenant.
const (
	HoldTenantSuspended            = "tenant_suspended"
	HoldAccountBlocked             = "account_blocked"
	HoldOverdueBlocksChanges       = "overdue_blocks_changes"
	HoldOverdueDisablesEntitlement = "overdue_disables_entitlement"
)

// Overdue is the overdue state of a Kill Bill account.
type Overdue struct {
	State               string `json:"state"`
	Clear               bool   `json:"clear"`
	BlocksChanges       bool   `json:"blocks_changes"`
	DisablesEntitlement bool   `json:"disables_entitlement"`
}

// Summary is a tenant's billing account as Kill Bill holds it.
// BalanceMinor is what the account owes, negative when it holds credit, and
// CreditMinor the account credit, both in Currency. Core stores neither.
// HoldReasons is empty unless billing changes are on hold.
type Summary struct {
	Currency      string   `json:"currency"`
	BalanceMinor  int64    `json:"balance_minor"`
	CreditMinor   int64    `json:"credit_minor"`
	Overdue       Overdue  `json:"overdue"`
	OnHold        bool     `json:"on_hold"`
	HoldReasons   []string `json:"hold_reasons"`
	PaymentPlugin string   `json:"payment_plugin"`
}

// standing is a tenant's billing standing as Kill Bill reports it now.
type standing struct {
	overdue Overdue
	reasons []string
}

func (st standing) onHold() bool { return len(st.reasons) > 0 }

// holdsEntitlement reports whether the hold also stops entitlement. An
// overdue state that blocks changes only does not.
func (st standing) holdsEntitlement() bool {
	for _, r := range st.reasons {
		if r != HoldOverdueBlocksChanges {
			return true
		}
	}
	return false
}

// standing reads the overdue state and the blocking states of the tenant's
// Kill Bill account, and refreshes the standing cache.
func (s *Service) standing(ctx context.Context, a account) (standing, error) {
	st, err := s.readStanding(ctx, a)
	if err != nil {
		return standing{}, err
	}
	if err := s.saveStanding(ctx, s.DB, a.tenant.Scope(), st.overdue, ""); err != nil {
		// The cache is a convenience; Kill Bill answered, so the caller
		// gets the answer.
		s.log().WarnContext(ctx, "billing standing cache not updated", "error", err.Error())
	}
	return st, nil
}

// readStanding reads the overdue state and the blocking states of the
// tenant's Kill Bill account and writes nothing.
func (s *Service) readStanding(ctx context.Context, a account) (standing, error) {
	od, err := s.KillBill.AccountOverdue(ctx, a.creds, a.id())
	if err != nil {
		return standing{}, kbError(err)
	}
	states, err := s.KillBill.AccountBlockingStates(ctx, a.creds, a.id())
	if err != nil {
		return standing{}, kbError(err)
	}
	st := standing{overdue: Overdue{State: od.Name, Clear: od.IsClearState, BlocksChanges: od.IsBlockChanges,
		DisablesEntitlement: od.IsDisableEntitlementAndChangesBlocked}, reasons: []string{}}
	if a.tenant.Status == tenant.StatusSuspended {
		st.reasons = append(st.reasons, HoldTenantSuspended)
	}
	if blocked(states) {
		st.reasons = append(st.reasons, HoldAccountBlocked)
	}
	if od.IsBlockChanges {
		st.reasons = append(st.reasons, HoldOverdueBlocksChanges)
	}
	if od.IsDisableEntitlementAndChangesBlocked {
		st.reasons = append(st.reasons, HoldOverdueDisablesEntitlement)
	}
	return st, nil
}

// blocked reports whether a blocking state in force blocks changes,
// entitlement or billing. Per service the latest state is in force. Overdue
// states are reported by the overdue state and left out here.
func blocked(states []killbill.BlockingState) bool {
	latest := map[string]killbill.BlockingState{}
	for _, s := range states {
		if s.Service == "overdue-service" {
			continue
		}
		if prev, ok := latest[s.Service]; !ok || s.EffectiveDate >= prev.EffectiveDate {
			latest[s.Service] = s
		}
	}
	for _, s := range latest {
		if s.IsBlockChange || s.IsBlockEntitlement || s.IsBlockBilling {
			return true
		}
	}
	return false
}

// saveStanding upserts the standing cache. eventType is the Kill Bill event
// that caused the refresh, or "" for a read.
func (s *Service) saveStanding(ctx context.Context, q rowQuerier, sc scope.Scope, od Overdue, eventType string) error {
	args, err := sc.Bind(od.State, od.BlocksChanges, od.DisablesEntitlement, nullable(eventType), s.now())
	if err != nil {
		return err
	}
	var one int
	err = q.QueryRow(ctx, `INSERT INTO tenant_billing_standing (brand_id, tenant_id, overdue_state, overdue_blocks_changes,
    overdue_disables_entitlement, last_event_type, last_event_at, refreshed_at)
VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $6::text IS NULL THEN NULL ELSE $7::timestamptz END, $7)
ON CONFLICT (brand_id, tenant_id) DO UPDATE SET
    overdue_state = EXCLUDED.overdue_state,
    overdue_blocks_changes = EXCLUDED.overdue_blocks_changes,
    overdue_disables_entitlement = EXCLUDED.overdue_disables_entitlement,
    last_event_type = coalesce(EXCLUDED.last_event_type, tenant_billing_standing.last_event_type),
    last_event_at = coalesce(EXCLUDED.last_event_at, tenant_billing_standing.last_event_at),
    refreshed_at = EXCLUDED.refreshed_at
RETURNING 1`, args...).Scan(&one)
	if err != nil {
		return unavailable(err)
	}
	return nil
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Summary returns the tenant's billing account: currency, balance and
// credit from Kill Bill, the overdue state, whether billing is on hold and
// why, and the payment plugin of the brand.
func (s *Service) Summary(ctx context.Context, sc scope.Scope) (Summary, error) {
	a, err := s.account(ctx, sc)
	if err != nil {
		return Summary{}, err
	}
	acct, err := s.KillBill.AccountWithBalance(ctx, a.creds, a.id())
	if err != nil {
		return Summary{}, kbError(err)
	}
	st, err := s.standing(ctx, a)
	if err != nil {
		return Summary{}, err
	}
	balance, err := ToMinor(acct.AccountBalance, acct.Currency)
	if err != nil {
		return Summary{}, kbError(err)
	}
	credit, err := ToMinor(acct.AccountCBA, acct.Currency)
	if err != nil {
		return Summary{}, kbError(err)
	}
	return Summary{
		Currency: acct.Currency, BalanceMinor: balance, CreditMinor: credit, Overdue: st.overdue,
		OnHold: st.onHold(), HoldReasons: st.reasons, PaymentPlugin: strings.TrimSpace(a.pluginName()),
	}, nil
}
