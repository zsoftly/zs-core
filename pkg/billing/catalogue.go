package billing

import (
	"context"
	"fmt"
	"regexp"

	"github.com/zsoftly/zs-core/internal/killbill"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Catalogue is the summary of the catalogue version in force for a brand.
// Plans, prices, trials and usage units live in the Kill Bill catalogue XML
// the brand manifest uploads; this is a read of it.
type Catalogue struct {
	Name          string    `json:"name"`
	EffectiveDate string    `json:"effective_date"`
	Currencies    []string  `json:"currencies"`
	Units         []string  `json:"units"`
	Products      []Product `json:"products"`
}

// Product is a catalogue product. Features are what an active subscription
// to one of its plans entitles to: the product name and the usage units of
// its plans.
type Product struct {
	Name       string   `json:"name"`
	PrettyName string   `json:"pretty_name,omitempty"`
	Category   string   `json:"category"`
	Features   []string `json:"features"`
	Plans      []Plan   `json:"plans"`
}

// Plan is a plan of a product.
type Plan struct {
	Name          string  `json:"name"`
	PrettyName    string  `json:"pretty_name,omitempty"`
	BillingPeriod string  `json:"billing_period"`
	Phases        []Phase `json:"phases"`
}

// Phase is a phase of a plan. Duration is nil for an unlimited phase.
type Phase struct {
	Type            string         `json:"type"`
	Duration        *PhaseDuration `json:"duration"`
	FixedPrices     []Money        `json:"fixed_prices"`
	RecurringPrices []Money        `json:"recurring_prices"`
	Usage           []UsagePrice   `json:"usage"`
}

// PhaseDuration is the length of a phase.
type PhaseDuration struct {
	Unit   string `json:"unit"`
	Number int    `json:"number"`
}

// UsagePrice prices blocks of a usage unit.
type UsagePrice struct {
	Unit      string  `json:"unit"`
	BlockSize string  `json:"block_size"`
	Prices    []Money `json:"prices"`
}

// current returns the catalogue version in force, the last one Kill Bill
// returns.
func current(versions []killbill.Catalog) (killbill.Catalog, bool) {
	if len(versions) == 0 {
		return killbill.Catalog{}, false
	}
	return versions[len(versions)-1], true
}

// Catalogue returns the summary of a brand's catalogue in force.
func (s *Service) Catalogue(ctx context.Context, brandID idgen.UUID) (Catalogue, error) {
	_, creds, err := s.brandCreds(ctx, brandID)
	if err != nil {
		return Catalogue{}, err
	}
	versions, err := s.KillBill.Catalogue(ctx, creds)
	if err != nil {
		return Catalogue{}, kbError(err)
	}
	c, ok := current(versions)
	if !ok {
		return Catalogue{Currencies: []string{}, Units: []string{}, Products: []Product{}}, nil
	}
	return summarize(c)
}

func summarize(c killbill.Catalog) (Catalogue, error) {
	out := Catalogue{Name: c.Name, EffectiveDate: c.EffectiveDate, Currencies: append([]string{}, c.Currencies...),
		Units: []string{}, Products: []Product{}}
	for _, u := range c.Units {
		out.Units = append(out.Units, u.Name)
	}
	for _, p := range c.Products {
		prod := Product{Name: p.Name, PrettyName: p.PrettyName, Category: p.Type, Features: features(p), Plans: []Plan{}}
		for _, pl := range p.Plans {
			plan := Plan{Name: pl.Name, PrettyName: pl.PrettyName, BillingPeriod: pl.BillingPeriod, Phases: []Phase{}}
			for _, ph := range pl.Phases {
				phase := Phase{Type: ph.Type, Usage: []UsagePrice{}}
				if ph.Duration.Unit != "" && ph.Duration.Unit != "UNLIMITED" {
					phase.Duration = &PhaseDuration{Unit: ph.Duration.Unit, Number: ph.Duration.Number}
				}
				var err error
				if phase.FixedPrices, err = prices(ph.FixedPrices); err != nil {
					return Catalogue{}, fmt.Errorf("catalogue plan %s: %w", pl.Name, err)
				}
				if phase.RecurringPrices, err = prices(ph.Prices); err != nil {
					return Catalogue{}, fmt.Errorf("catalogue plan %s: %w", pl.Name, err)
				}
				for _, u := range ph.Usages {
					for _, t := range u.Tiers {
						for _, b := range t.Blocks {
							ps, err := prices(b.Prices)
							if err != nil {
								return Catalogue{}, fmt.Errorf("catalogue plan %s: %w", pl.Name, err)
							}
							phase.Usage = append(phase.Usage, UsagePrice{Unit: b.Unit, BlockSize: b.Size, Prices: ps})
						}
					}
				}
				plan.Phases = append(plan.Phases, phase)
			}
			prod.Plans = append(prod.Plans, plan)
		}
		out.Products = append(out.Products, prod)
	}
	return out, nil
}

// features lists what a product entitles to: its name, then the usage units
// of its plans, without duplicates.
func features(p killbill.CatalogProduct) []string {
	seen := map[string]bool{p.Name: true}
	out := []string{p.Name}
	for _, pl := range p.Plans {
		for _, ph := range pl.Phases {
			for _, u := range ph.Usages {
				for _, t := range u.Tiers {
					for _, b := range t.Blocks {
						if b.Unit != "" && !seen[b.Unit] {
							seen[b.Unit] = true
							out = append(out, b.Unit)
						}
					}
				}
			}
		}
	}
	return out
}

// Entitlement answers whether a tenant may use a feature now. Limit, Used
// and Remaining are nil without a quota: package quota sets them. Plan
// names the plan that grants the feature when Allowed.
type Entitlement struct {
	Allowed   bool   `json:"allowed"`
	Limit     *int64 `json:"limit"`
	Used      *int64 `json:"used"`
	Remaining *int64 `json:"remaining"`
	Reason    string `json:"reason"`
	Plan      string `json:"plan,omitempty"`
}

// Entitlement reasons.
const (
	ReasonIncluded             = "included_in_plan"
	ReasonNoActiveSubscription = "no_active_subscription"
	ReasonNotInPlan            = "feature_not_in_plan"
	ReasonWalletOnHold         = "wallet_on_hold"
	ReasonBillingUnavailable   = "billing_unavailable"
	ReasonTenantNotFound       = "tenant_not_found"
	ReasonFeatureInvalid       = "feature_invalid"
	ReasonQuotaExceeded        = "quota_exceeded"
)

var featurePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,99}$`)

// ValidFeature reports whether a feature name has the accepted form.
func ValidFeature(feature string) bool { return featurePattern.MatchString(feature) }

// EntitlementState is what an entitlement check reads from Kill Bill for a
// tenant: the reason it may not use anything now, or its active
// subscriptions and the catalogue in force. It is built by
// Service.EntitlementState, which fails closed.
type EntitlementState struct {
	// Deny is the reason every feature is refused, or "" when the tenant
	// has at least one active subscription and no hold that stops
	// entitlement.
	Deny    string
	Active  []killbill.Subscription
	Catalog killbill.Catalog
}

// Grants reports whether an active subscription's product has feature, the
// product name or a usage unit of its plans, and names that plan.
func (st EntitlementState) Grants(feature string) (string, bool) {
	for _, sub := range st.Active {
		for _, p := range st.Catalog.Products {
			if p.Name != sub.ProductName {
				continue
			}
			for _, f := range features(p) {
				if f == feature {
					return sub.PlanName, true
				}
			}
		}
	}
	return "", false
}

// Plans returns the plans of the active subscriptions, in Kill Bill's
// order, without duplicates.
func (st EntitlementState) Plans() []string {
	seen := map[string]bool{}
	var out []string
	for _, sub := range st.Active {
		if !seen[sub.PlanName] {
			seen[sub.PlanName] = true
			out = append(out, sub.PlanName)
		}
	}
	return out
}

// EntitlementState reads the tenant's standing, its active Kill Bill
// subscriptions and the catalogue in force. It fails closed: a tenant that
// is closed or unknown, on hold (suspended in Core, or its Kill Bill account
// blocked or overdue in a state that disables entitlement), without an
// active subscription, or a Kill Bill that cannot be reached all give a
// state with Deny set.
func (s *Service) EntitlementState(ctx context.Context, sc scope.Scope) EntitlementState {
	a, err := s.account(ctx, sc)
	if err != nil {
		if zserrors.IsCode(err, zserrors.CodeTenantNotFound) {
			return EntitlementState{Deny: ReasonTenantNotFound}
		}
		s.log().WarnContext(ctx, "entitlement check failed closed", "error", err.Error())
		return EntitlementState{Deny: ReasonBillingUnavailable}
	}
	return s.entitlementState(ctx, a, true)
}

// EntitlementState is Service.EntitlementState for an account already
// loaded. It writes nothing to the database.
func (k *TenantAccount) EntitlementState(ctx context.Context) EntitlementState {
	return k.s.entitlementState(ctx, k.a, false)
}

// entitlementState reads the standing, the active subscriptions and the
// catalogue of an account, refreshing the standing cache when save is set.
func (s *Service) entitlementState(ctx context.Context, a account, save bool) EntitlementState {
	deny := func(reason string) EntitlementState { return EntitlementState{Deny: reason} }
	read := s.readStanding
	if save {
		read = s.standing
	}
	st, err := read(ctx, a)
	if err != nil {
		s.log().WarnContext(ctx, "entitlement check failed closed", "error", err.Error())
		return deny(ReasonBillingUnavailable)
	}
	if st.holdsEntitlement() {
		return deny(ReasonWalletOnHold)
	}
	bundles, err := s.KillBill.AccountBundles(ctx, a.creds, a.id())
	if err != nil {
		s.log().WarnContext(ctx, "entitlement check failed closed", "error", err.Error())
		return deny(ReasonBillingUnavailable)
	}
	var active []killbill.Subscription
	for _, b := range bundles {
		for _, sub := range b.Subscriptions {
			if sub.State == subscriptionActive {
				active = append(active, sub)
			}
		}
	}
	if len(active) == 0 {
		return deny(ReasonNoActiveSubscription)
	}
	versions, err := s.KillBill.Catalogue(ctx, a.creds)
	if err != nil {
		s.log().WarnContext(ctx, "entitlement check failed closed", "error", err.Error())
		return deny(ReasonBillingUnavailable)
	}
	c, _ := current(versions)
	return EntitlementState{Active: active, Catalog: c}
}

// CheckEntitlement reads the tenant's active Kill Bill subscriptions and the
// features of their catalogue products and answers whether the tenant may
// use feature. It fails closed: a tenant that is closed or unknown, on hold
// (suspended in Core, or its Kill Bill account blocked or overdue in a state
// that disables entitlement), without an active subscription, or a Kill Bill
// that cannot be reached all give Allowed false with the reason. Quotas are
// applied by package quota on top of this check.
func (s *Service) CheckEntitlement(ctx context.Context, sc scope.Scope, feature string) Entitlement {
	if !ValidFeature(feature) {
		return Entitlement{Reason: ReasonFeatureInvalid}
	}
	st := s.EntitlementState(ctx, sc)
	if st.Deny != "" {
		return Entitlement{Reason: st.Deny}
	}
	if plan, ok := st.Grants(feature); ok {
		return Entitlement{Allowed: true, Reason: ReasonIncluded, Plan: plan}
	}
	return Entitlement{Reason: ReasonNotInPlan}
}
