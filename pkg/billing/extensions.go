package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// Querier is the subset of pgxpool.Pool and pgx.Tx the account reads need.
type Querier interface {
	tenant.Querier
}

// TenantAccount is the Kill Bill account of one tenant, for the wallet,
// metering and quota extensions. It is loaded through the caller's querier,
// usually its transaction, so a library caller that holds a transaction
// never needs a second database connection. Its methods call Kill Bill
// only and write nothing to the database.
type TenantAccount struct {
	s *Service
	a account
}

// TenantAccount loads the tenant of sc, its brand and the brand's Kill Bill
// credentials through q. A closed tenant is tenant_not_found.
func (s *Service) TenantAccount(ctx context.Context, q Querier, sc scope.Scope) (*TenantAccount, error) {
	if !s.Ready() {
		return nil, errNotReady
	}
	t, err := tenant.Get(ctx, q, sc)
	if err != nil {
		return nil, err
	}
	if t.Status == tenant.StatusClosed {
		return nil, tenant.ErrNotFound
	}
	rec, err := brand.Get(ctx, q, sc.Brand)
	if err != nil {
		return nil, err
	}
	if rec.Status != brand.StatusActive || rec.AppliedAt.IsZero() || rec.Manifest == nil || len(rec.Manifest.Billing.Currencies) == 0 {
		return nil, zserrors.New(zserrors.CodeNotReady, "The brand is not applied.")
	}
	secret, err := s.Secrets.Resolve(ctx, rec.Slug, rec.KillBillAPISecretRef)
	if err != nil {
		return nil, zserrors.Wrap(zserrors.CodeBillingUnavailable, "", err)
	}
	return &TenantAccount{s: s, a: account{tenant: t, brand: rec, creds: killbill.TenantCredentials{APIKey: rec.KillBillAPIKey, APISecret: secret}}}, nil
}

// ID returns the Kill Bill account ID.
func (k *TenantAccount) ID() string { return k.a.id() }

// Currency returns the account currency.
func (k *TenantAccount) Currency() string { return k.a.currency() }

// Tenant returns the tenant.
func (k *TenantAccount) Tenant() tenant.Tenant { return k.a.tenant }

// Brand returns the brand.
func (k *TenantAccount) Brand() brand.Record { return k.a.brand }

// Snapshot is the Kill Bill state of an account a wallet needs: its credit,
// its overdue state and why billing is on hold.
type Snapshot struct {
	Currency    string
	CreditMinor int64
	Overdue     Overdue
	HoldReasons []string
}

// HoldsEntitlement reports whether a hold stops entitlement and new holds:
// every hold reason but an overdue state that blocks changes only.
func (s Snapshot) HoldsEntitlement() bool {
	return standing{reasons: s.HoldReasons}.holdsEntitlement()
}

// Snapshot reads the account credit, the overdue state and the blocking
// states from Kill Bill. Kill Bill is the authority. Nothing is cached.
func (k *TenantAccount) Snapshot(ctx context.Context) (Snapshot, error) {
	currency, credit, err := k.Credit(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	st, err := k.s.readStanding(ctx, k.a)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Currency: currency, CreditMinor: credit, Overdue: st.overdue, HoldReasons: st.reasons}, nil
}

// Credit reads the account currency and credit from Kill Bill.
func (k *TenantAccount) Credit(ctx context.Context) (string, int64, error) {
	acct, err := k.s.KillBill.AccountWithBalance(ctx, k.a.creds, k.ID())
	if err != nil {
		return "", 0, kbError(err)
	}
	credit, err := ToMinor(acct.AccountCBA, acct.Currency)
	if err != nil {
		return "", 0, kbError(err)
	}
	return acct.Currency, credit, nil
}

// HoldReasons reads why billing is on hold for the tenant, from the tenant
// row the account was loaded with and from Kill Bill's overdue and
// blocking states. It is never cached.
func (k *TenantAccount) HoldReasons(ctx context.Context) ([]string, error) {
	st, err := k.s.readStanding(ctx, k.a)
	if err != nil {
		return nil, err
	}
	return st.reasons, nil
}

