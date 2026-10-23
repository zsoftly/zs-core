package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/kbplugin"
	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/pdfrender"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/quota"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

// brandRegistryTTL is how long serve caches the applied brands.
const brandRegistryTTL = 30 * time.Second

// newAPIOptions wires token verification and brand apply for serve.
// ZS_KEYCLOAK_URL is required: without it no token can be verified. The
// admin settings are optional; without them POST /v1/brands/{id}/apply
// answers 503.
func newAPIOptions(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) (httpapi.Options, error) {
	if cfg.KeycloakURL == "" {
		return httpapi.Options{}, errors.New("serve requires ZS_KEYCLOAK_URL to verify tokens")
	}
	registry := brand.NewRegistry(pool, brandRegistryTTL, log)
	resolver := secrets.Default{Dir: cfg.SecretsDir}
	notifier := newNotificationService(pool, resolver, log)
	opts := httpapi.Options{
		DB:                    pool,
		Migrations:            migrations.FS,
		Logger:                log,
		Verifier:              &identity.Verifier{Brands: registry, Keys: keycloak.NewKeySet(cfg.KeycloakURL, keycloak.KeySetOptions{})},
		Brands:                registry,
		BrandDeps:             brand.Deps{Secrets: resolver},
		BrandsDir:             cfg.BrandsDir,
		KeycloakAuditSecrets:  cfg.KeycloakAuditSecrets,
		KeycloakAuditSourceID: cfg.KeycloakAuditSourceID,
	}
	if kc, err := newKeycloak(cfg); err == nil {
		opts.BrandDeps.Keycloak = kc
	}
	if kb, err := newKillBill(cfg); err == nil {
		opts.BrandDeps.KillBill = kb
	}
	opts.Tenants = tenant.Engines{Keycloak: opts.BrandDeps.Keycloak, KillBill: opts.BrandDeps.KillBill, Secrets: resolver}
	applyBillingDeps(&opts.BrandDeps, cfg, log)
	opts.BrandDeps.AfterManifestRecorded = notificationReconciler(notifier)
	if opts.BrandDeps.KillBill != nil {
		opts.Billing = &billing.Service{DB: pool, KillBill: opts.BrandDeps.KillBill, Secrets: resolver,
			WebhookSecret: []byte(cfg.KillBillWebhookSecret), Log: log}
		if cfg.PDFRendererURL != "" {
			renderer, err := pdfrender.New(cfg.PDFRendererURL, nil)
			if err != nil {
				return httpapi.Options{}, err
			}
			opts.Billing.PDF = renderer
		}
	}
	if cfg.PluginMirrorDir != "" && cfg.PluginMirrorSecret != "" {
		pins, err := components.Load(defaultComponentsFile)
		if err != nil {
			return httpapi.Options{}, fmt.Errorf("the plugin mirror needs components.yaml: %w", err)
		}
		opts.PluginMirror = &kbplugin.Mirror{Dir: cfg.PluginMirrorDir, Manifest: pins, Secret: []byte(cfg.PluginMirrorSecret)}
		opts.PluginMirrorSecret = []byte(cfg.PluginMirrorSecret)
		log.Info("plugin mirror served", "dir", cfg.PluginMirrorDir)
	}
	opts.Wallet, opts.Metering, opts.Quota = newExtensions(pool, opts.Billing, cfg, log)
	if cfg.BrandsDir != "" {
		if err := checkBrandsDir(ctx, cfg.BrandsDir, resolver, pool, log); err != nil {
			return httpapi.Options{}, err
		}
	}
	return opts, nil
}

// checkBrandsDir validates every manifest in dir, including its secret
// namespaces, and compares it with what was last applied. An invalid
// manifest stops the start; a manifest that differs from the applied one is
// reported, because apply stays an explicit step.
func checkBrandsDir(ctx context.Context, dir string, refs brand.SecretRefChecker, pool *pgxpool.Pool, log *slog.Logger) error {
	manifests, err := brand.LoadDir(dir)
	if err != nil {
		return fmt.Errorf("ZS_BRANDS_DIR: %w", err)
	}
	for _, m := range manifests {
		if err := m.CheckSecretRefs(refs); err != nil {
			return fmt.Errorf("ZS_BRANDS_DIR: %s: %w", m.Slug, err)
		}
	}
	for _, m := range manifests {
		rec, err := brand.GetBySlug(ctx, pool, m.Slug)
		switch {
		case errors.Is(err, brand.ErrNotFound):
			log.Warn("brand manifest is not applied; its tokens are refused until `zs-core brand apply` runs", "brand", m.Slug)
		case err != nil:
			return fmt.Errorf("check brand %s: %w", m.Slug, err)
		case rec.AppliedAt.IsZero():
			log.Warn("brand apply did not complete; run `zs-core brand apply` again", "brand", m.Slug)
		case rec.ManifestDigest != m.Digest():
			log.Warn("brand manifest differs from the applied one; tokens are verified against the applied manifest until it is applied",
				"brand", m.Slug, "applied_digest", rec.ManifestDigest, "file_digest", m.Digest())
		default:
			log.Info("brand manifest matches the applied one", "brand", m.Slug, "digest", m.Digest())
		}
	}
	return nil
}

