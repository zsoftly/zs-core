//go:build integration

package database_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/migrations"
)

func TestMigrateAppliesOnceAndReportsPending(t *testing.T) {
	db := postgrestest.NewEmpty(t)
	ctx := context.Background()

	all, err := database.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := database.Pending(ctx, db.Pool, migrations.FS)
	if err != nil || len(pending) != len(all) {
		t.Fatalf("pending before = %v, %v", pending, err)
	}

	applied, err := database.Migrate(ctx, db.Pool, migrations.FS)
	if err != nil || len(applied) != len(all) {
		t.Fatalf("first run applied %v, %v", applied, err)
	}
	applied, err = database.Migrate(ctx, db.Pool, migrations.FS)
	if err != nil || len(applied) != 0 {
		t.Fatalf("second run applied %v, %v", applied, err)
	}
	pending, err = database.Pending(ctx, db.Pool, migrations.FS)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after = %v, %v", pending, err)
	}

	var tables int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
		WHERE table_schema = $1 AND table_name IN ('idempotency_keys', 'outbox', 'audit_events', 'schema_migrations')`,
		db.Schema).Scan(&tables); err != nil || tables != 4 {
		t.Fatalf("tables in isolated schema = %d, %v", tables, err)
	}
}

func TestMigrateConcurrentRunnersApplyEachVersionOnce(t *testing.T) {
	db := postgrestest.NewEmpty(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	results := make([][]int64, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = database.Migrate(ctx, db.Pool, migrations.FS)
		}()
	}
	wg.Wait()

	total := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("runner %d: %v", i, errs[i])
		}
		total += len(results[i])
	}
	all, _ := database.Load(migrations.FS)
	if total != len(all) {
		t.Fatalf("versions applied across runners = %d, want %d", total, len(all))
	}
}

func TestMigrateRejectsChangedMigration(t *testing.T) {
	db := postgrestest.NewEmpty(t)
	ctx := context.Background()
	original := fstest.MapFS{"000001_first.up.sql": {Data: []byte("CREATE TABLE first_table (id int)")}}
	if _, err := database.Migrate(ctx, db.Pool, original); err != nil {
		t.Fatal(err)
	}
	changed := fstest.MapFS{"000001_first.up.sql": {Data: []byte("CREATE TABLE first_table (id bigint)")}}
	if _, err := database.Migrate(ctx, db.Pool, changed); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Fatalf("changed migration error = %v", err)
	}
	if _, err := database.Pending(ctx, db.Pool, changed); err == nil {
		t.Fatal("Pending accepted a changed migration")
	}
}

func TestMigrateFailedMigrationRollsBack(t *testing.T) {
	db := postgrestest.NewEmpty(t)
	ctx := context.Background()
	broken := fstest.MapFS{
		"000001_good.up.sql": {Data: []byte("CREATE TABLE good_table (id int)")},
		"000002_bad.up.sql":  {Data: []byte("CREATE TABLE bad_table (id int); SELECT * FROM missing_table")},
	}
	applied, err := database.Migrate(ctx, db.Pool, broken)
	if err == nil || len(applied) != 1 {
		t.Fatalf("applied = %v, err = %v", applied, err)
	}
	var exists bool
	if err := db.Pool.QueryRow(ctx, `SELECT to_regclass('bad_table') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("failed migration left bad_table behind: %v %v", exists, err)
	}
	pending, err := database.Pending(ctx, db.Pool, broken)
	if err != nil || len(pending) != 1 || pending[0] != 2 {
		t.Fatalf("pending = %v, %v", pending, err)
	}
}
