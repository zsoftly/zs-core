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

	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// runTenant implements `zs-core tenant suspend|reactivate|close|sweep`. No
// HTTP route suspends, reactivates or closes a tenant in v1; these commands
// do, and they are audited. sweep runs one pass of the sweeper that serve
// runs on a timer.
func runTenant(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	switch sub {
	case "suspend", "reactivate", "close", "sweep":
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := newFlagSet("tenant "+sub, stderr)
	slug := fs.String("brand", "", "brand slug")
	tenantID := fs.String("tenant", "", "tenant ID, tnt_...")
	reason := fs.String("reason", "", "why, recorded in audit and in Kill Bill")
	operator := fs.String("operator", "", "who acts, recorded in audit")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if sub != "sweep" && (*slug == "" || *tenantID == "" || strings.TrimSpace(*reason) == "" || strings.TrimSpace(*operator) == "") {
		fmt.Fprintf(stderr, "tenant %s: --brand, --tenant, --reason and --operator are required\n", sub)
		return 2
	}
	cfg, err := config.Load(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	kc, err := newKeycloak(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "tenant %s needs ZS_KEYCLOAK_URL and Keycloak admin credentials: %v\n", sub, err)
		return 2
	}
	kb, err := newKillBill(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "tenant %s needs ZS_KILLBILL_URL and Kill Bill admin credentials: %v\n", sub, err)
		return 2
	}
	engines := tenant.Engines{Keycloak: kc, KillBill: kb, Secrets: secrets.Default{Dir: cfg.SecretsDir}}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	pool, err := openMigrated(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer pool.Close()

	if sub == "sweep" {
		report, err := (&tenant.Sweeper{DB: pool, Engines: engines}).SweepOnce(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "tenant sweep:", err)
			return 1
		}
		if report.Skipped {
			fmt.Fprintln(stdout, "another sweep is running; nothing done")
			return 0
		}
		fmt.Fprintf(stdout, "completed %d, still pending %d, organizations checked %d, updated %d, members added %d, removed %d, recipients checked %d, updated %d, deferred %d, errors %d\n",
			report.Completed, report.Failed, report.OrganizationsChecked, report.OrganizationsUpdated,
			report.MembersAdded, report.MembersRemoved, report.RecipientsChecked, report.RecipientsUpdated, report.RecipientsDeferred, report.Errors)
		if report.Errors > 0 {
			return 1
		}
		return 0
	}

	rec, err := brand.GetBySlug(ctx, pool, *slug)
	if err != nil {
		fmt.Fprintln(stderr, "brand "+*slug+":", err)
		return 1
	}
	id, err := idgen.ParseAs(idgen.Tenant, *tenantID)
	if err != nil {
		fmt.Fprintf(stderr, "tenant %s: --tenant must be a tnt_ ID\n", sub)
		return 2
	}
	sc := scope.Scope{Brand: rec.ID, Tenant: id}
	op := map[string]func(context.Context, tenant.DB, tenant.Engines, scope.Scope, events.Actor, string, string) (tenant.Tenant, error){
		"suspend": tenant.Suspend, "reactivate": tenant.Reactivate, "close": tenant.Close,
	}[sub]
	t, err := op(ctx, pool, engines, sc, operatorActor(*operator), *reason, idgen.MustNewV7().String())
	if err != nil {
		fmt.Fprintf(stderr, "tenant %s: %v\n", sub, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s %s in %s is %s\n", t.PublicID(), t.Name, rec.Slug, t.Status)
	return 0
}
