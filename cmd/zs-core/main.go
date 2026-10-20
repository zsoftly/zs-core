// Command zs-core runs the ZSoftly Core service.
//
//	zs-core serve          apply pending migrations (unless disabled) and serve HTTP
//	zs-core migrate        apply pending migrations and exit
//	zs-core doctor         check a running stack against components.yaml
//	zs-core upgrade check  list newer upstream versions; changes nothing
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
                   --components <file>    components file (default components.yaml)
  upgrade check  list newer upstream versions of the pinned components; changes nothing
                   --strict               exit 1 when a release feed cannot be read
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
	if cfg.DevTrustedScopeHeaders {
		log.Warn("trusted scope headers are enabled: X-ZS-Brand and X-ZS-Tenant are accepted without authentication; development only",
			"listen_addr", cfg.ListenAddr)
	}

	server := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: httpapi.New(httpapi.Options{
			DB:                  pool,
			Migrations:          migrations.FS,
			Logger:              log,
			TrustedScopeHeaders: cfg.DevTrustedScopeHeaders,
		}),
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
