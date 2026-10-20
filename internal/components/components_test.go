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
// default image of every pinned component must equal components.yaml.
func TestComposeImagesMatchPins(t *testing.T) {
	m, err := Load(filepath.Join(repoRoot, "components.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	want := map[string]string{
		"POSTGRES_IMAGE": m.Components[Postgres].Image,
		"KEYCLOAK_IMAGE": m.Components[Keycloak].Image,
		"KILLBILL_IMAGE": m.Components[KillBill].Image,
		"KAUI_IMAGE":     m.Components[KAUI].Image,
		"MAILPIT_IMAGE":  m.Components["mailpit"].Image,
	}
	for variable, image := range want {
		re := regexp.MustCompile(`\$\{` + variable + `:-([^}]+)\}`)
		matches := re.FindAllStringSubmatch(text, -1)
		if len(matches) == 0 {
			t.Errorf("compose.yaml has no default for %s", variable)
		}
		for _, match := range matches {
			if match[1] != image {
				t.Errorf("compose.yaml default for %s is %q, components.yaml pins %q", variable, match[1], image)
			}
		}
	}
}
