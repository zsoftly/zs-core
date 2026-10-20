package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// runCreditCode implements `zs-core credit-code add|list|disable`. The code
// is read from the command line once and only its digest is stored. There
// is no HTTP route that adds or disables codes; this command is the only
// way, and it is audited.
func runCreditCode(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "add" && args[0] != "list" && args[0] != "disable") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	fs := newFlagSet("credit-code "+sub, stderr)
	slug := fs.String("brand", "", "brand slug")
	code := fs.String("code", "", "the code, 4 to 64 letters, digits and dashes")
	amount := fs.Int64("amount-minor", 0, "the credit in minor units, such as cents")
	currency := fs.String("currency", "", "ISO 4217 currency, one of the brand's billing currencies")
	maxRedemptions := fs.Int("max-redemptions", 0, "redemptions across all tenants; 0 means no cap")
	perTenant := fs.Int("per-tenant-limit", 1, "redemptions per tenant")
	expires := fs.String("expires-at", "", "RFC 3339 time after which the code no longer redeems")
	validDays := fs.Int("credit-valid-days", 0, "days after redemption when the unused credit expires; 0 means never")
	id := fs.String("id", "", "credit code ID, ccd_...")
	reason := fs.String("reason", "", "why, recorded in audit")
	operator := fs.String("operator", "", "who acts, recorded in audit")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *slug == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch sub {
	case "add":
		if *code == "" || *amount <= 0 || *currency == "" || strings.TrimSpace(*reason) == "" || strings.TrimSpace(*operator) == "" {
			fmt.Fprintln(stderr, "credit-code add: --code, --amount-minor, --currency, --reason and --operator are required")
			return 2
		}
	case "disable":
		if *id == "" || strings.TrimSpace(*reason) == "" || strings.TrimSpace(*operator) == "" {
			fmt.Fprintln(stderr, "credit-code disable: --id, --reason and --operator are required")
			return 2
		}
	}
	req := billing.NewCreditCode{Code: *code, AmountMinor: *amount, Currency: *currency, PerTenantLimit: *perTenant,
		Reason: *reason, CreatedBy: "zs-core-cli:" + *operator}
	if *maxRedemptions > 0 {
		req.MaxRedemptions = maxRedemptions
	}
	if *validDays > 0 {
		req.CreditValidDays = validDays
	}
	if *expires != "" {
		t, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			fmt.Fprintln(stderr, "credit-code add: --expires-at must be an RFC 3339 time")
			return 2
		}
		req.ExpiresAt = &t
	}
	cfg, err := config.Load(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pool, err := openMigrated(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer pool.Close()
	rec, err := brand.GetBySlug(ctx, pool, *slug)
	if err != nil {
		fmt.Fprintln(stderr, "brand "+*slug+":", err)
		return 1
	}
	if sub == "list" {
		codes, err := billing.ListCreditCodes(ctx, pool, rec.ID)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tHINT\tAMOUNT\tCURRENCY\tREDEEMED\tCAP\tPER TENANT\tEXPIRES\tSTATUS")
		for _, c := range codes {
			limit, exp := "none", "never"
			if c.MaxRedemptions != nil {
				limit = fmt.Sprint(*c.MaxRedemptions)
			}
			if c.ExpiresAt != nil {
				exp = c.ExpiresAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\t%s\t%d\t%s\t%s\n", c.ID, c.Hint, c.AmountMinor, c.Currency, c.Redemptions, limit, c.PerTenantLimit, exp, c.Status)
		}
		if err := tw.Flush(); err != nil {
			return 1
		}
		return 0
	}
	var out billing.CreditCode
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		if sub == "add" {
			out, err = billing.AddCreditCode(ctx, tx, rec.ID, req, operatorActor(*operator), idgen.MustNewV7().String())
		} else {
			out, err = billing.DisableCreditCode(ctx, tx, rec.ID, *id, *reason, operatorActor(*operator), idgen.MustNewV7().String())
		}
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "credit-code %s: %v\n", sub, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s %s in %s is %s\n", out.ID, out.Hint, rec.Slug, out.Status)
	return 0
}

// runCreditSweep implements `zs-core credit sweep`: one pass of the sweeper
// that serve runs every ZS_CREDIT_SWEEP_INTERVAL.
func runCreditSweep(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "sweep" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cfg, err := config.Load(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	kb, err := newKillBill(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "credit sweep needs ZS_KILLBILL_URL and Kill Bill admin credentials: %v\n", err)
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
	report, err := (&billing.CreditSweeper{Service: svc}).SweepOnce(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "credit sweep:", err)
		return 1
	}
	if report.Skipped {
		fmt.Fprintln(stdout, "another sweep is running; nothing done")
		return 0
	}
	fmt.Fprintf(stdout, "expired %d, reversed %d minor units, deferred %d, errors %d, notification receipts deleted %d\n",
		report.Expired, report.ReversedMinor, report.Deferred, report.Errors, report.NotificationsDeleted)
	if report.Errors > 0 {
		return 1
	}
	return 0
}
