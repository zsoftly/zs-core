package tenant

import (
	"context"
	"errors"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Engines are the Keycloak and Kill Bill clients and the secret resolver
// that tenant creation, suspension, closure and the sweeper use. The Kill
// Bill tenant secret of a brand is resolved from the brand's secret
// reference at each call and never stored.
type Engines struct {
	Keycloak *keycloak.Client
	KillBill *killbill.Client
	Secrets  secrets.Resolver
}

// Ready reports whether every engine is configured.
func (e Engines) Ready() bool { return e.Keycloak != nil && e.KillBill != nil && e.Secrets != nil }

// Upstream names Core writes into the engines.
const (
	// OrganizationTenantAttribute holds the Core tenant ID on the Keycloak
	// Organization. The alias holds it too.
	OrganizationTenantAttribute = "zs_tenant_id"
	// OrganizationBrandAttribute holds the Core brand ID.
	OrganizationBrandAttribute = "zs_brand_id"
	// BlockingService is the Kill Bill blocking state service of Core.
	BlockingService = "zs-core-tenant"
	// BlockingSuspended and BlockingActive are its two states.
	BlockingSuspended = "ZS_TENANT_SUSPENDED"
	BlockingActive    = "ZS_TENANT_ACTIVE"
	// BlockingClosed is the state of a closed tenant's Kill Bill account: it
	// blocks changes and entitlements but not billing. The account is blocked,
	// never closed, because closing it writes off the unpaid invoices.
	BlockingClosed = "ZS_TENANT_CLOSED"
)

// brandEngines is what the engines need to act for one brand.
type brandEngines struct {
	brandID  idgen.UUID
	slug     string
	realm    string
	creds    killbill.TenantCredentials
	currency string
	// locales and defaultLocale are the brand's locales.
	locales       []string
	defaultLocale string
}

func (e Engines) forBrand(ctx context.Context, q Querier, id idgen.UUID) (brandEngines, error) {
	if !e.Ready() {
		return brandEngines{}, zserrors.New(zserrors.CodeNotReady, "Tenant engines need the Keycloak and Kill Bill admin settings.")
	}
	return e.forBillingBrand(ctx, q, id)
}

// forBillingBrand needs only billing credentials, so a failed or unavailable
// identity engine does not prevent recipient correction or drift inspection.
func (e Engines) forBillingBrand(ctx context.Context, q Querier, id idgen.UUID) (brandEngines, error) {
	if e.KillBill == nil || e.Secrets == nil {
		return brandEngines{}, zserrors.New(zserrors.CodeNotReady, "Billing recipient reconciliation needs the Kill Bill admin settings.")
	}
	rec, err := brand.Get(ctx, q, id)
	if err != nil {
		return brandEngines{}, err
	}
	return e.billingFor(ctx, rec)
}

// billingFor builds the billing engines of a brand record that the caller
// already read, so a caller that holds a lock reads the brand once.
func (e Engines) billingFor(ctx context.Context, rec brand.Record) (brandEngines, error) {
	if e.KillBill == nil || e.Secrets == nil {
		return brandEngines{}, zserrors.New(zserrors.CodeNotReady, "Billing recipient reconciliation needs the Kill Bill admin settings.")
	}
	if rec.Status != brand.StatusActive || rec.AppliedAt.IsZero() || rec.Manifest == nil || len(rec.Manifest.Billing.Currencies) == 0 {
		return brandEngines{}, zserrors.New(zserrors.CodeNotReady, "The brand is not applied.")
	}
	secret, err := e.Secrets.Resolve(ctx, rec.Slug, rec.KillBillAPISecretRef)
	if err != nil {
		return brandEngines{}, zserrors.Wrap(zserrors.CodeBillingUnavailable, "", err)
	}
	return brandEngines{
		brandID: rec.ID, slug: rec.Slug, realm: rec.KeycloakRealm,
		creds:    killbill.TenantCredentials{APIKey: rec.KillBillAPIKey, APISecret: secret},
		currency: rec.Manifest.Billing.Currencies[0],
		locales:  rec.Manifest.Identity.Locales, defaultLocale: rec.Manifest.Identity.DefaultLocale,
	}, nil
}

// engineError maps an engine failure to a typed, retryable error. The
// upstream message stays in the cause, which is logged and never returned.
func engineError(err error) error {
	var zsErr *zserrors.Error
	var kcErr *keycloak.Error
	var kbErr *killbill.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &zsErr):
		return err
	case keycloak.IsUnavailable(err), errors.As(err, &kcErr),
		errors.Is(err, keycloak.ErrNotFound), errors.Is(err, keycloak.ErrConflict):
		return zserrors.Wrap(zserrors.CodeIdentityProviderUnavailable, "", err)
	case killbill.IsUnavailable(err), errors.As(err, &kbErr), errors.Is(err, killbill.ErrNotFound),
		errors.Is(err, killbill.ErrConflict), errors.Is(err, killbill.ErrCredentialsRejected):
		return zserrors.Wrap(zserrors.CodeBillingUnavailable, "", err)
	}
	return zserrors.Wrap(zserrors.CodeInternal, "", err)
}

func codeName(err error) string {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return zserrors.Lookup(zsErr.Code).Name
	}
	return "internal_error"
}

func callContext(correlationID, reason string) killbill.CallContext {
	return killbill.CallContext{CreatedBy: "zs-core", Reason: reason, Comment: correlationID}
}
