package tenant

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Only the code that starts a closure may insert a closure settlement, because
// the start rule of the database is the one place that says how it starts, and
// a second writer would be a second reading of that rule. A new writer is added
// to this list on purpose.
var settlementInsertAllowlist = map[string]bool{
	// "pkg/tenant/closure_start.go": the code that starts a closure.
}

var settlementInsert = regexp.MustCompile(`(?is)insert\s+into\s+(\w+\.)?"?tenant_closure_settlements`)

func TestOnlyTheStarterInsertsClosureSettlements(t *testing.T) {
	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		goSource := strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
		migration := strings.HasPrefix(rel, "migrations/") && strings.HasSuffix(path, ".sql")
		if !goSource && !migration {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if settlementInsert.Match(data) {
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range found {
		if !settlementInsertAllowlist[rel] {
			t.Errorf("%s inserts closure settlements and is not on the allowlist", rel)
		}
	}
}

func TestSettlementInsertPatternSeesEveryFormOfTheStatement(t *testing.T) {
	for _, sql := range []string{
		"INSERT INTO tenant_closure_settlements (a) VALUES (1)",
		"insert   into\n\ttenant_closure_settlements(a)",
		"INSERT INTO public.tenant_closure_settlements (a)",
		`INSERT INTO "tenant_closure_settlements" (a)`,
		"WITH t AS (SELECT 1) INSERT INTO tenant_closure_settlements SELECT 1",
	} {
		if !settlementInsert.MatchString(sql) {
			t.Errorf("the pattern misses %q", sql)
		}
	}
	if settlementInsert.MatchString("SELECT * FROM tenant_closure_settlements") {
		t.Error("the pattern matches a select")
	}
}
