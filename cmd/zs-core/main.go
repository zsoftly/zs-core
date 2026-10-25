// Command zs-core runs the ZSoftly Core service.
//
//	zs-core serve          apply pending migrations (unless disabled) and serve HTTP
//	zs-core migrate        apply pending migrations and exit
//	zs-core doctor         check a running stack against components.yaml
//	zs-core upgrade check  list newer upstream versions; changes nothing
//	zs-core brand apply    converge Keycloak and Kill Bill to a brand manifest
//	zs-core brand diff     show what brand apply would change
//	zs-core grant add      give a staff principal a scope in one brand
//	zs-core grant revoke   take a scope away from a staff principal
//	zs-core grant list     list the staff grants of a brand
//	zs-core tenant suspend|reactivate|close  change a tenant's status in Core and the engines
//	zs-core tenant start-closure  start the closure of a tenant and settle its final billing
//	zs-core tenant sweep   run one pass of the tenant sweeper
//	zs-core credit-code add|list|disable  manage the promo and voucher codes of a brand
//	zs-core credit sweep   run one pass of the credit expiry sweeper
//	zs-core wallet sweep   expire reservations past their lease and reconcile wallets once
//	zs-core usage forward  forward recorded usage to Kill Bill once
//	zs-core usage resolve-rollup  confirm that Kill Bill holds an unconfirmed usage rollup
//	zs-core plugin ddl show|apply  print or apply a pinned plugin's DDL
//	zs-core plugin mirror sync     download the pinned plugin jars into the mirror
//
// Configuration comes from the environment only; see docs/configuration.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/migrations"
)

const usage = `Usage: zs-core <command>

Commands:
  serve          apply pending migrations (unless ZS_MIGRATE_ON_START=false) and serve HTTP
  migrate        apply pending migrations and exit
  doctor         check the database, Keycloak, Kill Bill and KAUI against components.yaml
                   --json                 write the report as JSON
                   --allow-skips          exit 0 when a check was skipped (no credentials)
                   --components <file>    components file (default components.yaml)
  upgrade check  list newer upstream versions of the pinned components; changes nothing
                   --strict               exit 1 when a release feed cannot be read
                   --components <file>    components file (default components.yaml)
  brand apply <file>  converge Keycloak and Kill Bill to the manifest and record the brand
                   --operator <name>      who applies, recorded in audit (required)
                   --reason <text>        why, recorded in audit and in Kill Bill
                   --components <file>    approved payment plugins (default components.yaml)
  brand diff <file>   print what brand apply would change; changes nothing
  grant add      give a staff principal a scope in one brand
                   --brand <slug> --subject <sub> --scope operator|activation|billing|support
                   --reason <text> --operator <name> [--issuer <url>]
  grant revoke   revoke a staff principal's active grant of a scope in one brand
                   --brand <slug> --subject <sub> --scope operator|activation|billing|support
                   --reason <text> --operator <name> [--issuer <url>]
  grant list     list the staff grants of a brand
                   --brand <slug>
  tenant suspend|reactivate|close  change a tenant's status in Core, Kill Bill and Keycloak
  tenant start-closure  start a tenant's closure; the sweeper then settles its final billing
                   --brand <slug> --tenant <tnt_...> --reason <text> --operator <name>
  tenant sweep   complete interrupted tenant creations and correct Keycloak Organizations once
  credit-code add      add a promo or voucher code to a brand; only its digest is stored
                   --brand <slug> --code <code> --amount-minor <n> --currency <ISO 4217>
                   --reason <text> --operator <name> [--max-redemptions <n>] [--per-tenant-limit <n>]
                   [--expires-at <RFC 3339>] [--credit-valid-days <n>]
  credit-code list     list the codes of a brand
                   --brand <slug>
  credit-code disable  disable a code; credits already redeemed stay
                   --brand <slug> --id <ccd_...> --reason <text> --operator <name>
  credit sweep   expire the credits Core issued that are past their expiry, once
  wallet sweep   expire reservations past their lease and reconcile every wallet with Kill Bill, once
  usage forward  forward recorded usage to Kill Bill as usage records, once
  usage resolve-rollup  confirm, against a Kill Bill invoice, a rollup whose send was never confirmed
  plugin ddl show <name>   print the DDL of a pinned plugin's jar, read from the mirror or the repository
                   --components <file>    components file (default components.yaml)
  plugin ddl apply <name>|--all  create the plugin's missing tables in the Kill Bill database
                   --operator <name>      who applies, recorded with the DDL (required)
                   --force                apply a DDL that differs from one recorded for the same version
                   --components <file>    components file (default components.yaml)
  plugin mirror sync       download every pinned plugin jar into ZS_PLUGIN_MIRROR_DIR and check it
                   --components <file>    components file (default components.yaml)

Configuration is read from ZS_* environment variables; see docs/configuration.md.
`

func main() {
	os.Exit(run(os.Args[1:], os.Environ(), os.Stdout, os.Stderr))
}

func run(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	command := args[0]
	switch command {
	case "serve", "migrate":
		if len(args) != 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
	case "doctor":
		return runDoctor(args[1:], environ, stdout, stderr)
	case "upgrade":
		return runUpgrade(args[1:], environ, stdout, stderr)
	case "brand":
		return runBrand(args[1:], environ, stdout, stderr)
	case "grant":
		return runGrant(args[1:], environ, stdout, stderr)
	case "tenant":
		return runTenant(args[1:], environ, stdout, stderr)
	case "credit-code":
		return runCreditCode(args[1:], environ, stdout, stderr)
	case "credit":
		return runCreditSweep(args[1:], environ, stdout, stderr)
	case "wallet":
		return runWalletSweep(args[1:], environ, stdout, stderr)
	case "usage":
		return runUsageForward(args[1:], environ, stdout, stderr)
	case "plugin":
		return runPlugin(args[1:], environ, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", command, usage)
		return 2
	}

	cfg, err := config.Load(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	log := newLogger(cfg, stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "migrate":
		err = migrate(ctx, cfg, log)
	case "serve":
		err = serve(ctx, cfg, log)
	}
	if err != nil {
		log.Error("zs-core stopped with an error", "command", command, "error", err.Error())
		return 1
	}
	return 0
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func migrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := database.Open(ctx, database.Options{URL: cfg.DatabaseURL, MaxConns: cfg.DatabaseMaxConns})
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, err := database.Migrate(ctx, pool, migrations.FS)
	if err != nil {
		return err
	}
	log.Info("migrations applied", "count", len(applied), "versions", applied)
	return nil
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	pool, err := database.Open(ctx, database.Options{URL: cfg.DatabaseURL, MaxConns: cfg.DatabaseMaxConns})
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.MigrateOnStart {
		applied, err := database.Migrate(ctx, pool, migrations.FS)
		if err != nil {
			return err
		}
		log.Info("migrations applied", "count", len(applied), "versions", applied)
	}
	api, err := newAPIOptions(ctx, cfg, pool, log)
	if err != nil {
		return err
	}
	if err := startNotifications(ctx, pool, api.BrandDeps.Secrets, log); err != nil {
		return err
	}
	startTenantSweeper(ctx, pool, api.Tenants, api.Metering, cfg.TenantSweepInterval, log)
	startCreditSweeper(ctx, api.Billing, cfg.CreditSweepInterval, log)
	startExtensions(ctx, pool, api, cfg, log)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpapi.New(api),
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}
	log.Info("zs-core listening", "addr", listener.Addr().String())

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	log.Info("zs-core shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
