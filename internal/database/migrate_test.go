package database

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zsoftly/zs-core/migrations"
)

func TestLoadEmbeddedMigrations(t *testing.T) {
	ms, err := Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 3 || ms[0].Version != 1 || ms[0].Name != "idempotency_keys" {
		t.Fatalf("unexpected migrations: %+v", ms)
	}
	for _, m := range ms {
		if strings.Contains(m.SQL, "public.") {
			t.Errorf("migration %06d qualifies a schema; migrations must use unqualified names", m.Version)
		}
	}
}

func TestLoadRejectsBadSets(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"bad name":    {"1_init.up.sql": {Data: []byte("SELECT 1")}},
		"down file":   {"000001_init.up.sql": {Data: []byte("SELECT 1")}, "000001_init.down.sql": {Data: []byte("SELECT 1")}},
		"gap":         {"000001_a.up.sql": {Data: []byte("SELECT 1")}, "000003_c.up.sql": {Data: []byte("SELECT 1")}},
		"duplicate":   {"000001_a.up.sql": {Data: []byte("SELECT 1")}, "000001_b.up.sql": {Data: []byte("SELECT 1")}},
		"not from 1":  {"000002_a.up.sql": {Data: []byte("SELECT 1")}},
		"empty":       {},
		"upper name":  {"000001_Init.up.sql": {Data: []byte("SELECT 1")}},
		"other files": {"000001_a.up.sql": {Data: []byte("SELECT 1")}, "README.md": {Data: []byte("x")}},
	}
	for name, fsys := range cases {
		if _, err := Load(fsys); err == nil {
			t.Errorf("%s: Load succeeded", name)
		}
	}
}

func TestValidateAppliedMigrations(t *testing.T) {
	loaded, err := Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	latest := loaded[len(loaded)-1].Version
	if latest < 19 {
		t.Fatalf("latest migration = %d, want at least 19", latest)
	}
	checksums := make(map[int64]string, len(loaded))
	for _, migration := range loaded {
		checksums[migration.Version] = migration.Checksum
	}
	cases := []struct {
		name    string
		applied map[int64]string
		want    string
	}{
		{name: "empty history", applied: map[int64]string{}},
		{name: "contiguous partial history", applied: contiguousChecksums(checksums, 18)},
		{name: "complete history through released migration nineteen", applied: contiguousChecksums(checksums, 19)},
		{name: "complete current history", applied: contiguousChecksums(checksums, latest)},
		{
			name:    "changed embedded migration",
			applied: map[int64]string{2: "changed"},
			want:    "migration 000002_outbox was changed after it was applied",
		},
		{
			name:    "newer database history",
			applied: map[int64]string{1: checksums[1], latest + 1: "future"},
			want:    fmt.Sprintf("database schema has applied migration version %06d, but this binary supports versions through %06d", latest+1, latest),
		},
		{
			name:    "zero historical version",
			applied: map[int64]string{0: "unknown", 1: checksums[1]},
			want:    fmt.Sprintf("database schema has applied migration version 000000, but this binary supports versions through %06d", latest),
		},
		{
			name:    "negative historical version",
			applied: map[int64]string{-1: "unknown", 1: checksums[1]},
			want:    fmt.Sprintf("database schema has applied migration version -00001, but this binary supports versions through %06d", latest),
		},
		{
			name:    "noncontiguous history",
			applied: map[int64]string{2: checksums[2]},
			want:    "database schema history is missing migration version 000001 before applied version 000002",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAppliedMigrations(loaded, tc.applied)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("validateAppliedMigrations() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func contiguousChecksums(checksums map[int64]string, through int64) map[int64]string {
	applied := make(map[int64]string, through)
	for version := int64(1); version <= through; version++ {
		applied[version] = checksums[version]
	}
	return applied
}
