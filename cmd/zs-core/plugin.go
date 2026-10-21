package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/zsoftly/zs-core/internal/kbplugin"
)

// runPlugin implements `zs-core plugin ddl show|apply` and `zs-core plugin
// mirror sync`. Exit 0 on success, 1 when the step failed, 2 for a usage or
// configuration error.
func runPlugin(args, environ []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) >= 2 && args[0] == "ddl" && args[1] == "show":
		return runPluginDDLShow(args[2:], environ, stdout, stderr)
	case len(args) >= 2 && args[0] == "ddl" && args[1] == "apply":
		return runPluginDDLApply(args[2:], environ, stdout, stderr)
	case len(args) >= 2 && args[0] == "mirror" && args[1] == "sync":
		return runPluginMirrorSync(args[2:], environ, stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// mirrorDirFromEnv reads ZS_PLUGIN_MIRROR_DIR for the commands that need
// no database. It must be an absolute path, as configuration requires.
func mirrorDirFromEnv(environ []string) (string, error) {
	dir := lookupEnv(environ, config.EnvPluginMirrorDir)
	if dir != "" && !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s must be an absolute directory path", config.EnvPluginMirrorDir)
	}
	return dir, nil
}

// pluginPins returns the pinned plugins a command acts on: the named one,
// or every plugin with a Java artifact for --all.
func pluginPins(m components.Manifest, names []string, all bool) ([]components.Plugin, error) {
	if all {
		if len(names) > 0 {
			return nil, errors.New("name plugins or pass --all, not both")
		}
		var out []components.Plugin
		for _, p := range m.Artifacts() {
			if len(p.Tables) > 0 {
				out = append(out, p)
			}
		}
		return out, nil
	}
	if len(names) != 1 {
		return nil, errors.New("name one plugin, or pass --all")
	}
	p, ok := m.PinnedPlugin(names[0])
	if !ok {
		return nil, fmt.Errorf("plugin %s is not pinned in components.yaml", names[0])
	}
	if !p.HasArtifact() {
		return nil, fmt.Errorf("plugin %s has no Java artifact pinned in components.yaml", names[0])
	}
	return []components.Plugin{p}, nil
}

func signalContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, func() { cancel(); stop() }
}

func runPluginDDLShow(args, environ []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plugin ddl show", stderr)
	componentsFile := fs.String("components", defaultComponentsFile, "path to components.yaml")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall time limit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	m, err := components.Load(*componentsFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	pins, err := pluginPins(m, fs.Args(), false)
	if err != nil {
		fmt.Fprintln(stderr, "plugin ddl show:", err)
		return 2
	}
	dir, err := mirrorDirFromEnv(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, cancel := signalContext(*timeout)
	defer cancel()
	src := kbplugin.Source{MirrorDir: dir, Repository: m.KillBillPlugins.Repository}
	jar, from, err := src.Jar(ctx, pins[0])
	if err != nil {
		fmt.Fprintln(stderr, "plugin ddl show:", err)
		return 1
	}
	d, err := kbplugin.ExtractDDL(jar, pins[0])
	if err != nil {
		fmt.Fprintln(stderr, "plugin ddl show:", err)
		return 1
	}
	fmt.Fprintf(stderr, "plugin %s %s from %s, jar sha256 %s matches components.yaml, ddl sha256 %s\n", pins[0].Name, pins[0].Version, from, pins[0].Sha256, d.Digest)
	if _, err := io.WriteString(stdout, d.Text); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runPluginDDLApply(args, environ []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plugin ddl apply", stderr)
	operator := fs.String("operator", "", "who applies, recorded with the DDL")
	force := fs.Bool("force", false, "apply a DDL whose digest differs from one recorded for the same plugin version")
	all := fs.Bool("all", false, "every pinned plugin with tables")
	componentsFile := fs.String("components", defaultComponentsFile, "path to components.yaml")
	timeout := fs.Duration("timeout", 15*time.Minute, "overall time limit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*operator) == "" {
		fmt.Fprintln(stderr, "plugin ddl apply: --operator is required")
		return 2
	}
	m, err := components.Load(*componentsFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	pins, err := pluginPins(m, fs.Args(), *all)
	if err != nil {
		fmt.Fprintln(stderr, "plugin ddl apply:", err)
		return 2
	}
	cfg, err := config.Load(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if cfg.KillBillDatabaseURL == "" {
		fmt.Fprintln(stderr, "plugin ddl apply needs ZS_KILLBILL_DATABASE_URL, the Kill Bill database")
		return 2
	}
	ctx, cancel := signalContext(*timeout)
	defer cancel()
	core, err := openMigrated(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer core.Close()
	kb, err := database.Open(ctx, database.Options{URL: cfg.KillBillDatabaseURL, MaxConns: 2})
	if err != nil {
		fmt.Fprintln(stderr, "open the Kill Bill database:", err)
		return 1
	}
	defer kb.Close()
	src := kbplugin.Source{MirrorDir: cfg.PluginMirrorDir, Repository: m.KillBillPlugins.Repository}
	enc := json.NewEncoder(stdout)
	failed := false
	for _, pin := range pins {
		res, err := applyPluginDDL(ctx, src, kb, core, pin, kbplugin.ApplyOptions{AppliedBy: "zs-core-cli:" + *operator, Force: *force})
		if err != nil {
			fmt.Fprintf(stderr, "plugin ddl apply %s: %v\n", pin.Name, err)
			failed = true
			continue
		}
		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	if failed {
		return 1
	}
	return 0
}

func applyPluginDDL(ctx context.Context, src kbplugin.Source, kb, core *pgxpool.Pool, pin components.Plugin, opts kbplugin.ApplyOptions) (kbplugin.Result, error) {
	jar, _, err := src.Jar(ctx, pin)
	if err != nil {
		return kbplugin.Result{}, err
	}
	d, err := kbplugin.ExtractDDL(jar, pin)
	if err != nil {
		return kbplugin.Result{}, err
	}
	return kbplugin.Apply(ctx, kb, core, d, pin, opts)
}

func runPluginMirrorSync(args, environ []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("plugin mirror sync", stderr)
	componentsFile := fs.String("components", defaultComponentsFile, "path to components.yaml")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall time limit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	m, err := components.Load(*componentsFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	dir, err := mirrorDirFromEnv(environ)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if dir == "" {
		fmt.Fprintln(stderr, "plugin mirror sync needs ZS_PLUGIN_MIRROR_DIR")
		return 2
	}
	secret := lookupEnv(environ, config.EnvPluginMirrorSecret)
	if len(secret) < 32 {
		fmt.Fprintln(stderr, "plugin mirror sync needs ZS_PLUGIN_MIRROR_SECRET of at least 32 characters: it signs the mirror manifest")
		return 2
	}
	ctx, cancel := signalContext(*timeout)
	defer cancel()
	results, err := kbplugin.Sync(ctx, dir, []byte(secret), m, nil)
	for _, r := range results {
		state := "kept"
		if r.Downloaded {
			state = "downloaded"
		}
		fmt.Fprintf(stdout, "%s %s %s sha256 %s (%d bytes)\n", r.Entry.Name, r.Entry.Version, state, r.Entry.Sha256, r.Entry.Size)
	}
	if err != nil {
		fmt.Fprintln(stderr, "plugin mirror sync:", err)
		return 1
	}
	fmt.Fprintf(stdout, "mirror %s holds %d jars; manifest %s\n", dir, len(results), filepath.Join(dir, kbplugin.ManifestFile))
	return 0
}
