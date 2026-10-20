package database

import (
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
