package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAcceptsOnlyEnvAndFile(t *testing.T) {
	good := map[string]Ref{
		"env://ZS_STAFF_CLIENT_SECRET": {Scheme: "env", Name: "ZS_STAFF_CLIENT_SECRET"},
		"file:///run/secrets/kb":       {Scheme: "file", Name: "/run/secrets/kb"},
	}
	for ref, want := range good {
		got, err := Parse(ref)
		if err != nil || got != want || got.String() != ref {
			t.Errorf("Parse(%q) = %+v, %v", ref, got, err)
		}
	}
	bad := []string{
		"", "sk_live_abcdef", "vault://kv/zs", "secret://zcp/stripe", "ENV://X", "env://", "env://1BAD",
		"env://A-B", "file://relative/path", "file:///a/../etc/passwd", "file:///", "https://x",
	}
	for _, ref := range bad {
		if _, err := Parse(ref); err == nil {
			t.Errorf("Parse(%q) accepted", ref)
		}
		if IsReference(ref) {
			t.Errorf("IsReference(%q) = true", ref)
		}
	}
}

func TestResolveEnv(t *testing.T) {
	r := Default{LookupEnv: func(name string) (string, bool) {
		switch name {
		case "SET":
			return "s3cr3t-value", true
		case "EMPTY":
			return "", true
		}
		return "", false
	}}
	v, err := r.Resolve(context.Background(), "env://SET")
	if err != nil || v != "s3cr3t-value" {
		t.Fatalf("resolve = %q, %v", v, err)
	}
	for _, ref := range []string{"env://EMPTY", "env://MISSING"} {
		if _, err := r.Resolve(context.Background(), ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", ref, err)
		}
	}
}

func TestResolveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("file-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := Default{}.Resolve(context.Background(), "file://"+path)
	if err != nil || v != "file-secret-value" {
		t.Fatalf("resolve = %q, %v", v, err)
	}
	if _, err := (Default{}).Resolve(context.Background(), "file://"+filepath.Join(dir, "missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := (Default{}).Resolve(context.Background(), "file://"+dir); err == nil {
		t.Fatal("directory accepted")
	}
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", MaxFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Default{}).Resolve(context.Background(), "file://"+big); err == nil {
		t.Fatal("oversized file accepted")
	}
}

func TestErrorsNeverEchoValues(t *testing.T) {
	const pasted = "sk_live_0123456789abcdef"
	_, err := Default{}.Resolve(context.Background(), pasted)
	if err == nil || strings.Contains(err.Error(), pasted) {
		t.Fatalf("error = %v", err)
	}
	_, err = Parse("env://" + pasted + "-x")
	if err == nil || strings.Contains(err.Error(), pasted) {
		t.Fatalf("error = %v", err)
	}
	r := Default{LookupEnv: func(string) (string, bool) { return "", false }}
	_, err = r.Resolve(context.Background(), "env://NAME_ONLY")
	if err == nil || !strings.Contains(err.Error(), "env://NAME_ONLY") {
		t.Fatalf("error should name the reference: %v", err)
	}
}