// unbilledHorizon is how far ahead the dry run looks: one billing period
// of a month and a day, so the usage of the current period, which Kill Bill
// invoices in arrears at its end, is in it.
func unbilledTarget(now time.Time) string { return now.UTC().AddDate(0, 1, 1).Format(time.DateOnly) }

// Unbilled asks Kill Bill, with a dry-run invoice, what the account will be
// invoiced at the end of the current period and has not been yet: the
// usage recorded so far and the recurring charges due by then, in minor
// units. Kill Bill rates it. Nothing is invoiced. Kill Bill 0.24.22 answers
// a dry run of today with nothing for usage billed in arrears, so the
// target date is a month and a day ahead.
func (k *TenantAccount) Unbilled(ctx context.Context) (int64, error) {
	u, err := k.UnbilledAmounts(ctx)
	return u.TotalMinor, err
}

// UnbilledAmounts is what a dry-run invoice says the account will be
// invoiced at the end of the current period: TotalMinor is its amount,
// recurring charges included, and UsageMinor the sum of its USAGE items.
type UnbilledAmounts struct {
	TotalMinor int64
	UsageMinor int64
}

// UnbilledAmounts asks Kill Bill for the dry-run invoice Unbilled reads,
// and returns its amount and the part of it that is usage.
func (k *TenantAccount) UnbilledAmounts(ctx context.Context) (UnbilledAmounts, error) {
	cc := killbill.CallContext{CreatedBy: "zs-core", Reason: "wallet unbilled usage"}
	in, found, err := k.s.KillBill.DryRunInvoice(ctx, k.a.creds, cc, k.ID(), unbilledTarget(k.s.now()))
	if err != nil {
		return UnbilledAmounts{}, kbError(err)
	}
	if !found {
		return UnbilledAmounts{}, nil
	}
	total, err := ToMinor(in.Amount, k.Currency())
	if err != nil {
		return UnbilledAmounts{}, kbError(err)
	}
	var usage int64
	for _, it := range in.Items {
		if it.ItemType != "USAGE" {
			continue
		}
		v, err := ToMinor(it.Amount, k.Currency())
		if err != nil {
			return UnbilledAmounts{}, kbError(err)
		}
		usage += v
	}
	return UnbilledAmounts{TotalMinor: max(total, 0), UsageMinor: max(usage, 0)}, nil
}

// Invoices returns the account's invoices with their items.
func (k *TenantAccount) Invoices(ctx context.Context) ([]killbill.Invoice, error) {
	out, err := k.s.KillBill.AccountInvoices(ctx, k.a.creds, k.ID(), true)
	if err != nil {
		return nil, kbError(err)
	}
	return out, nil
}

// FindCharge returns the account's external charge with the description,
// if Kill Bill holds one.
func (k *TenantAccount) FindCharge(ctx context.Context, description string) (killbill.InvoiceItem, bool, error) {
	return k.s.findItem(ctx, k.a, "EXTERNAL_CHARGE", description)
}

// Charge writes an external charge of amountMinor in the account currency
// with the description, which Kill Bill pays from the account credit
// first. A charge with the same description that Kill Bill already holds is
// returned instead, so a retry after a lost answer never charges twice.
// Kill Bill refuses an amount that is not positive.
func (k *TenantAccount) Charge(ctx context.Context, call Call, amountMinor int64, description string) (killbill.InvoiceItem, error) {
	if err := call.validate(); err != nil {
		return killbill.InvoiceItem{}, err
	}
	if item, found, err := k.FindCharge(ctx, description); err != nil || found {
		return item, err
	}
	amount, err := FromMinor(amountMinor, k.Currency())
	if err != nil {
		return killbill.InvoiceItem{}, validation("The currency is not supported.")
	}
	item, err := k.s.KillBill.CreateExternalCharge(ctx, k.a.creds, call.kb(), killbillItem(k.ID(), amount, k.Currency(), description))
	if err != nil {
		return killbill.InvoiceItem{}, kbError(err)
	}
	return item, nil
}

