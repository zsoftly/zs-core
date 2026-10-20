package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const slug = "zs-reference"

func TestParseAcceptsOnlyEnvAndFile(t *testing.T) {
	good := map[string]Ref{
		"env://BRAND_ZS_REFERENCE__X": {Scheme: "env", Name: "BRAND_ZS_REFERENCE__X"},
		"file:///run/secrets/kb":      {Scheme: "file", Name: "/run/secrets/kb"},
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

func TestEnvPrefix(t *testing.T) {
	if EnvPrefix("zs-reference") != "BRAND_ZS_REFERENCE__" || EnvPrefix("acme") != "BRAND_ACME__" || EnvPrefix("zs") != "BRAND_ZS__" {
		t.Fatal(EnvPrefix("zs-reference"))
	}
}

func TestValidSlug(t *testing.T) {
	for _, s := range []string{"zs", "zs-reference", "a1", "acme-2-b", strings.Repeat("a", 40)} {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false", s)
		}
	}
	for _, s := range []string{"", "a", "-zs", "zs-", "zs--reference", "ZS", "zs_reference", "zs.reference", strings.Repeat("a", 41)} {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true", s)
		}
	}
}

// No valid slug's prefix starts another's, so no variable is in the
// namespace of two brands, whichever brand exists first.
func TestEnvPrefixesNeverOverlap(t *testing.T) {
	slugs := []string{"zs", "zs-reference", "zs-ref", "zs-ref-x", "zs-r", "zs-reference-2", "zsr", "zs-0", "a-zs", "zs-ref-x-y"}
	for _, a := range slugs {
		for _, b := range slugs {
			if !ValidSlug(a) || !ValidSlug(b) {
				t.Fatalf("invalid test slug %s or %s", a, b)
			}
			if a != b && strings.HasPrefix(EnvPrefix(b), EnvPrefix(a)) {
				t.Errorf("prefix of %s (%s) starts the prefix of %s (%s)", a, EnvPrefix(a), b, EnvPrefix(b))
			}
		}
	}
	// zs and zs-reference: neither reaches the other's variables.
	if err := Check("zs", "", "env://BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("zs reached zs-reference: %v", err)
	}
	if err := Check("zs-reference", "", "env://BRAND_ZS__KILLBILL_TENANT_API_SECRET"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("zs-reference reached zs: %v", err)
	}
	if err := Check("zs", "", "env://BRAND_ZS__KILLBILL_TENANT_API_SECRET"); err != nil {
		t.Fatalf("zs own variable: %v", err)
	}
	if err := Check("zs-reference", "", "env://BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET"); err != nil {
		t.Fatalf("zs-reference own variable: %v", err)
	}
}

func TestCheckNamespacesReferencesPerBrand(t *testing.T) {
	dir := t.TempDir()
	allowed := []string{
		"env://BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET",
		"file://" + filepath.Join(dir, slug, "kb"),
		"file://" + filepath.Join(dir, slug, "sub", "kb"),
	}
	for _, ref := range allowed {
		if err := Check(slug, dir, ref); err != nil {
			t.Errorf("Check(%q) = %v", ref, err)
		}
	}
	refused := []string{
		"env://BRAND_ZS_SECOND__X",           // another brand
		"env://BRAND_ZS_REFERENCE__",         // the prefix alone
		"env://BRAND_ZS_REFERENCE_X",         // a single underscore
		"env://KEYCLOAK_STAFF_CLIENT_SECRET", // no namespace
		"env://ZS_DATABASE_URL",              // Core configuration
		"file:///etc/passwd",
		"file://" + filepath.Join(dir, "zs-second", "x"), // another brand's directory
		"file://" + filepath.Join(dir, slug),             // the directory itself
		"file://" + filepath.Join(dir, "x"),
	}
	for _, ref := range refused {
		if err := Check(slug, dir, ref); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Check(%q) = %v, want ErrNotAllowed", ref, err)
		}
	}
	if err := Check(slug, "", "file://"+filepath.Join(dir, slug, "kb")); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("file reference without ZS_SECRETS_DIR: %v", err)
	}
	for _, bad := range []string{"Bad_Slug", "zs--reference", "zs-"} {
		if err := Check(bad, dir, "env://"+EnvPrefix(bad)+"X"); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("invalid slug %s: %v", bad, err)
		}
	}
	if err := Check(slug, dir, "vault://x"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported scheme: %v", err)
	}
}

