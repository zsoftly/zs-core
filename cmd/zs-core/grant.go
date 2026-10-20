package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/upstream"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// runGrant implements `zs-core grant add|revoke|list`. There is no HTTP route
// that creates or revokes grants; this command is the only way, and it is
// audited.
func runGrant(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "add" && args[0] != "revoke" && args[0] != "list") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	fs := newFlagSet("grant "+sub, stderr)
	slug := fs.String("brand", "", "brand slug")
	subject := fs.String("subject", "", "the staff user's subject (Keycloak user ID)")
	scopeFlag := fs.String("scope", "", "operator, activation, billing or support")
	reason := fs.String("reason", "", "why the grant is given or revoked")
	operator := fs.String("operator", "", "who gives or revokes the grant")
	issuer := fs.String("issuer", "", "the realm issuer; read from Keycloak discovery when omitted")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *slug == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if sub != "list" && (*subject == "" || *scopeFlag == "" || strings.TrimSpace(*reason) == "" || strings.TrimSpace(*operator) == "") {
		fmt.Fprintf(stderr, "grant %s: --subject, --scope, --reason and --operator are required\n", sub)
		return 2
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
		grants, err := identity.ListStaffGrants(ctx, pool, rec.ID)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSCOPE\tSTATUS\tSUBJECT\tISSUER\tGRANTED BY\tCREATED")
		for _, g := range grants {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", g.PublicID(), g.Scope, g.Status, g.Subject, g.Issuer, g.GrantedBy, g.CreatedAt.UTC().Format(time.RFC3339))
		}
		if err := tw.Flush(); err != nil {
			return 1
		}
		return 0
	}

	iss := *issuer
	if iss == "" {
		if cfg.KeycloakURL == "" {
			fmt.Fprintf(stderr, "grant %s: pass --issuer or set ZS_KEYCLOAK_URL to read it from discovery\n", sub)
			return 2
		}
		iss, err = discoverIssuer(ctx, cfg.KeycloakURL, rec.KeycloakRealm)
		if err != nil {
			fmt.Fprintf(stderr, "grant %s: %v\n", sub, err)
			return 1
		}
	}
	// A grant whose issuer is not the brand's realm could never match a
	// token of the brand; refuse it instead of storing a dead row.
	if !strings.HasSuffix(iss, "/realms/"+rec.KeycloakRealm) {
		fmt.Fprintf(stderr, "grant %s: issuer must be the issuer of realm %s\n", sub, rec.KeycloakRealm)
		return 2
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	g := identity.StaffGrant{
		BrandID: rec.ID, Issuer: iss, Subject: *subject, Scope: identity.StaffScope(*scopeFlag),
		GrantedBy: *operator, Reason: *reason,
	}
	verb := "granted"
	if sub == "revoke" {
		g, err = identity.RevokeStaffGrant(ctx, tx, g, *operator, operatorActor(*operator), idgen.MustNewV7().String())
		verb = "revoked"
	} else {
		g, err = identity.AddStaffGrant(ctx, tx, g, operatorActor(*operator), idgen.MustNewV7().String())
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "grant %s: %v\n", sub, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s %s %s %s in %s\n", g.PublicID(), verb, g.Scope, g.Subject, rec.Slug)
	return 0
}

func discoverIssuer(ctx context.Context, base, realm string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/realms/"+realm+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := upstream.Do(upstream.NewClient(), req)
	if err != nil {
		return "", fmt.Errorf("read discovery: %w", err)
	}
	if !resp.OK() {
		return "", fmt.Errorf("read discovery: HTTP %d", resp.Status)
	}
	var d struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(resp.Body, &d); err != nil || d.Issuer == "" {
		return "", errors.New("read discovery: no issuer")
	}
	return d.Issuer, nil
}
