package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration is one embedded forward-only migration.
type Migration struct {
	Version  int64
	Name     string
	SQL      string
	Checksum string
}

var fileName = regexp.MustCompile(`^([0-9]{6})_([a-z0-9]+(?:_[a-z0-9]+)*)\.up\.sql$`)

// Load reads and orders the migrations in fsys. Every .sql file must follow
// the NNNNNN_name.up.sql pattern, versions must be unique and start at 1
// without gaps.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var migrations []Migration
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := fileName.FindStringSubmatch(entry.Name())
		if match == nil {
			return nil, fmt.Errorf("migration file %q does not match NNNNNN_name.up.sql", entry.Name())
		}
		version, _ := strconv.ParseInt(match[1], 10, 64)
		body, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version: version, Name: match[2], SQL: string(body), Checksum: hex.EncodeToString(sum[:]),
		})
	}
	slices.SortFunc(migrations, func(a, b Migration) int { return int(a.Version - b.Version) })
	for i, m := range migrations {
		if m.Version != int64(i+1) {
			return nil, fmt.Errorf("migration versions must start at 000001 and have no gaps or duplicates; found %06d at position %d", m.Version, i+1)
		}
	}
	if len(migrations) == 0 {
		return nil, errors.New("no migrations found")
	}
	return migrations, nil
}

const createHistorySQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version bigint PRIMARY KEY,
    name text NOT NULL,
    checksum text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`

// The lock key is derived from the current schema, so isolated schemas in one
// database migrate independently while two processes on the same schema
// serialise.
const lockSQL = `SELECT pg_advisory_lock(hashtextextended('zs-core.migrations.' || current_schema(), 0))`
const unlockSQL = `SELECT pg_advisory_unlock(hashtextextended('zs-core.migrations.' || current_schema(), 0))`

// Migrate applies every pending migration in fsys under a session advisory
// lock. Each migration runs in its own transaction together with its history
// row. It returns the versions it applied. An applied migration whose
// checksum changed is an error: migrations are immutable once released.
func Migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]int64, error) {
	migrations, err := Load(fsys)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, lockSQL); err != nil {
		return nil, fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Unlock on a fresh context so a cancelled ctx cannot leave the
		// session lock held on a pooled connection.
		if _, err := conn.Exec(context.WithoutCancel(ctx), unlockSQL); err != nil {
			conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()

	if _, err := conn.Exec(ctx, createHistorySQL); err != nil {
		return nil, fmt.Errorf("create migration history: %w", err)
	}
	applied, err := appliedChecksums(ctx, conn.Conn())
	if err != nil {
		return nil, err
	}

	var done []int64
	for _, m := range migrations {
		if checksum, ok := applied[m.Version]; ok {
			if checksum != m.Checksum {
				return done, fmt.Errorf("migration %06d_%s was changed after it was applied", m.Version, m.Name)
			}
			continue
		}
		err := pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
				m.Version, m.Name, m.Checksum)
			return err
		})
		if err != nil {
			return done, fmt.Errorf("apply migration %06d_%s: %w", m.Version, m.Name, err)
		}
		done = append(done, m.Version)
	}
	return done, nil
}

// Pending returns the versions in fsys that the database has not applied. A
// missing history table means every migration is pending. A checksum
// mismatch is an error.
func Pending(ctx context.Context, db Querier, fsys fs.FS) ([]int64, error) {
	migrations, err := Load(fsys)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check migration history: %w", err)
	}
	applied := map[int64]string{}
	if exists {
		applied, err = appliedChecksums(ctx, db)
		if err != nil {
			return nil, err
		}
	}
	var pending []int64
	for _, m := range migrations {
		checksum, ok := applied[m.Version]
		if !ok {
			pending = append(pending, m.Version)
			continue
		}
		if checksum != m.Checksum {
			return nil, fmt.Errorf("migration %06d_%s was changed after it was applied", m.Version, m.Name)
		}
	}
	return pending, nil
}

// Querier is satisfied by pgxpool.Pool, pgx.Conn and pgx.Tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func appliedChecksums(ctx context.Context, db Querier) (map[int64]string, error) {
	rows, err := db.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	applied, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct {
		version  int64
		checksum string
	}, error) {
		var r struct {
			version  int64
			checksum string
		}
		err := row.Scan(&r.version, &r.checksum)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("read migration history: %w", err)
	}
	out := make(map[int64]string, len(applied))
	for _, r := range applied {
		out[r.version] = r.checksum
	}
	return out, nil
}
