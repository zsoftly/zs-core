package doctor

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/database"
)

// Database is what doctor needs from Core's database.
type Database interface {
	Ping(ctx context.Context) error
	// Pending returns the versions of migrations that are not applied.
	Pending(ctx context.Context) ([]int64, error)
	// ServerVersion returns the server's version string, such as "18.0".
	ServerVersion(ctx context.Context) (string, error)
	// Now returns the database server's current time.
	Now(ctx context.Context) (time.Time, error)
}

// Queryer is the part of pgxpool.Pool that PGDatabase uses.
type Queryer interface {
	Ping(ctx context.Context) error
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGDatabase implements Database over a Postgres connection pool.
type PGDatabase struct {
	DB         Queryer
	Migrations fs.FS
}

// Ping implements Database.
func (p PGDatabase) Ping(ctx context.Context) error { return p.DB.Ping(ctx) }

// Pending implements Database.
func (p PGDatabase) Pending(ctx context.Context) ([]int64, error) {
	return database.Pending(ctx, p.DB, p.Migrations)
}

// ServerVersion implements Database.
func (p PGDatabase) ServerVersion(ctx context.Context) (string, error) {
	var v string
	err := p.DB.QueryRow(ctx, `SHOW server_version`).Scan(&v)
	return v, err
}

// Now implements Database.
func (p PGDatabase) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := p.DB.QueryRow(ctx, `SELECT now()`).Scan(&t)
	return t, err
}

// checkCore covers Core's own database: reachable, migrations, server version
// against the Postgres pin, and clock skew.
func checkCore(ctx context.Context, opts Options) ([]ComponentReport, []Finding) {
	report := ComponentReport{Name: "core", Expected: opts.Manifest.CoreRelease}
	pg := ComponentReport{Name: components.Postgres}
	var findings []Finding
	add := func(component, check string, status Status, format string, args ...any) {
		findings = append(findings, Finding{component, check, status, fmt.Sprintf(format, args...)})
	}

	if opts.Database == nil {
		add("core", "database", Fail, "no database configured")
		return []ComponentReport{report}, findings
	}
	if err := opts.Database.Ping(ctx); err != nil {
		add("core", "database", Fail, "database not reachable: %s", shorten(err.Error()))
		return []ComponentReport{report}, findings
	}
	report.Reachable = true
	report.Version = opts.Manifest.CoreRelease
	add("core", "release", OK, "release %s", opts.Manifest.CoreRelease)
	add("core", "database", OK, "database reachable")

	pending, err := opts.Database.Pending(ctx)
	switch {
	case err != nil:
		add("core", "migrations", Fail, "cannot read migration state: %s", shorten(err.Error()))
	case len(pending) > 0:
		add("core", "migrations", Fail, "%d migration(s) pending, versions %v; run zs-core migrate", len(pending), pending)
	default:
		add("core", "migrations", OK, "all migrations applied")
	}

	if pin, ok := opts.Manifest.Component(components.Postgres); ok {
		pg.Expected = pin.Version
		version, err := opts.Database.ServerVersion(ctx)
		if err != nil {
			add(components.Postgres, "version", Warn, "cannot read the server version: %s", shorten(err.Error()))
		} else {
			pg.Reachable = true
			pg.Version = version
			major := version
			if i := strings.IndexAny(version, ". "); i >= 0 {
				major = version[:i]
			}
			// The pin is a major version: postgres:18-alpine is "18".
			f, matches := compareVersion(components.Postgres, version, major, pin.Version)
			findings = append(findings, f)
			pg.MatchesPin = matches
		}
	}

	before := opts.Now()
	dbNow, err := opts.Database.Now(ctx)
	after := opts.Now()
	if err != nil {
		add("core", "clock", Warn, "cannot read the database clock: %s", shorten(err.Error()))
	} else {
		local := before.Add(after.Sub(before) / 2)
		skew := dbNow.Sub(local)
		report.Extra = map[string]any{"clock_skew_ms": skew.Milliseconds()}
		abs := skew
		if abs < 0 {
			abs = -abs
		}
		if abs > opts.MaxClockSkew {
			add("core", "clock", Fail, "clock skew to the database is %s, above the limit of %s", skew.Round(time.Millisecond), opts.MaxClockSkew)
		} else {
			add("core", "clock", OK, "clock skew to the database is %s", skew.Round(time.Millisecond))
		}
	}

	return []ComponentReport{report, pg}, findings
}
