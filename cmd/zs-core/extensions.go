package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

// extensionCommand loads the configuration, the migrated database and the
// billing service for a one-pass command, and runs fn.
func extensionCommand(name string, environ []string, stdout, stderr io.Writer, fn func(context.Context, *billing.Service, *pgxpool.Pool) (string, bool, error)) int {
	cfg, err := config.Load(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	kb, err := newKillBill(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s needs ZS_KILLBILL_URL and Kill Bill admin credentials: %v\n", name, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	pool, err := openMigrated(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer pool.Close()
	svc := &billing.Service{DB: pool, KillBill: kb, Secrets: secrets.Default{LookupEnv: envLookup(environ), Dir: cfg.SecretsDir}}
	out, failed, err := fn(ctx, svc, pool)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return 1
	}
	fmt.Fprintln(stdout, out)
	if failed {
		return 1
	}
	return 0
}

// runWalletSweep implements `zs-core wallet sweep`: one pass of the sweeper
// that serve runs every ZS_WALLET_SWEEP_INTERVAL. It expires leases,
// completes the settlements that wait for Kill Bill, and reconciles every
// tenant, whenever it was last reconciled.
func runWalletSweep(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "sweep" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	return extensionCommand("wallet sweep", environ, stdout, stderr, func(ctx context.Context, svc *billing.Service, pool *pgxpool.Pool) (string, bool, error) {
		w := &wallet.Service{DB: pool, Billing: svc}
		report, err := (&wallet.Sweeper{Service: w, ReconcileEvery: time.Nanosecond}).SweepOnce(ctx)
		if err != nil {
			return "", false, err
		}
		return fmt.Sprintf("expired %d, settled %d, still pending %d, settlements refused %d, newly stale %d, reconciled %d, new mismatches %d, errors %d",
			report.Expired, report.Settled, report.Pending, report.Failed, report.Stale, report.Reconciled, report.Mismatches, report.Errors), report.Errors > 0, nil
	})
}

// runUsageForward implements `zs-core usage forward`: one pass of the
// forwarder that serve runs every ZS_METERING_FORWARD_INTERVAL.
func runUsageForward(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "forward" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	return extensionCommand("usage forward", environ, stdout, stderr, func(ctx context.Context, svc *billing.Service, pool *pgxpool.Pool) (string, bool, error) {
		m := &metering.Service{DB: pool, Billing: svc}
		report, err := m.Forward(ctx)
		if err != nil {
			return "", false, err
		}
		return fmt.Sprintf("rolled up %d, forwarded %d (%d already in Kill Bill), failed %d, rejected %d, backlog alerts %d",
			report.RolledUp, report.Forwarded, report.Duplicates, report.Failed, report.Rejected, report.Backlogs), report.Failed > 0, nil
	})
}
