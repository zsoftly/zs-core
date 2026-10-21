package kbplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/components"
)

// DB is what the DDL steps need from a database: the Kill Bill database for
// the tables and Core's database for the record.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Querier reads a database.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ErrDDLChanged means Core recorded another DDL for the same plugin version.
var ErrDDLChanged = errors.New("another DDL was applied for this plugin version")

// MissingTables returns the tables of the pin that the Kill Bill database
// does not have, in the order components.yaml lists them.
func MissingTables(ctx context.Context, kb Querier, pin components.Plugin) ([]string, error) {
	if len(pin.Tables) == 0 {
		return nil, nil
	}
	present, err := existing(ctx, kb, `SELECT t FROM unnest($1::text[]) AS t WHERE to_regclass(t) IS NOT NULL`, pin.Tables)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, t := range pin.Tables {
		if !present[t] {
			missing = append(missing, t)
		}
	}
	return missing, nil
}

func existing(ctx context.Context, db Querier, sql string, names []string) (map[string]bool, error) {
	rows, err := db.Query(ctx, sql, names)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, n := range found {
		out[n] = true
	}
	return out, nil
}

// Result is what Apply did.
type Result struct {
	Plugin  string   `json:"plugin"`
	Version string   `json:"version"`
	Digest  string   `json:"ddl_sha256"`
	Created []string `json:"created_tables"`
	Present []string `json:"present_tables"`
	Domains []string `json:"created_domains,omitempty"`
	Forced  bool     `json:"forced,omitempty"`
}

// Applied reports whether Apply created anything.
func (r Result) Applied() bool { return len(r.Created) > 0 || len(r.Domains) > 0 }

// ApplyOptions describe an apply.
type ApplyOptions struct {
	// AppliedBy is recorded with the DDL, such as "zs-core-cli:<operator>".
	AppliedBy string
	// Force applies a DDL whose digest differs from one already recorded
	// for the same plugin version.
	Force bool
}

// Apply creates the plugin's tables that the Kill Bill database lacks and
// records the DDL in Core's killbill_plugin_ddl table.
//
// It runs only the statements of the tables that do not exist yet, and of
// the domains that do not exist yet, in one transaction. A table that
// exists is never dropped or changed, even when the DDL starts with a DROP
// TABLE for it, so applying twice changes nothing. A DDL whose tables differ
// from components.yaml is refused. A DDL whose digest differs from one Core
// recorded for the same plugin version is refused unless opts.Force is set.
func Apply(ctx context.Context, kb, core DB, d DDL, pin components.Plugin, opts ApplyOptions) (Result, error) {
	res := Result{Plugin: pin.Name, Version: pin.Version, Digest: d.Digest, Forced: opts.Force, Created: []string{}, Present: []string{}}
	if strings.TrimSpace(opts.AppliedBy) == "" {
		return res, errors.New("plugin ddl apply: the operator is required")
	}
	if err := CheckTables(d, pin); err != nil {
		return res, err
	}
	record, err := core.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = record.Rollback(context.WithoutCancel(ctx)) }()
	// Hold the record lock until Core commits, including the Kill Bill
	// transaction, so concurrent applies cannot bypass the digest check.
	if _, err := record.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('zs-core.plugin.ddl:' || current_schema() || ':' || $1, 0))`, pin.Name); err != nil {
		return res, err
	}
	other, err := otherDigests(ctx, record, pin, d.Digest)
	if err != nil {
		return res, err
	}
	if len(other) > 0 && !opts.Force {
		return res, fmt.Errorf("plugin %s %s: %w (recorded sha256 %s, this DDL %s). Read both DDLs and run again with --force",
			pin.Name, pin.Version, ErrDDLChanged, strings.Join(other, ", "), d.Digest)
	}
	tables := Tables(d.Statements)
	if err := pgx.BeginFunc(ctx, kb, func(tx pgx.Tx) error {
		// Plugins share domains, so serialize schema work across plugins.
		// Read object existence only after this lock, on this transaction.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('zs-core.plugin.ddl:' || current_schema(), 0))`); err != nil {
			return err
		}
		presentTables, err := existing(ctx, tx, `SELECT t FROM unnest($1::text[]) AS t WHERE to_regclass(t) IS NOT NULL`, tables)
		if err != nil {
			return fmt.Errorf("read the Kill Bill tables: %w", err)
		}
		domains := Domains(d.Statements)
		presentDomains, err := existing(ctx, tx, `SELECT t FROM unnest($1::text[]) AS t WHERE to_regtype(t) IS NOT NULL`, domains)
		if err != nil {
			return fmt.Errorf("read the Kill Bill domains: %w", err)
		}
		var run []Statement
		for _, s := range d.Statements {
			switch {
			case s.Kind == KindCreateType:
				if !presentDomains[s.Object] {
					run = append(run, s)
				}
			case !contains(tables, s.Object):
				return fmt.Errorf("plugin %s %s: the statement %q acts on %s, which the DDL does not create", pin.Name, pin.Version, shorten(s.SQL, 60), s.Object)
			case !presentTables[s.Object]:
				run = append(run, s)
			}
		}
		for _, t := range tables {
			if presentTables[t] {
				res.Present = append(res.Present, t)
			} else {
				res.Created = append(res.Created, t)
			}
		}
		for _, dn := range domains {
			if !presentDomains[dn] {
				res.Domains = append(res.Domains, dn)
			}
		}
		for _, s := range run {
			if _, err := tx.Exec(ctx, s.SQL); err != nil {
				return fmt.Errorf("plugin %s %s: %q: %w", pin.Name, pin.Version, shorten(s.SQL, 60), err)
			}
		}
		return nil
	}); err != nil {
		return res, err
	}
	if _, err := record.Exec(ctx, `INSERT INTO killbill_plugin_ddl (plugin, version, ddl_sha256, tables, created_tables, applied_by, forced)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (plugin, version, ddl_sha256) DO UPDATE SET
    created_tables = CASE WHEN cardinality(EXCLUDED.created_tables) > 0 THEN EXCLUDED.created_tables ELSE killbill_plugin_ddl.created_tables END,
    applied_by = EXCLUDED.applied_by, forced = killbill_plugin_ddl.forced OR EXCLUDED.forced, applied_at = now()`,
		pin.Name, pin.Version, d.Digest, tables, res.Created, opts.AppliedBy, opts.Force); err != nil {
		return res, fmt.Errorf("record the plugin DDL in Core's database: %w", err)
	}
	return res, record.Commit(ctx)
}

func otherDigests(ctx context.Context, core Querier, pin components.Plugin, digest string) ([]string, error) {
	rows, err := core.Query(ctx, `SELECT ddl_sha256 FROM killbill_plugin_ddl WHERE plugin = $1 AND version = $2 AND ddl_sha256 <> $3 ORDER BY applied_at`,
		pin.Name, pin.Version, digest)
	if err != nil {
		return nil, fmt.Errorf("read killbill_plugin_ddl in Core's database: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Record is a row of killbill_plugin_ddl.
type Record struct {
	Plugin  string
	Version string
	Digest  string
}

// Recorded returns the DDLs Core recorded, by plugin and version.
func Recorded(ctx context.Context, core Querier) ([]Record, error) {
	rows, err := core.Query(ctx, `SELECT plugin, version, ddl_sha256 FROM killbill_plugin_ddl ORDER BY plugin, version, applied_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Record, error) {
		var rec Record
		err := r.Scan(&rec.Plugin, &rec.Version, &rec.Digest)
		return rec, err
	})
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