// applyBillingDeps gives brand apply the pinned plugins of components.yaml,
// read from the working directory, the plugin mirror and the push
// notification callback settings. Without components.yaml, an apply that
// names a plugin is refused with plugin_unavailable. serve never sets the
// plugin table check: it never reads the Kill Bill database.
func applyBillingDeps(deps *brand.Deps, cfg config.Config, log *slog.Logger) {
	deps.WebhookBaseURL = cfg.KillBillCallbackURL
	deps.WebhookSecret = []byte(cfg.KillBillWebhookSecret)
	m, err := components.Load(defaultComponentsFile)
	if err != nil {
		if log != nil {
			log.Warn("components.yaml not loaded; brand apply refuses payment plugins", "error", err.Error())
		}
		return
	}
	setPluginDeps(deps, m, cfg)
}

// startCreditSweeper runs the credit expiry sweeper every interval until
// ctx ends, when billing is configured.
func startCreditSweeper(ctx context.Context, svc *billing.Service, interval time.Duration, log *slog.Logger) {
	if !svc.Ready() {
		log.Warn("credit sweeper not started: billing needs the Kill Bill admin settings")
		return
	}
	sweeper := &billing.CreditSweeper{Service: svc, Log: log}
	go sweeper.Run(ctx, interval)
	log.Info("credit sweeper started", "interval", interval.String())
}

func newKeycloak(cfg config.Config) (*keycloak.Client, error) {
	return keycloak.New(keycloak.Config{
		URL: cfg.KeycloakURL, AdminUser: cfg.KeycloakAdminUser, AdminPassword: cfg.KeycloakAdminPassword,
		AdminClientID: cfg.KeycloakAdminClientID, AdminClientSecret: cfg.KeycloakAdminClientSecret,
	})
}

func newKillBill(cfg config.Config) (*killbill.Client, error) {
	return killbill.New(killbill.Config{URL: cfg.KillBillURL, AdminUser: cfg.KillBillAdminUser, AdminPassword: cfg.KillBillAdminPassword})
}

// startTenantSweeper runs the tenant sweeper every interval until ctx ends,
// and the relays that mirror membership changes to Keycloak Organizations,
// when the engines are configured. Without them tenants cannot be created
// and there is nothing to sweep or mirror.
func startTenantSweeper(ctx context.Context, pool *pgxpool.Pool, engines tenant.Engines, interval time.Duration, log *slog.Logger) {
	if !engines.Ready() {
		log.Warn("tenant sweeper not started: tenant creation needs the Keycloak and Kill Bill admin settings")
		return
	}
	sweeper := &tenant.Sweeper{DB: pool, Engines: engines, Log: log}
	go sweeper.Run(ctx, interval)
	log.Info("tenant sweeper started", "interval", interval.String())
	go tenant.RunMirror(ctx, pool, engines, log)
	log.Info("keycloak organization mirror started", "workers", tenant.MirrorWorkers)
}

// newExtensions builds the wallet, metering and quota services on the
// billing surface. Without billing, usage is still recorded; the wallet,
// forwarding and quotas need Kill Bill.
func newExtensions(pool *pgxpool.Pool, svc *billing.Service, cfg config.Config, log *slog.Logger) (*wallet.Service, *metering.Service, *quota.Service) {
	m := &metering.Service{DB: pool, Billing: svc, Log: log}
	if !svc.Ready() {
		return nil, m, nil
	}
	w := &wallet.Service{DB: pool, Billing: svc, Metering: m, CreditTTL: cfg.WalletCreditCacheTTL, Log: log}
	q := &quota.Service{DB: pool, Billing: svc, Log: log}
	return w, m, q
}

// startExtensions runs the usage partition sweeper until ctx ends, and,
// when billing is configured, the wallet sweeper, the settlement relay, the
// usage forwarder and the quota lease sweeper. Each relay claims only its
// own event type.
func startExtensions(ctx context.Context, pool *pgxpool.Pool, api httpapi.Options, cfg config.Config, log *slog.Logger) {
	go (&metering.PartitionSweeper{DB: pool, Log: log}).Run(ctx, cfg.UsagePartitionInterval)
	log.Info("usage partition sweeper started", "interval", cfg.UsagePartitionInterval.String())
	if !api.Wallet.Ready() {
		log.Warn("wallet sweeper and usage forwarder not started: they need the Kill Bill admin settings")
		return
	}
	go (&wallet.Sweeper{Service: api.Wallet}).Run(ctx, cfg.WalletSweepInterval)
	log.Info("wallet sweeper started", "interval", cfg.WalletSweepInterval.String())
	go api.Wallet.RunRelay(ctx, outbox.NewPostgresReader(pool).WithEventTypes(wallet.EventSettlementRequested), time.Second)
	log.Info("wallet settlement relay started")
	go api.Metering.Run(ctx, outbox.NewPostgresReader(pool).WithEventTypes(metering.EventUsageRecorded), cfg.MeteringForwardInterval)
	log.Info("usage forwarder started", "interval", cfg.MeteringForwardInterval.String())
	if api.Quota != nil {
		go (&quota.Sweeper{Service: api.Quota}).Run(ctx, cfg.QuotaSweepInterval)
		log.Info("quota lease sweeper started", "interval", cfg.QuotaSweepInterval.String())
	}
}
