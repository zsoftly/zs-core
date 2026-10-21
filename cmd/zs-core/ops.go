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
	"github.com/zsoftly/zs-core/internal/kbplugin"
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

// runDoctor implements `zs-core doctor`. Exit 0 when every pin was checked and
// nothing failed, 1 when a finding failed or a check was skipped (unless
// --allow-skips), 2 for a usage or configuration error.
func runDoctor(args, environ []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	asJSON := fs.Bool("json", false, "write the report as JSON")
	allowSkips := fs.Bool("allow-skips", false, "exit 0 even when a check was skipped for lack of credentials or settings")
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

	opts := doctor.Options{
		Manifest:       manifest,
		Database:       db,
		Keycloak:       doctor.Target{URL: cfg.KeycloakURL, AdminUser: cfg.KeycloakAdminUser, AdminPassword: cfg.KeycloakAdminPassword, Realm: cfg.KeycloakRealm},
		KillBill:       doctor.Target{URL: cfg.KillBillURL, AdminUser: cfg.KillBillAdminUser, AdminPassword: cfg.KillBillAdminPassword},
		KAUI:           doctor.Target{URL: cfg.KAUIURL},
		Mirror:         doctor.MirrorOptions{Dir: cfg.PluginMirrorDir, URL: cfg.PluginMirrorURL, Secret: []byte(cfg.PluginMirrorSecret)},
		PDFRendererURL: cfg.PDFRendererURL,
	}
	if cfg.KillBillDatabaseURL != "" {
		kbPool, err := database.Open(ctx, database.Options{URL: cfg.KillBillDatabaseURL, MaxConns: 2})
		if err != nil {
			opts.PluginTables = func(context.Context, components.Plugin) ([]string, error) { return nil, err }
		} else {
			defer kbPool.Close()
			opts.PluginTables = tablesIn(kbPool)
		}
	}
	report := doctor.Run(ctx, opts)
	if *asJSON {
		err = report.WriteJSON(stdout)
	} else {
		err = report.WriteText(stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return doctorExitCode(report, *allowSkips)
}

// doctorExitCode is 1 when a finding failed, or when a check was skipped and
// skips are not allowed, so a green doctor means every pin was checked.
func doctorExitCode(report doctor.Report, allowSkips bool) int {
	if report.Failed() || (report.Skipped() && !allowSkips) {
		return 1
	}
	return 0
}

// tablesIn returns the check of a plugin's tables in the Kill Bill database
// that doctor and brand apply run.
func tablesIn(kb kbplugin.Querier) func(context.Context, components.Plugin) ([]string, error) {
	return func(ctx context.Context, pin components.Plugin) ([]string, error) {
		return kbplugin.MissingTables(ctx, kb, pin)
	}
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