// UsagePlan is a subscription that takes usage of a unit, with the
// catalogue plan it is on.
type UsagePlan struct {
	Subscription killbill.Subscription
	Plan         killbill.CatalogPlan
}

// planHasUnit reports whether a phase of the plan prices the unit.
func planHasUnit(p killbill.CatalogPlan, unit string) bool {
	for _, ph := range p.Phases {
		for _, u := range ph.Usages {
			for _, t := range u.Tiers {
				for _, b := range t.Blocks {
					if b.Unit == unit {
						return true
					}
				}
			}
		}
	}
	return false
}

func findPlan(c killbill.Catalog, name string) (killbill.CatalogPlan, bool) {
	for _, p := range c.Products {
		for _, pl := range p.Plans {
			if pl.Name == name {
				return pl, true
			}
		}
	}
	return killbill.CatalogPlan{}, false
}

// errNoUsageSubscription is the answer when no active subscription of the
// tenant takes usage of the unit.
var errNoUsageSubscription = zserrors.New(zserrors.CodeBillingRejected, "No active subscription of the tenant takes usage of this unit.")

// ErrUsageBeforeStart is the cause of a refusal of usage dated before the
// subscription started: Kill Bill 0.24.22 records it and leaves it off the
// invoice.
var ErrUsageBeforeStart = errors.New("billing: the usage is dated before the subscription started")

// ErrSubscriptionCancelled is the cause of a refusal of usage on a
// cancelled or expired subscription, which Kill Bill 0.24.22 records and
// never bills.
var ErrSubscriptionCancelled = errors.New("billing: the subscription is cancelled")

// SubscriptionStart reads the start of a Kill Bill subscription, a date or
// a date and time, in UTC. A date starts at midnight UTC.
func SubscriptionStart(sub killbill.Subscription) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z07:00", time.DateOnly} {
		if t, err := time.Parse(layout, sub.StartDate); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// cancelled reports whether Kill Bill stopped a subscription for good.
func cancelled(sub killbill.Subscription) bool {
	return sub.State == "CANCELLED" || sub.State == "EXPIRED"
}

// startedBy reports whether the subscription started by at. A zero at
// skips the check. A start Core cannot read is taken as not started.
func startedBy(sub killbill.Subscription, at time.Time) bool {
	if at.IsZero() {
		return true
	}
	start, ok := SubscriptionStart(sub)
	return ok && !start.After(at)
}

// UsageSubscription returns the subscription usage of unit goes to. With
// requested set, it must be an active subscription of this account whose
// plan prices the unit. Otherwise it is the first active subscription whose
// plan does. Kill Bill 0.24.22 records usage of any unit on any
// subscription, cancelled ones included, so Core checks this itself.
func (k *TenantAccount) UsageSubscription(ctx context.Context, requested, unit string) (UsagePlan, error) {
	return k.UsageSubscriptionAt(ctx, requested, unit, time.Time{})
}

