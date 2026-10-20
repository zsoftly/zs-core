package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/config"
	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/upgrade"
	"github.com/zsoftly/zs-core/migrations"
)

const defaultComponentsFile = "components.yaml"

// upgradeFeeds lets tests point `upgrade check` at local servers. Empty means
// the real feeds.
var upgradeFeeds struct{ github, dockerHub string }

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// runDoctor implements `zs-core doctor`. Exit 0 when nothing failed, 1 when a
// finding failed, 2 for a usage or configuration error.
func runDoctor(args, environ []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	asJSON := fs.Bool("json", false, "write the report as JSON")
	componentsFile := fs.String("components", defaultComponentsFile, "path to components.yaml")
	timeout := fs.Duration("timeout", 60*time.Second, "overall time limit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cfg, err := config.Load(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	manifest, err := components.Load(*componentsFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	var db doctor.Database
	pool, err := database.Open(ctx, database.Options{URL: cfg.DatabaseURL, MaxConns: 2})
	if err != nil {
		db = failedDatabase{err: err}
	} else {
		defer pool.Close()
		db = doctor.PGDatabase{DB: pool, Migrations: migrations.FS}
	}

	report := doctor.Run(ctx, doctor.Options{
		Manifest: manifest,
		Database: db,
		Keycloak: doctor.Target{URL: cfg.KeycloakURL, AdminUser: cfg.KeycloakAdminUser, AdminPassword: cfg.KeycloakAdminPassword, Realm: cfg.KeycloakRealm},
		KillBill: doctor.Target{URL: cfg.KillBillURL, AdminUser: cfg.KillBillAdminUser, AdminPassword: cfg.KillBillAdminPassword},
		KAUI:     doctor.Target{URL: cfg.KAUIURL},
	})
	if *asJSON {
		err = report.WriteJSON(stdout)
	} else {
		err = report.WriteText(stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if report.Failed() {
		return 1
	}
	return 0
}

// failedDatabase reports a connection error as a doctor finding instead of
// aborting the run.
type failedDatabase struct{ err error }

func (f failedDatabase) Ping(context.Context) error { return f.err }
func (f failedDatabase) Pending(context.Context) ([]int64, error) {
	return nil, f.err
}
func (f failedDatabase) ServerVersion(context.Context) (string, error) { return "", f.err }
func (f failedDatabase) Now(context.Context) (time.Time, error)        { return time.Time{}, f.err }

// runUpgrade implements `zs-core upgrade check`. It never changes anything.
func runUpgrade(args, environ []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := newFlagSet("upgrade check", stderr)
	strict := fs.Bool("strict", false, "exit 1 when a release feed cannot be read")
	componentsFile := fs.String("components", defaultComponentsFile, "path to components.yaml")
	timeout := fs.Duration("timeout", 60*time.Second, "overall time limit")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	manifest, err := components.Load(*componentsFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	results := upgrade.Check(ctx, upgrade.Options{
		Manifest:     manifest,
		Client:       &http.Client{Timeout: 20 * time.Second},
		GitHubAPI:    upgradeFeeds.github,
		DockerHubAPI: upgradeFeeds.dockerHub,
		GitHubToken:  lookupEnv(environ, "GITHUB_TOKEN"),
	})
	failed := false
	for _, r := range results {
		if r.Err != nil {
			failed = true
			fmt.Fprintf(stderr, "warning: %s: %s\n", r.Component, r.Err)
			continue
		}
		fmt.Fprintln(stdout, formatUpgrade(r))
	}
	if failed && *strict {
		return 1
	}
	return 0
}

func formatUpgrade(r upgrade.Result) string {
	if len(r.Newer) == 0 {
		return fmt.Sprintf("%s: pinned %s, up to date", r.Component, r.Pinned)
	}
	parts := make([]string, 0, len(r.Newer))
	for _, n := range r.Newer {
		date := "date unknown"
		if !n.Published.IsZero() {
			date = n.Published.UTC().Format("2006-01-02")
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", n.Version, upgrade.Kind(r.Pinned, n.Version), date))
	}
	return fmt.Sprintf("%s: pinned %s, newer: %s", r.Component, r.Pinned, strings.Join(parts, ", "))
}

func lookupEnv(environ []string, key string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
