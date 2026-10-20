package gen

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGeneratedTypesAreFresh fails when types.gen.go does not match what the
// pinned generator produces from api/openapi.yaml.
func TestGeneratedTypesAreFresh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping generator run in -short mode")
	}
	output := filepath.Join(t.TempDir(), "types.gen.go")
	cmd := exec.Command("go", "tool", "oapi-codegen",
		"-config", "../../../api/oapi-codegen.yaml",
		"-o", output,
		"../../../api/openapi.yaml")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run oapi-codegen: %v\n%s", err, out)
	}
	want, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("types.gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("internal/httpapi/gen/types.gen.go is stale; run make generate and commit the result")
	}
}