// UsageSubscriptionAt is UsageSubscription for usage dated at. A
// subscription that started after at does not take it: the refusal wraps
// ErrUsageBeforeStart. A requested subscription that is cancelled or
// expired is refused with ErrSubscriptionCancelled. A zero at skips the
// start check.
func (k *TenantAccount) UsageSubscriptionAt(ctx context.Context, requested, unit string, at time.Time) (UsagePlan, error) {
	versions, err := k.s.KillBill.Catalogue(ctx, k.a.creds)
	if err != nil {
		return UsagePlan{}, kbError(err)
	}
	c, _ := current(versions)
	if requested != "" {
		sub, err := k.s.ownedSubscription(ctx, k.a, requested)
		if err != nil {
			return UsagePlan{}, err
		}
		if cancelled(sub) {
			return UsagePlan{}, zserrors.Wrap(zserrors.CodeBillingRejected, "The subscription is cancelled.", ErrSubscriptionCancelled)
		}
		plan, ok := findPlan(c, sub.PlanName)
		if sub.State != subscriptionActive || !ok || !planHasUnit(plan, unit) {
			return UsagePlan{}, zserrors.New(zserrors.CodeBillingRejected, "The subscription is not active or its plan does not take usage of this unit.")
		}
		if !startedBy(sub, at) {
			return UsagePlan{}, zserrors.Wrap(zserrors.CodeBillingRejected, "The usage is dated before the subscription started.", ErrUsageBeforeStart)
		}
		return UsagePlan{Subscription: sub, Plan: plan}, nil
	}
	bundles, err := k.s.KillBill.AccountBundles(ctx, k.a.creds, k.ID())
	if err != nil {
		return UsagePlan{}, kbError(err)
	}
	later := false
	for _, b := range bundles {
		for _, sub := range b.Subscriptions {
			if sub.State != subscriptionActive {
				continue
			}
			plan, ok := findPlan(c, sub.PlanName)
			if !ok || !planHasUnit(plan, unit) {
				continue
			}
			if !startedBy(sub, at) {
				later = true
				continue
			}
			return UsagePlan{Subscription: sub, Plan: plan}, nil
		}
	}
	if later {
		return UsagePlan{}, zserrors.Wrap(zserrors.CodeBillingRejected, "The usage is dated before every subscription that takes it started.", ErrUsageBeforeStart)
	}
	return UsagePlan{}, errNoUsageSubscription
}

// OwnedSubscription returns a subscription of this account from Kill Bill.
// A subscription of another account is not_found.
func (k *TenantAccount) OwnedSubscription(ctx context.Context, id string) (killbill.Subscription, error) {
	return k.s.ownedSubscription(ctx, k.a, id)
}

// microPerUnit is the number of micro-units in one unit.
const microPerUnit = 1_000_000

// MicroToDecimal formats micro-units as the decimal Kill Bill takes, with
// six digits after the point.
func MicroToDecimal(micro int64) json.Number {
	r := new(big.Rat).SetFrac(big.NewInt(micro), big.NewInt(microPerUnit))
	return json.Number(r.FloatString(6))
}

// DecimalToMicro reads a Kill Bill decimal amount of a unit as micro-units.
// A value with more than six digits after the point is rounded half away
// from zero. A value out of range is an error.
func DecimalToMicro(v json.Number) (int64, error) {
	if v == "" {
		return 0, nil
	}
	r, ok := new(big.Rat).SetString(string(v))
	if !ok {
		return 0, fmt.Errorf("billing: amount is not a decimal")
	}
	r.Mul(r, new(big.Rat).SetInt64(microPerUnit))
	num, den := new(big.Int).Set(r.Num()), r.Denom()
	neg := num.Sign() < 0
	num.Abs(num)
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("billing: amount is out of range")
	}
	return q.Int64(), nil
}

// UsageAmount is an amount of a unit at a time, in micro-units. Kill Bill
// 0.24.22 leaves a usage record dated before the subscription started off
// the invoice, so the time is kept, not only the day.
type UsageAmount struct {
	At          time.Time
	AmountMicro int64
}

// RecordUsage writes usage of unit to a subscription of this account under
// trackingID. Kill Bill keeps one record per tracking ID and subscription,
// so a retry with the same tracking ID records nothing more: it returns
// duplicate true. The caller has chosen the subscription with
// UsageSubscription.
func (k *TenantAccount) RecordUsage(ctx context.Context, call Call, subscriptionID, unit, trackingID string, amounts []UsageAmount) (bool, error) {
	if err := call.validate(); err != nil {
		return false, err
	}
	if err := checkKillBillID(subscriptionID); err != nil {
		return false, err
	}
	rec := killbill.SubscriptionUsageRecord{SubscriptionID: subscriptionID, TrackingID: trackingID,
		UnitUsageRecords: []killbill.UnitUsageRecords{{UnitType: unit}}}
	for _, a := range amounts {
		if a.AmountMicro <= 0 {
			return false, validation("Usage amounts must be positive.")
		}
		rec.UnitUsageRecords[0].UsageRecords = append(rec.UnitUsageRecords[0].UsageRecords,
			killbill.UsageRecord{RecordDate: a.At.UTC().Format("2006-01-02T15:04:05.000Z"), Amount: MicroToDecimal(a.AmountMicro)})
	}
	err := k.s.KillBill.RecordUsage(ctx, k.a.creds, call.kb(), rec)
	if errors.Is(err, killbill.ErrTrackingIDExists) {
		return true, nil
	}
	if err != nil {
		return false, kbError(err)
	}
	return false, nil
}