// The resolver refuses a foreign reference even when validation was
// bypassed, and never looks the value up.
func TestResolveRefusesOtherBrandsSecrets(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "zs-second")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "x"), []byte("other-brand-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	looked := false
	r := Default{Dir: dir, LookupEnv: func(string) (string, bool) { looked = true; return "other-brand-secret", true }}
	for _, ref := range []string{"env://BRAND_ZS_SECOND__X", "file:///etc/passwd", "file://" + filepath.Join(other, "x")} {
		v, err := r.Resolve(context.Background(), slug, ref)
		if !errors.Is(err, ErrNotAllowed) || v != "" {
			t.Errorf("Resolve(%q) = %q, %v", ref, v, err)
		}
	}
	if looked {
		t.Fatal("the resolver looked up a variable outside the brand's namespace")
	}
}

func TestResolveEnv(t *testing.T) {
	r := Default{LookupEnv: func(name string) (string, bool) {
		switch name {
		case "BRAND_ZS_REFERENCE__SET":
			return "s3cr3t-value", true
		case "BRAND_ZS_REFERENCE__EMPTY":
			return "", true
		}
		return "", false
	}}
	v, err := r.Resolve(context.Background(), slug, "env://BRAND_ZS_REFERENCE__SET")
	if err != nil || v != "s3cr3t-value" {
		t.Fatalf("resolve = %q, %v", v, err)
	}
	for _, ref := range []string{"env://BRAND_ZS_REFERENCE__EMPTY", "env://BRAND_ZS_REFERENCE__MISSING"} {
		if _, err := r.Resolve(context.Background(), slug, ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", ref, err)
		}
	}
}

func TestResolveFile(t *testing.T) {
	dir := t.TempDir()
	brandDir := filepath.Join(dir, slug)
	if err := os.MkdirAll(brandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brandDir, "secret")
	if err := os.WriteFile(path, []byte("file-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Default{Dir: dir}
	ctx := context.Background()
	v, err := r.Resolve(ctx, slug, "file://"+path)
	if err != nil || v != "file-secret-value" {
		t.Fatalf("resolve = %q, %v", v, err)
	}

	// Every failure of a valid reference is the same error with the same
	// message shape: missing, unreadable, not a file, too large, empty, or a
	// link that leaves the brand's directory.
	if err := os.MkdirAll(filepath.Join(brandDir, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(brandDir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", MaxFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(brandDir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(brandDir, "unreadable")
	if err := os.WriteFile(unreadable, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("outside-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(brandDir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	cases := []string{filepath.Join(brandDir, "missing"), filepath.Join(brandDir, "dir"), big, empty, link}
	if os.Geteuid() != 0 {
		cases = append(cases, unreadable)
	}
	for _, p := range cases {
		ref := "file://" + p
		v, err := r.Resolve(ctx, slug, ref)
		if !errors.Is(err, ErrNotFound) || v != "" {
			t.Errorf("%s: %q, %v", p, v, err)
			continue
		}
		if err.Error() != ErrNotFound.Error()+": "+ref {
			t.Errorf("%s: message %q differs from the common form", p, err)
		}
	}
}

func TestErrorsNeverEchoValues(t *testing.T) {
	const pasted = "sk_live_0123456789abcdef"
	_, err := Default{}.Resolve(context.Background(), slug, pasted)
	if err == nil || strings.Contains(err.Error(), pasted) {
		t.Fatalf("error = %v", err)
	}
	_, err = Parse("env://" + pasted + "-x")
	if err == nil || strings.Contains(err.Error(), pasted) {
		t.Fatalf("error = %v", err)
	}
	r := Default{LookupEnv: func(string) (string, bool) { return "", false }}
	_, err = r.Resolve(context.Background(), slug, "env://BRAND_ZS_REFERENCE__NAME_ONLY")
	if err == nil || !strings.Contains(err.Error(), "env://BRAND_ZS_REFERENCE__NAME_ONLY") {
		t.Fatalf("error should name the reference: %v", err)
	}
}
