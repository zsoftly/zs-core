//go:build integration

package postgrestest_test

import (
	"context"
	"testing"

	"github.com/zsoftly/zs-core/internal/postgrestest"
)

// Every test gets a migrated database of its own: rows of one are invisible
// to another, and the name is unique.
func TestNewGivesMigratedAndIsolatedDatabases(t *testing.T) {
	ctx := context.Background()
	a, b := postgrestest.New(t), postgrestest.New(t)
	if a.Schema == b.Schema {
		t.Fatalf("two tests share %q", a.Schema)
	}
	for name, db := range map[string]*postgrestest.DB{"a": a, "b": b} {
		var migrated int
		if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&migrated); err != nil || migrated == 0 {
			t.Fatalf("%s: applied migrations = %d, %v", name, migrated, err)
		}
	}
	if _, err := a.Pool.Exec(ctx, `CREATE TABLE isolation_probe (v int)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Pool.Exec(ctx, `INSERT INTO isolation_probe VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	var visible bool
	if err := b.Pool.QueryRow(ctx, `SELECT to_regclass('isolation_probe') IS NOT NULL`).Scan(&visible); err != nil || visible {
		t.Fatalf("a table of one test is visible in another: %v, %v", visible, err)
	}
}

// A database is gone when its test ends.
func TestNewDropsItsDatabaseWhenTheTestEnds(t *testing.T) {
	ctx := context.Background()
	var gone string
	t.Run("use", func(t *testing.T) {
		db := postgrestest.New(t)
		gone = db.Schema
		if err := db.Pool.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err != nil {
			t.Fatal(err)
		}
	})
	observer := postgrestest.New(t)
	var exists bool
	if err := observer.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1 OR EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1))`, gone).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("%q still exists after its test ended", gone)
	}
}