// BilledUnit is the usage of a unit Kill Bill holds for a period.
type BilledUnit struct {
	Unit          string `json:"unit"`
	QuantityMicro int64  `json:"quantity_micro"`
}

// BilledUsage is the usage Kill Bill holds for a subscription and period.
type BilledUsage struct {
	SubscriptionID string       `json:"subscription_id"`
	From           string       `json:"from"`
	To             string       `json:"to"`
	Units          []BilledUnit `json:"units"`
}

// BilledUsage reads the usage Kill Bill holds for a subscription of this
// account from from, inclusive, to to, exclusive.
func (k *TenantAccount) BilledUsage(ctx context.Context, subscriptionID string, from, to time.Time) (BilledUsage, error) {
	if _, err := k.s.ownedSubscription(ctx, k.a, subscriptionID); err != nil {
		return BilledUsage{}, err
	}
	got, err := k.s.KillBill.RolledUpUsage(ctx, k.a.creds, subscriptionID, from.UTC().Format(time.DateOnly), to.UTC().Format(time.DateOnly))
	if err != nil {
		return BilledUsage{}, kbError(err)
	}
	out := BilledUsage{SubscriptionID: subscriptionID, From: from.UTC().Format(time.DateOnly), To: to.UTC().Format(time.DateOnly), Units: []BilledUnit{}}
	for _, u := range got.RolledUpUnits {
		micro, err := DecimalToMicro(u.Amount)
		if err != nil {
			return BilledUsage{}, kbError(err)
		}
		out.Units = append(out.Units, BilledUnit{Unit: u.UnitType, QuantityMicro: micro})
	}
	return out, nil
}

// Subscriptions returns every subscription of the account.
func (k *TenantAccount) Subscriptions(ctx context.Context) ([]Subscription, error) {
	bundles, err := k.s.KillBill.AccountBundles(ctx, k.a.creds, k.ID())
	if err != nil {
		return nil, kbError(err)
	}
	out := []Subscription{}
	for _, b := range bundles {
		for _, sub := range b.Subscriptions {
			out = append(out, subscriptionOf(sub))
		}
	}
	return out, nil
}

// EstimateUsage converts a quantity of a unit to minor units of the account
// currency with the catalogue price of the plan's phase: the first tier
// whose blocks price the unit, one block price per started block. It reads
// the price from the catalogue and stores none. Kill Bill's invoice stays
// the authority, so this is an estimate. It returns false when the phase
// does not price the unit in the account currency.
func (k *TenantAccount) EstimateUsage(plan killbill.CatalogPlan, phase, unit string, quantityMicro int64) (int64, bool) {
	for _, ph := range plan.Phases {
		if ph.Type != phase {
			continue
		}
		for _, u := range ph.Usages {
			for _, t := range u.Tiers {
				for _, b := range t.Blocks {
					if b.Unit != unit {
						continue
					}
					var price *killbill.Price
					for i := range b.Prices {
						if b.Prices[i].Currency == k.Currency() {
							price = &b.Prices[i]
						}
					}
					size, err := DecimalToMicro(json.Number(b.Size))
					if price == nil || err != nil || size <= 0 {
						return 0, false
					}
					perBlock, err := ToMinor(price.Value, k.Currency())
					if err != nil {
						return 0, false
					}
					blocks := new(big.Int).Div(new(big.Int).Add(big.NewInt(quantityMicro), big.NewInt(size-1)), big.NewInt(size))
					total := new(big.Int).Mul(blocks, big.NewInt(perBlock))
					if !total.IsInt64() {
						return 0, false
					}
					return total.Int64(), true
				}
			}
		}
	}
	return 0, false
}

