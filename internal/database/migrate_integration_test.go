//go:build integration

package database_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
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

func TestMigrateRefusesInvalidHistoryBeforePendingWrites(t *testing.T) {
	ctx := context.Background()
	current := fstest.MapFS{
		"000001_first.up.sql":  {Data: []byte("CREATE TABLE pending_sentinel (id int)")},
		"000002_second.up.sql": {Data: []byte("CREATE TABLE second_table (id int)")},
		"000003_third.up.sql":  {Data: []byte("CREATE TABLE third_table (id int)")},
	}
	loaded, err := database.Load(current)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		history map[int64]string
		want    string
	}{
		{
			name:    "checksum drift in a later applied migration",
			history: map[int64]string{2: "changed"},
			want:    "migration 000002_second was changed after it was applied",
		},
		{
			name:    "negative version",
			history: map[int64]string{-1: "unknown", 1: loaded[0].Checksum},
			want:    "applied migration version -00001",
		},
		{
			name:    "zero version",
			history: map[int64]string{0: "unknown", 1: loaded[0].Checksum},
			want:    "applied migration version 000000",
		},
		{
			name:    "newer version",
			history: map[int64]string{1: loaded[0].Checksum, 4: "future"},
			want:    "applied migration version 000004",
		},
		{
			name:    "history hole",
			history: map[int64]string{2: loaded[1].Checksum},
			want:    "missing migration version 000001 before applied version 000002",
		},
		{
			name:    "internal history gap",
			history: map[int64]string{1: loaded[0].Checksum, 3: loaded[2].Checksum},
			want:    "missing migration version 000002 before applied version 000003",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := postgrestest.NewEmpty(t)
			createMigrationHistory(t, ctx, db.Pool)
			for version, checksum := range tc.history {
				if _, err := db.Pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, version, fmt.Sprintf("migration-%d", version), checksum); err != nil {
					t.Fatal(err)
				}
			}

			before := migrationHistory(t, ctx, db.Pool)

			if _, err := database.Pending(ctx, db.Pool, current); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Pending() error = %v, want %q", err, tc.want)
			}
			assertMigrationHistoryUntouched(t, ctx, db.Pool, before)
			assertTableAbsent(t, ctx, db.Pool, "pending_sentinel")
			assertTableAbsent(t, ctx, db.Pool, "second_table")
			assertTableAbsent(t, ctx, db.Pool, "third_table")
			applied, err := database.Migrate(ctx, db.Pool, current)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Migrate() error = %v, want %q", err, tc.want)
			}
			if len(applied) != 0 {
				t.Errorf("Migrate() applied = %v, want none", applied)
			}
			assertMigrationHistoryUntouched(t, ctx, db.Pool, before)
			assertTableAbsent(t, ctx, db.Pool, "pending_sentinel")
			assertTableAbsent(t, ctx, db.Pool, "second_table")
			assertTableAbsent(t, ctx, db.Pool, "third_table")
		})
	}
}

func TestMigrateProgressesFromReleasedMigrationEighteenToNineteen(t *testing.T) {
	db := postgrestest.NewEmpty(t)
	ctx := context.Background()
	throughEighteen := migrationFilesThrough(t, 18)
	throughNineteen := migrationFilesThrough(t, 19)
	if applied, err := database.Migrate(ctx, db.Pool, throughEighteen); err != nil || len(applied) != 18 {
		t.Fatalf("Migrate() through 18 = %v, %v", applied, err)
	}
	pending, err := database.Pending(ctx, db.Pool, throughNineteen)
	if err != nil || len(pending) != 1 || pending[0] != 19 {
		t.Fatalf("Pending() through 19 = %v, %v", pending, err)
	}
	applied, err := database.Migrate(ctx, db.Pool, throughNineteen)
	if err != nil || len(applied) != 1 || applied[0] != 19 {
		t.Fatalf("Migrate() through 19 = %v, %v", applied, err)
	}
	pending, err = database.Pending(ctx, db.Pool, throughNineteen)
	if err != nil || len(pending) != 0 {
		t.Fatalf("Pending() after 19 = %v, %v", pending, err)
	}
}

func createMigrationHistory(t *testing.T, ctx context.Context, db *pgxpool.Pool) {
	t.Helper()
	if _, err := db.Exec(ctx, `CREATE TABLE schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatal(err)
	}
}

func migrationHistory(t *testing.T, ctx context.Context, db *pgxpool.Pool) string {
	t.Helper()
	var rows string
	if err := db.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(schema_migrations) ORDER BY version)::text, '[]') FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func assertMigrationHistoryUntouched(t *testing.T, ctx context.Context, db *pgxpool.Pool, want string) {
	t.Helper()
	if got := migrationHistory(t, ctx, db); got != want {
		t.Errorf("migration history changed: before=%s after=%s", want, got)
	}
}

func assertTableAbsent(t *testing.T, ctx context.Context, db *pgxpool.Pool, name string) {
	t.Helper()
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil || exists {
		t.Fatalf("table %s exists = %v, %v", name, exists, err)
	}
}

func migrationFilesThrough(t *testing.T, version int64) fstest.MapFS {
	t.Helper()
	loaded, err := database.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	files := make(fstest.MapFS, version)
	for _, migration := range loaded {
		if migration.Version > version {
			break
		}
		name := fmt.Sprintf("%06d_%s.up.sql", migration.Version, migration.Name)
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: body}
	}
	return files
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
