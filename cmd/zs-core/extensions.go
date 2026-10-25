package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/scope"
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
	if len(args) > 0 && args[0] == "resolve-rollup" {
		return runUsageResolve(args[1:], environ, stdout, stderr)
	}
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

// runUsageResolve implements `zs-core usage resolve-rollup`: staff confirm that
// Kill Bill holds a usage rollup whose send it never confirmed. The tracking ID
// of the rollup has to be on an invoice of the tenant's account, which the
// command reads, and nothing is sent again. It is audited and idempotent.
func runUsageResolve(args, environ []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("usage resolve-rollup", stderr)
	slug := fs.String("brand", "", "brand slug")
	tenantID := fs.String("tenant", "", "tenant ID, tnt_...")
	rollup := fs.String("rollup", "", "rollup ID, rol_...")
	reason := fs.String("reason", "", "why, recorded in audit")
	operator := fs.String("operator", "", "who acts, recorded in audit")
	decision := fs.String("decision", "forwarded", "forwarded: a committed invoice carries the record; not_billed: Kill Bill does not hold it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *slug == "" || *tenantID == "" || *rollup == "" || strings.TrimSpace(*reason) == "" || strings.TrimSpace(*operator) == "" {
		fmt.Fprintln(stderr, "usage resolve-rollup: --brand, --tenant, --rollup, --reason and --operator are required")
		return 2
	}
	id, err := idgen.ParseAs(idgen.Tenant, *tenantID)
	if err != nil {
		fmt.Fprintln(stderr, "usage resolve-rollup: --tenant must be a tnt_ ID")
		return 2
	}
	return extensionCommand("usage resolve-rollup", environ, stdout, stderr, func(ctx context.Context, svc *billing.Service, pool *pgxpool.Pool) (string, bool, error) {
		rec, err := brand.GetBySlug(ctx, pool, *slug)
		if err != nil {
			return "", false, fmt.Errorf("brand %s: %w", *slug, err)
		}
		m := &metering.Service{DB: pool, Billing: svc}
		call := metering.StaffCall{Call: metering.Call{Actor: operatorActor(*operator), CorrelationID: idgen.MustNewV7().String()}, Reason: *reason}
		res, replayed, err := m.ResolveUnconfirmedRollup(ctx, scope.Scope{Brand: rec.ID, Tenant: id}, call, *rollup, *decision)
		if err != nil {
			return "", false, err
		}
		if replayed {
			return fmt.Sprintf("%s was already confirmed as %s by %s, invoice %s", res.RollupID, res.Decision, res.ResolvedBy, res.InvoiceID), false, nil
		}
		if res.Decision == metering.ResolveDecisionNotBilled {
			return fmt.Sprintf("%s recorded as not billed, %d micro-units lost", res.RollupID, res.UnbilledQuantityMicro), false, nil
		}
		return fmt.Sprintf("%s confirmed as %s, invoice %s", res.RollupID, res.Decision, res.InvoiceID), false, nil
	})
}
