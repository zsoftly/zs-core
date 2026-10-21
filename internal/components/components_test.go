package components

import (
	"fmt"
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
		"POSTGRES_IMAGE":  m.Components[Postgres],
		"KEYCLOAK_IMAGE":  m.Components[Keycloak],
		"KILLBILL_IMAGE":  m.Components[KillBill],
		"KAUI_IMAGE":      m.Components[KAUI],
		"MAILPIT_IMAGE":   m.Components["mailpit"],
		"GOTENBERG_IMAGE": m.Components[Gotenberg],
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

// A plugin with an artifact needs the SHA-256 of its jar, and the pinned
// plugins of this release carry one.
func TestPluginArtifactsNeedSha256(t *testing.T) {
	head := "core_release: 1\ncomponents:\n"
	for _, name := range []string{Keycloak, KillBill, KAUI, Postgres} {
		head += "  " + name + ":\n    version: \"1\"\n    image: x/" + name + ":1\n    digest: sha256:" + strings.Repeat("a", 64) + "\n"
	}
	plugin := func(extra string) string {
		return head + "killbill_plugins:\n  repository: https://repo.example.test\n  approved_payment_plugins:\n    - name: stripe\n      version: 8.0.3\n      artifact: g:stripe-plugin\n      status: pinned_not_installed\n" + extra
	}
	if _, err := Parse([]byte(plugin("      sha256: " + strings.Repeat("c", 64) + "\n"))); err != nil {
		t.Fatalf("valid sha256 rejected: %v", err)
	}
	for name, extra := range map[string]string{"missing": "", "malformed": "      sha256: ABC\n", "prefixed": "      sha256: sha256:" + strings.Repeat("c", 64) + "\n"} {
		if _, err := Parse([]byte(plugin(extra))); err == nil || !strings.Contains(err.Error(), "stripe.sha256") {
			t.Errorf("%s sha256: %v", name, err)
		}
	}
	m, err := Load("../../components.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range m.Plugins() {
		if p.Artifact != "" && len(p.Sha256) != 64 {
			t.Errorf("%s has no sha256", p.Name)
		}
		if p.HasArtifact() && len(p.Tables) == 0 {
			t.Errorf("%s lists no tables", p.Name)
		}
	}
	if g, ok := m.Component(Gotenberg); !ok || !g.Optional || g.Licence != "MIT" || ImageTag(g.Image) != g.Version {
		t.Errorf("gotenberg pin = %+v", g)
	}
}

func TestLoadValidatesPluginStatuses(t *testing.T) {
	head := "core_release: 1\ncomponents:\n"
	for _, name := range []string{Keycloak, KillBill, KAUI, Postgres} {
		head += "  " + name + ":\n    version: \"1\"\n    image: x/" + name + ":1\n    digest: sha256:" + strings.Repeat("a", 64) + "\n"
	}
	for _, list := range []string{"approved_payment_plugins", "approved_invoice_plugins", "installable"} {
		t.Run(list, func(t *testing.T) {
			for _, tc := range []struct {
				name, status string
				omit, valid  bool
			}{
				{name: "missing", omit: true},
				{name: "empty"},
				{name: "unknown", status: "optional"},
				{name: "required typo", status: "requried"},
				{name: "required", status: "required", valid: true},
				{name: "verified install", status: "verified_install", valid: true},
				{name: "not installed", status: "pinned_not_installed", valid: true},
				{name: "no artifact", status: "no_java_artifact", valid: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					doc := head + "killbill_plugins:\n  " + list + ":\n    - name: demo\n      version: 1.0.0\n"
					if tc.status != "no_java_artifact" {
						doc += "      artifact: g:demo-plugin\n      sha256: " + strings.Repeat("b", 64) + "\n"
					}
					if !tc.omit {
						doc += fmt.Sprintf("      status: %q\n", tc.status)
					}
					path := filepath.Join(t.TempDir(), "components.yaml")
					if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
						t.Fatal(err)
					}
					m, err := Load(path)
					if !tc.valid {
						if err == nil || !strings.Contains(err.Error(), "killbill_plugins."+list+" demo.status") {
							t.Fatalf("invalid plugin status was not rejected with its location: %v", err)
						}
						if m.CoreRelease != "" || len(m.Components) != 0 || len(m.Plugins()) != 0 {
							t.Fatal("invalid plugin status returned a usable manifest")
						}
						return
					}
					if err != nil {
						t.Fatalf("known status rejected: %v", err)
					}
					plugin, ok := m.PinnedPlugin("demo")
					if !ok || plugin.Status != tc.status || plugin.Required() != (tc.status == "required") {
						t.Fatalf("status semantics changed: %+v", plugin)
					}
					wantRequired := 0
					if tc.status == "required" {
						wantRequired = 1
					}
					if got := len(m.RequiredPlugins()); got != wantRequired {
						t.Fatalf("required plugin count = %d, want %d", got, wantRequired)
					}
				})
			}
		})
	}
}
