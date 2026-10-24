package tenant

import (
	"io/fs"
	"os"
	"path/filepath"
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(data)), "insert into tenant_closure_settlements") {
			rel, _ := filepath.Rel(root, path)
			found = append(found, filepath.ToSlash(rel))
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