// KBError maps a Kill Bill failure as the billing routes do:
// billing_unavailable when Kill Bill cannot be reached, billing_rejected
// with Kill Bill's error code when it refuses.
func KBError(err error) error { return kbError(err) }

// Unreachable reports whether err means Kill Bill could not be reached or
// did not answer, so a write may not have happened.
func Unreachable(err error) bool {
	return zserrors.IsCode(err, zserrors.CodeBillingUnavailable) || killbill.IsUnavailable(err) ||
		errors.Is(err, context.DeadlineExceeded)
}

// CoreCredits are the credits Core wrote to Kill Bill for a tenant: the
// Kill Bill credit item IDs of completed credits with their amounts, and
// the descriptions of pending ones, which have no item ID yet.
type CoreCredits struct {
	Items   map[string]int64
	Pending map[string]bool
}

// LoadCoreCredits reads the tenant's Core credits through q.
func LoadCoreCredits(ctx context.Context, q Querier, sc scope.Scope) (CoreCredits, error) {
	args, err := sc.Bind()
	if err != nil {
		return CoreCredits{}, err
	}
	out := CoreCredits{Items: map[string]int64{}, Pending: map[string]bool{}}
	rows, err := q.Query(ctx, `SELECT id, status, coalesce(killbill_credit_id::text, ''), amount_minor FROM credit_code_redemptions
WHERE brand_id = $1 AND tenant_id = $2
UNION ALL
SELECT id, status, coalesce(killbill_credit_id::text, ''), amount_minor FROM staff_credits
WHERE brand_id = $1 AND tenant_id = $2`, args...)
	if err != nil {
		return CoreCredits{}, unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id             idgen.UUID
			status, itemID string
			amount         int64
		)
		if err := rows.Scan(&id, &status, &itemID, &amount); err != nil {
			return CoreCredits{}, unavailable(err)
		}
		if status == "completed" && itemID != "" {
			out.Items[strings.ToLower(itemID)] = amount
		} else {
			out.Pending[creditDescription(id)] = true
		}
	}
	if err := rows.Err(); err != nil {
		return CoreCredits{}, unavailable(err)
	}
	return out, nil
}

// Execer runs a statement. pgx.Tx and pgxpool.Pool implement it.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// LockWallet takes the tenant's wallet lock for the rest of tx. Reserve
// holds it while it reads the holds and the wallet version, and every Core
// write that lowers the Kill Bill credit without a hold that covers it,
// such as a credit expiry, holds it across the Kill Bill call. The key
// names the schema, as the migration lock does, so two deployments that
// share a Postgres server never share a lock.
func LockWallet(ctx context.Context, tx Execer, sc scope.Scope) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('zs-core.wallet:' || current_schema() || ':' || $1::text || ':' || $2::text, 0))`,
		sc.Brand, sc.Tenant); err != nil {
		return unavailable(err)
	}
	return nil
}

// BumpWalletVersion moves the tenant's wallet version on, in tx. Call it in
// the transaction that records a change Core made to the tenant's Kill Bill
// credit or unbilled amount, or one it detected. Every Core instance then
// stops relying on the credit it cached for the tenant.
func BumpWalletVersion(ctx context.Context, tx Execer, sc scope.Scope) error {
	args, err := sc.Bind()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_versions (brand_id, tenant_id, version, updated_at) VALUES ($1, $2, 1, now())
ON CONFLICT (brand_id, tenant_id) DO UPDATE SET version = wallet_versions.version + 1, updated_at = now()`, args...); err != nil {
		return unavailable(err)
	}
	return nil
}
