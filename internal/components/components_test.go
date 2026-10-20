package components

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const repoRoot = "../.."

func TestRepositoryManifestParses(t *testing.T) {
	m, err := Load(filepath.Join(repoRoot, "components.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{Keycloak, KillBill, KAUI, Postgres} {
		c, ok := m.Component(name)
		if !ok || c.Version == "" || c.Image == "" {
			t.Errorf("component %s incomplete: %+v", name, c)
		}
		if tag := ImageTag(c.Image); tag != c.Version && name != Postgres {
			t.Errorf("%s image tag %q differs from version %q", name, tag, c.Version)
		}
	}
	if p, ok := m.PinnedPlugin("stripe"); !ok || p.Version == "" {
		t.Errorf("stripe plugin pin missing: %+v", p)
	}
	if c := m.Components[Keycloak]; c.Released != "2026-10-13" {
		t.Errorf("keycloak released = %q", c.Released)
	}
}

func TestRepositoryDigestsAreWellFormed(t *testing.T) {
	m, err := Load(filepath.Join(repoRoot, "components.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	well := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	for name, c := range m.Components {
		if c.Digest != "" && !well.MatchString(c.Digest) {
			t.Errorf("%s digest %q is not sha256:<64 hex>", name, c.Digest)
		}
	}
}

func TestParseRejectsMissingAndUnknown(t *testing.T) {
	if _, err := Parse([]byte("core_release: 1\ncomponents: {}\n")); err == nil ||
		!strings.Contains(err.Error(), "components.keycloak is missing") {
		t.Fatalf("missing components: %v", err)
	}
	if _, err := Parse([]byte("core_release: 1\nvesions: {}\n")); err == nil {
		t.Fatal("unknown key must be rejected")
	}
	if _, err := Parse([]byte("{{")); err == nil {
		t.Fatal("invalid yaml must be rejected")
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"quay.io/keycloak/keycloak:26.8.1": "26.8.1",
		"postgres:18-alpine":               "18-alpine",
		"localhost:5000/app":               "",
		"killbill/kaui":                    "",
	}
	for in, want := range cases {
		if got := ImageTag(in); got != want {
			t.Errorf("ImageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestComposeImagesMatchPins keeps deploy/compose/compose.yaml honest: the
// default image of every pinned component must be its digest pin from
// components.yaml, with the tag in a comment on the same line.
func TestComposeImagesMatchPins(t *testing.T) {
	m, err := Load(filepath.Join(repoRoot, "components.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Component{
		"POSTGRES_IMAGE": m.Components[Postgres],
		"KEYCLOAK_IMAGE": m.Components[Keycloak],
		"KILLBILL_IMAGE": m.Components[KillBill],
		"KAUI_IMAGE":     m.Components[KAUI],
		"MAILPIT_IMAGE":  m.Components["mailpit"],
	}
	for variable, c := range want {
		re := regexp.MustCompile(`\$\{` + variable + `:-([^}]+)\}(.*)`)
		matches := re.FindAllStringSubmatch(string(data), -1)
		if len(matches) == 0 {
			t.Errorf("compose.yaml has no default for %s", variable)
		}
		for _, match := range matches {
			if match[1] != c.PinnedReference() {
				t.Errorf("compose.yaml default for %s is %q, components.yaml pins %q", variable, match[1], c.PinnedReference())
			}
			if strings.TrimSpace(match[2]) != "# "+c.Image {
				t.Errorf("compose.yaml default for %s is not followed by the tag comment %q", variable, "# "+c.Image)
			}
		}
	}
}

func TestParseRequiresWellFormedDigests(t *testing.T) {
	base := func(digest string) string {
		lines := "core_release: 1\ncomponents:\n"
		for _, name := range []string{Keycloak, KillBill, KAUI, Postgres} {
			d := "sha256:" + strings.Repeat("a", 64)
			if name == KAUI {
				d = digest
			}
			lines += "  " + name + ":\n    version: \"1\"\n    image: x/" + name + ":1\n"
			if d != "" {
				lines += "    digest: " + d + "\n"
			}
		}
		return lines
	}
	if _, err := Parse([]byte(base("sha256:" + strings.Repeat("b", 64)))); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	if _, err := Parse([]byte(base(""))); err == nil || !strings.Contains(err.Error(), "components.kaui.digest is missing") {
		t.Fatalf("missing digest: %v", err)
	}
	if _, err := Parse([]byte(base("sha256:ABC"))); err == nil || !strings.Contains(err.Error(), "components.kaui.digest") {
		t.Fatalf("malformed digest: %v", err)
	}
}

func TestPinnedReference(t *testing.T) {
	c := Component{Image: "quay.io/keycloak/keycloak:26.8.1", Digest: "sha256:" + strings.Repeat("0", 64)}
	if got := c.PinnedReference(); got != "quay.io/keycloak/keycloak@sha256:"+strings.Repeat("0", 64) {
		t.Fatalf("PinnedReference = %q", got)
	}
	cases := map[string]string{
		"postgres:18-alpine":           "postgres",
		"localhost:5000/app:1":         "localhost:5000/app",
		"localhost:5000/app":           "localhost:5000/app",
		"killbill/kaui@sha256:abc":     "killbill/kaui",
		"killbill/kaui:4.0@sha256:abc": "killbill/kaui",
	}
	for in, want := range cases {
		if got := ImageRepository(in); got != want {
			t.Errorf("ImageRepository(%q) = %q, want %q", in, got, want)
		}
	}
}
