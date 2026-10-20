// Package httpapi serves the ZSoftly Core HTTP API.
//
// The handler chain, outermost first, is: request context (request ID, W3C
// trace context and the access log), security headers including
// Cache-Control: no-store, panic recovery, the request body limit, and the
// router. /v1 routes then pass through authenticate, which verifies the
// bearer token or API key, chooses the brand and records the identity.
// Tenant routes also pass through member, which loads the tenant, the
// caller's membership and its effective permissions, and handlers call
// scope.Require. Staff routes check a staff grant with requireStaff. Every error leaves through
// writeError as the JSON error envelope.
// Request and response types come from internal/httpapi/gen, which is
// generated from api/openapi.yaml.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/quota"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

// MaxBodyBytes is the request body limit, 1 MiB.
const MaxBodyBytes int64 = 1 << 20

// DB is the subset of pgxpool.Pool the API needs.
type DB interface {
	Ping(ctx context.Context) error
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Options configure the API.
type Options struct {
	DB         DB
	Migrations fs.FS
	Logger     *slog.Logger
	// Verifier verifies bearer tokens. Without it every /v1 request is
	// 503 authentication_unavailable.
	Verifier *identity.Verifier
	// Brands is invalidated after a brand apply so new verification
	// settings take effect at once. Optional.
	Brands *brand.Registry
	// BrandDeps are the engine clients for POST /v1/brands/{id}/apply. When
	// Keycloak or Kill Bill is missing, that route is 503 not_ready.
	BrandDeps brand.Deps
	// BrandsDir is ZS_BRANDS_DIR. The file references of an apply of brand
	// <slug> resolve inside BrandsDir/<slug>/ and must start with "<slug>/".
	BrandsDir string
	// Tenants are the engines of tenant creation. Without them, creating a
	// tenant is 503 not_ready and memberships are mirrored to Keycloak only
	// by the sweeper.
	Tenants tenant.Engines
	// Billing is the billing surface on Kill Bill. Without it the billing
	// routes answer 503 not_ready, and entitlement checks answer allowed
	// false.
	Billing *billing.Service
	// Wallet is the wallet view and the reservations. Without it those
	// routes answer 503 not_ready.
	Wallet *wallet.Service
	// Metering records and reads usage. Without it, usage is still
	// recorded, and Kill Bill's billed usage answers 503 not_ready.
	Metering *metering.Service
	// Quota applies quotas to entitlement checks. Without it the check is
	// the billing one, and consume, release and overrides answer 503
	// not_ready.
	Quota *quota.Service
	// Now defaults to time.Now.
	Now func() time.Time
}

type server struct {
	db         DB
	migrations fs.FS
	log        *slog.Logger
	verifier   *identity.Verifier
	brands     *brand.Registry
	brandDeps  brand.Deps
	brandsDir  string
	tenants    tenant.Engines
	billing    *billing.Service
	wallet     *wallet.Service
	metering   *metering.Service
	quota      *quota.Service
	now        func() time.Time
}

// New returns the API handler.
func New(opts Options) http.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &server{
		db:         opts.DB,
		migrations: opts.Migrations,
		log:        log,
		verifier:   opts.Verifier,
		brands:     opts.Brands,
		brandDeps:  opts.BrandDeps,
		brandsDir:  opts.BrandsDir,
		tenants:    opts.Tenants,
		billing:    opts.Billing,
		wallet:     opts.Wallet,
		metering:   opts.Metering,
		quota:      opts.Quota,
		now:        opts.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /v1/me", s.authenticate(http.HandlerFunc(s.getMe)))
	mux.Handle("GET /v1/brands/{id}", s.authenticate(http.HandlerFunc(s.getBrand)))
	mux.Handle("POST /v1/brands/{id}/apply", s.authenticate(http.HandlerFunc(s.applyBrand)))
	mux.Handle("GET /v1/audit/events", s.authenticate(http.HandlerFunc(s.listAuditEvents)))

	mux.Handle("POST /v1/tenants/personal", s.authenticate(http.HandlerFunc(s.ensurePersonalTenant)))
	mux.Handle("POST /v1/tenants", s.authenticate(http.HandlerFunc(s.createTenant)))
	mux.Handle("POST /v1/invitations/accept", s.authenticate(http.HandlerFunc(s.acceptInvitation)))
	mux.Handle("POST /v1/staff/tenants/{id}/activation", s.authenticate(http.HandlerFunc(s.decideTenantActivation)))

	mux.Handle("GET /v1/tenants/{id}", s.member(s.getTenant))
	mux.Handle("PATCH /v1/tenants/{id}", s.member(s.updateTenant))
	mux.Handle("GET /v1/tenants/{id}/members", s.member(s.listMembers))
	mux.Handle("PATCH /v1/tenants/{id}/members/{mem}", s.member(s.updateMember))
	mux.Handle("DELETE /v1/tenants/{id}/members/{mem}", s.member(s.removeMember))
	mux.Handle("POST /v1/tenants/{id}/ownership/transfer", s.member(s.transferOwnership))
	mux.Handle("GET /v1/tenants/{id}/teams", s.member(s.listTeams))
	mux.Handle("POST /v1/tenants/{id}/teams", s.member(s.createTeam))
	mux.Handle("PATCH /v1/tenants/{id}/teams/{tm}", s.member(s.updateTeam))
	mux.Handle("DELETE /v1/tenants/{id}/teams/{tm}", s.member(s.deleteTeam))
	mux.Handle("PUT /v1/tenants/{id}/teams/{tm}/members", s.member(s.setTeamMembers))
	mux.Handle("PUT /v1/tenants/{id}/teams/{tm}/grants", s.member(s.setTeamGrants))
	mux.Handle("GET /v1/tenants/{id}/invitations", s.member(s.listInvitations))
	mux.Handle("POST /v1/tenants/{id}/invitations", s.member(s.createInvitation))
	mux.Handle("DELETE /v1/tenants/{id}/invitations/{inv}", s.member(s.revokeInvitation))
	mux.Handle("GET /v1/tenants/{id}/activation", s.member(s.getTenantActivation))
	mux.Handle("GET /v1/tenants/{id}/api-keys", s.member(s.listAPIKeys))
	mux.Handle("POST /v1/tenants/{id}/api-keys", s.member(s.createAPIKey))
	mux.Handle("DELETE /v1/tenants/{id}/api-keys/{key}", s.member(s.revokeAPIKey))
	mux.Handle("GET /v1/tenants/{id}/audit/events", s.member(s.listTenantAuditEvents))

	mux.Handle("GET /v1/brands/{id}/catalogue", s.authenticate(http.HandlerFunc(s.getCatalogue)))
	mux.Handle("GET /v1/tenants/{id}/billing", s.member(s.getBillingSummary))
	mux.Handle("GET /v1/tenants/{id}/subscriptions", s.member(s.listSubscriptions))
	mux.Handle("POST /v1/tenants/{id}/subscriptions", s.member(s.createSubscription))
	mux.Handle("POST /v1/tenants/{id}/subscriptions/{sub}/change", s.member(s.changeSubscriptionPlan))
	mux.Handle("DELETE /v1/tenants/{id}/subscriptions/{sub}", s.member(s.cancelSubscription))
	mux.Handle("GET /v1/tenants/{id}/entitlements/{feature}", s.member(s.checkEntitlement))
	mux.Handle("GET /v1/tenants/{id}/invoices", s.member(s.listInvoices))
	mux.Handle("GET /v1/tenants/{id}/invoices/{inv}", s.member(s.getInvoice))
	mux.Handle("GET /v1/tenants/{id}/invoices/{inv}/pdf", s.member(s.getInvoicePDF))
	mux.Handle("GET /v1/tenants/{id}/payments", s.member(s.listPayments))
	mux.Handle("POST /v1/tenants/{id}/payments/{pay}/refund", s.authenticate(http.HandlerFunc(s.refundPayment)))
	mux.Handle("GET /v1/tenants/{id}/payment-methods", s.member(s.listPaymentMethods))
	mux.Handle("POST /v1/tenants/{id}/payment-methods", s.member(s.addPaymentMethod))
	mux.Handle("PUT /v1/tenants/{id}/payment-methods/{pm}/default", s.member(s.setDefaultPaymentMethod))
	mux.Handle("DELETE /v1/tenants/{id}/payment-methods/{pm}", s.member(s.deletePaymentMethod))
	mux.Handle("POST /v1/tenants/{id}/credits/redeem", s.member(s.redeemCreditCode))
	mux.Handle("POST /v1/staff/tenants/{id}/credits", s.authenticate(http.HandlerFunc(s.grantCredit)))
	mux.Handle("GET /v1/tenants/{id}/billing/credentials", s.member(s.getBillingCredentials))
	mux.HandleFunc("GET /v1/tenants/{id}/billing/proxy/{path...}", s.proxyBilling)
	mux.HandleFunc("POST /v1/tenants/{id}/billing/proxy/{path...}", s.proxyBilling)
	mux.HandleFunc("POST /v1/billing/webhooks/killbill/{brand}/{token}", s.receiveKillBillNotification)

	mux.Handle("GET /v1/tenants/{id}/wallet", s.member(s.getWallet))
	mux.Handle("GET /v1/tenants/{id}/reservations", s.member(s.listReservations))
	mux.Handle("POST /v1/tenants/{id}/reservations", s.member(s.createReservation))
	mux.Handle("POST /v1/tenants/{id}/reservations/{ref}/settle", s.memberReducing(s.settleReservation))
	mux.Handle("POST /v1/tenants/{id}/reservations/{ref}/release", s.memberReducing(s.releaseReservation))
	mux.Handle("POST /v1/tenants/{id}/reservations/{ref}/extend", s.memberReducing(s.extendReservation))
	mux.Handle("POST /v1/staff/tenants/{id}/reservations/{ref}/settle", s.authenticate(http.HandlerFunc(s.staffSettleReservation)))
	mux.Handle("POST /v1/staff/tenants/{id}/reservations/{ref}/release", s.authenticate(http.HandlerFunc(s.staffReleaseReservation)))
	mux.Handle("POST /v1/staff/tenants/{id}/reservations/{ref}/extend", s.authenticate(http.HandlerFunc(s.staffExtendReservation)))
	mux.Handle("POST /v1/tenants/{id}/usage", s.member(s.recordUsage))
	mux.Handle("POST /v1/tenants/{id}/usage/batch", s.member(s.recordUsageBatch))
	mux.Handle("POST /v1/staff/tenants/{id}/usage/rollups/{rollup}/reassign", s.authenticate(http.HandlerFunc(s.reassignUsageRollup)))
	mux.Handle("POST /v1/staff/tenants/{id}/usage/rollups/{rollup}/discard", s.authenticate(http.HandlerFunc(s.discardUsageRollup)))
	mux.Handle("GET /v1/tenants/{id}/usage/events", s.member(s.listUsageEvents))
	mux.Handle("GET /v1/tenants/{id}/usage/summary", s.member(s.getUsageSummary))
	mux.Handle("GET /v1/tenants/{id}/usage/billed", s.member(s.getBilledUsage))
	mux.Handle("POST /v1/tenants/{id}/entitlements/{feature}/consume", s.member(s.consumeEntitlement))
	mux.Handle("POST /v1/tenants/{id}/entitlements/{feature}/release", s.memberReducing(s.releaseEntitlement))
	mux.Handle("POST /v1/tenants/{id}/entitlements/{feature}/extend", s.member(s.extendEntitlement))
	mux.Handle("POST /v1/staff/tenants/{id}/entitlements/{feature}/release", s.authenticate(http.HandlerFunc(s.staffReleaseEntitlement)))
	mux.Handle("GET /v1/brands/{id}/meters", s.authenticate(http.HandlerFunc(s.listMeters)))
	mux.Handle("PUT /v1/brands/{id}/meters", s.authenticate(http.HandlerFunc(s.replaceMeters)))
	mux.Handle("GET /v1/brands/{id}/quotas", s.authenticate(http.HandlerFunc(s.listQuotas)))
	mux.Handle("PUT /v1/brands/{id}/quotas", s.authenticate(http.HandlerFunc(s.replaceQuotas)))
	mux.Handle("POST /v1/staff/tenants/{id}/quota-overrides", s.authenticate(http.HandlerFunc(s.setQuotaOverride)))

	var h http.Handler = s.route(mux)
	h = s.limitBody(h)
	h = s.recoverPanics(h)
	h = securityHeaders(h)
	h = s.requestContext(h)
	return h
}
