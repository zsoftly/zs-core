package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
)

// operatorActor is the audit actor of an operator command.
func operatorActor(name string) events.Actor {
	return events.Actor{Type: events.ActorService, ID: "zs-core-cli:" + name}
}

// openMigrated opens the database and refuses to continue while migrations
// are pending, so an operator command never runs against an old schema.
func openMigrated(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := database.Open(ctx, database.Options{URL: cfg.DatabaseURL, MaxConns: 4})
	if err != nil {
		return nil, err
	}
	pending, err := database.Pending(ctx, pool, migrations.FS)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if len(pending) > 0 {
		pool.Close()
		return nil, fmt.Errorf("%d migrations are pending; run zs-core migrate first", len(pending))
	}
	return pool, nil
}

// runBrand implements `zs-core brand apply|diff <file>`. Exit 0 on success,
// 1 when the apply or the diff failed, 2 for a usage or configuration error.
func runBrand(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "apply" && args[0] != "diff") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	fs := newFlagSet("brand "+sub, stderr)
	operator := fs.String("operator", "", "who applies, recorded in audit")
	reason := fs.String("reason", "", "why, recorded in audit and in Kill Bill")
	timeout := fs.Duration("timeout", 15*time.Minute, "overall time limit")
	componentsFile := fs.String("components", defaultComponentsFile, "path to components.yaml, for the approved payment plugins")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if sub == "apply" && strings.TrimSpace(*operator) == "" {
		fmt.Fprintln(stderr, "brand apply: --operator is required")
		return 2
	}
	file := fs.Arg(0)
	cfg, err := config.Load(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	kc, err := newKeycloak(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "brand "+sub+" needs ZS_KEYCLOAK_URL and Keycloak admin credentials:", err)
		return 2
	}
	kb, err := newKillBill(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "brand "+sub+" needs ZS_KILLBILL_URL, ZS_KILLBILL_ADMIN_USER and ZS_KILLBILL_ADMIN_PASSWORD:", err)
		return 2
	}
	m, err := brand.Load(file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if m.Identity.SSLRequired == "none" {
		fmt.Fprintln(stderr, "warning: identity.ssl_required is none; Keycloak accepts plain http from any address. Use it only for the loopback reference stack. POST /v1/brands/{id}/apply refuses it.")
	}
	var pins *components.Manifest
	if len(m.Billing.Plugins()) > 0 || m.Billing.InvoicePlugin != nil || m.Billing.Email != nil {
		loaded, err := components.Load(*componentsFile)
		if err != nil {
			fmt.Fprintln(stderr, "brand "+sub+": the manifest names a plugin, which needs components.yaml:", err)
			return 2
		}
		if err := brand.CheckPluginRepository(loaded.KillBillPlugins.Repository); err != nil {
			fmt.Fprintln(stderr, "brand "+sub+": "+*componentsFile+":", err)
			return 2
		}
		pins = &loaded
	}
	root, err := os.OpenRoot(filepath.Dir(file))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer root.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	pool, err := openMigrated(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer pool.Close()

	resolver := secrets.Default{LookupEnv: envLookup(environ), Dir: cfg.SecretsDir}
	deps := brand.Deps{DB: pool, Keycloak: kc, KillBill: kb, Secrets: resolver, Files: root.FS(),
		WebhookBaseURL: cfg.KillBillCallbackURL, WebhookSecret: []byte(cfg.KillBillWebhookSecret)}
	if pins != nil {
		setPluginDeps(&deps, *pins, cfg)
	}
	// The command line checks the plugin tables as doctor does when it can
	// read the Kill Bill database. serve never reads it.
	if cfg.KillBillDatabaseURL != "" {
		kbPool, err := database.Open(ctx, database.Options{URL: cfg.KillBillDatabaseURL, MaxConns: 2})
		if err != nil {
			fmt.Fprintln(stderr, "open the Kill Bill database of ZS_KILLBILL_DATABASE_URL:", err)
			return 1
		}
		defer kbPool.Close()
		deps.PluginTables = tablesIn(kbPool)
	}
	opts := brand.Options{Actor: operatorActor(*operator), Reason: *reason, CreatedBy: "zs-core-cli:" + *operator}
	var diff brand.Diff
	if sub == "apply" {
		diff, err = brand.Apply(ctx, deps, m, opts)
	} else {
		diff, err = brand.Plan(ctx, deps, m, opts)
	}
	if len(diff.Changes) > 0 || err == nil {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(diff); encErr != nil {
			fmt.Fprintln(stderr, encErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "brand "+sub+" failed:", err)
		return 1
	}
	return 0
}

// setPluginDeps gives brand apply the pinned plugins and mirror settings.
// New installs require the mirror URL and secret; starts need no download.
func setPluginDeps(deps *brand.Deps, pins components.Manifest, cfg config.Config) {
	deps.PaymentPlugins = pins.KillBillPlugins.ApprovedPaymentPlugins
	deps.InvoicePlugins = pins.KillBillPlugins.ApprovedInvoicePlugins
	deps.InstallablePlugins = pins.KillBillPlugins.Installable
	deps.PluginRepository = pins.KillBillPlugins.Repository
	if cfg.PluginMirrorURL != "" {
		deps.PluginMirrorURL, deps.PluginMirrorSecret = cfg.PluginMirrorURL, []byte(cfg.PluginMirrorSecret)
	}
}

// envLookup resolves env:// references against the command's environment.
func envLookup(environ []string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		for _, kv := range environ {
			if k, v, ok := strings.Cut(kv, "="); ok && k == name {
				return v, true
			}
		}
		return "", false
	}
}
