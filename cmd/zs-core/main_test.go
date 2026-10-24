package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/doctor"
)

func TestRunUsageAndConfigErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "Usage") {
		t.Fatalf("no args: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"launch"}, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("unknown: %d %q", code, errOut.String())
	}
	out.Reset()
	if code := run([]string{"help"}, nil, &out, &errOut); code != 0 || !strings.Contains(out.String(), "migrate") {
		t.Fatalf("help: %d %q", code, out.String())
	}
	errOut.Reset()
	if code := run([]string{"serve"}, []string{"ZS_DATABASE_URL=postgres://u:pw-123@db/x", "ZS_DEV_TRUSTED_SCOPE_HEADERS=true", "ZS_LISTEN_ADDR=0.0.0.0:8080"}, &out, &errOut); code != 2 {
		t.Fatalf("removed trusted-header flag: exit %d", code)
	}
	if !strings.Contains(errOut.String(), "ZS_DEV_TRUSTED_SCOPE_HEADERS is not a known variable") || strings.Contains(errOut.String(), "pw-123") {
		t.Fatalf("config error = %q", errOut.String())
	}
}

const testComponents = `core_release: 1.0.0-test
components:
  keycloak: {version: 26.8.1, image: "quay.io/keycloak/keycloak:26.8.1", digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
  killbill: {version: 0.24.22, image: "killbill/killbill:0.24.22", digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
  kaui: {version: 4.0.31, image: "killbill/kaui:4.0.31", digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
  postgres: {version: "18", image: "postgres:18-alpine", digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
`

func writeComponents(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "components.yaml")
	if err := os.WriteFile(path, []byte(testComponents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUpgradeCheck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/keycloak/keycloak/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"26.8.2","published_at":"2026-10-27T10:00:00Z"},{"tag_name":"26.8.1","published_at":"2026-10-13T10:00:00Z"}]`)
	})
	mux.HandleFunc("GET /repos/killbill/killbill/releases", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	mux.HandleFunc("GET /v2/repositories/killbill/kaui/tags", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"name":"4.0.31","last_updated":"2026-09-01T00:00:00Z"}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	upgradeFeeds.github, upgradeFeeds.dockerHub = srv.URL, srv.URL
	defer func() { upgradeFeeds.github, upgradeFeeds.dockerHub = "", "" }()
	file := writeComponents(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"upgrade", "check", "--components", file}, nil, &out, &errOut); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "keycloak: pinned 26.8.1, newer: 26.8.2 (patch, 2026-10-27)") ||
		!strings.Contains(out.String(), "kaui: pinned 4.0.31, up to date") {
		t.Fatalf("stdout = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "warning: killbill") || !strings.Contains(errOut.String(), "HTTP 502") {
		t.Fatalf("stderr = %q", errOut.String())
	}

	out.Reset()
	errOut.Reset()
	if code := run([]string{"upgrade", "check", "--strict", "--components", file}, nil, &out, &errOut); code != 1 {
		t.Fatalf("--strict with a failing feed: exit %d", code)
	}
}

func TestUpgradeUsageAndMissingComponents(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"upgrade"}, nil, &out, &errOut); code != 2 {
		t.Fatalf("upgrade without subcommand: exit %d", code)
	}
	if code := run([]string{"upgrade", "apply"}, nil, &out, &errOut); code != 2 {
		t.Fatalf("unknown upgrade subcommand: exit %d", code)
	}
	errOut.Reset()
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if code := run([]string{"upgrade", "check", "--components", missing}, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "components file") {
		t.Fatalf("missing file: exit %d, %q", code, errOut.String())
	}
}

func TestDoctorReportsEverythingEvenWhenDatabaseIsDown(t *testing.T) {
	kc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer kc.Close()
	file := writeComponents(t)
	environ := []string{
		"ZS_DATABASE_URL=postgres://u:pw-secret@127.0.0.1:1/x?sslmode=disable",
		"ZS_KEYCLOAK_URL=" + kc.URL,
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"doctor", "--components", file}, environ, &out, &errOut); code != 1 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"FAIL core: database not reachable", "OK   keycloak: reachable", "FAIL killbill: ZS_KILLBILL_URL is not set"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "pw-secret") {
		t.Errorf("output leaks the database password:\n%s", text)
	}

	out.Reset()
	if code := run([]string{"doctor", "--json", "--components", file}, environ, &out, &errOut); code != 1 {
		t.Fatalf("json exit %d", code)
	}
	var report struct {
		OK       bool `json:"ok"`
		Findings []struct {
			Component string `json:"component"`
			Status    string `json:"status"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json output: %v\n%s", err, out.String())
	}
	if report.OK || len(report.Findings) == 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestDoctorConfigurationErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"doctor"}, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "ZS_DATABASE_URL is required") {
		t.Fatalf("no config: exit %d, %q", code, errOut.String())
	}
	errOut.Reset()
	environ := []string{"ZS_DATABASE_URL=postgres://u:p@127.0.0.1:1/x"}
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if code := run([]string{"doctor", "--components", missing}, environ, &out, &errOut); code != 2 {
		t.Fatalf("missing components: exit %d", code)
	}
}

func TestDoctorExitCodeTreatsSkipsAsFailureUnlessAllowed(t *testing.T) {
	skipped := doctor.Report{Findings: []doctor.Finding{
		{Component: "keycloak", Check: "version", Status: doctor.Skip, Message: "version not checked (no admin credentials)"},
	}}
	if code := doctorExitCode(skipped, false); code != 1 {
		t.Fatalf("skip without --allow-skips: exit %d", code)
	}
	if code := doctorExitCode(skipped, true); code != 0 {
		t.Fatalf("skip with --allow-skips: exit %d", code)
	}
	failed := doctor.Report{Findings: []doctor.Finding{{Component: "kaui", Check: "reachable", Status: doctor.Fail}}}
	if code := doctorExitCode(failed, true); code != 1 {
		t.Fatalf("fail with --allow-skips: exit %d", code)
	}
	if code := doctorExitCode(doctor.Report{}, false); code != 0 {
		t.Fatalf("clean report: exit %d", code)
	}
}

func TestDoctorExitCodeRejectsUnlistedPluginFailures(t *testing.T) {
	for _, state := range []string{"RUNNING", "STOPPED"} {
		t.Run(state, func(t *testing.T) {
			report := doctor.Report{Findings: []doctor.Finding{
				{Component: "killbill", Check: "plugin", Status: doctor.Fail,
					Message: "plugin custom version 1.0.0 is not listed in components.yaml (" + state + ")"},
			}}
			for _, allowSkips := range []bool{false, true} {
				if code := doctorExitCode(report, allowSkips); code != 1 {
					t.Errorf("unlisted %s plugin with allow-skips=%t: exit %d, want 1", state, allowSkips, code)
				}
			}
			report.Findings = append(report.Findings, doctor.Finding{Component: "killbill", Check: "plugin_tables", Status: doctor.Skip})
			if code := doctorExitCode(report, true); code != 1 {
				t.Errorf("allowed table-check skip masked unlisted plugin: exit %d", code)
			}
		})
	}
}

func TestGrantRevokeUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"grant", "remove", "--brand", "zs-reference"}, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "grant revoke") {
		t.Fatalf("unknown subcommand: %d %q", code, errOut.String())
	}
	errOut.Reset()
	args := []string{"grant", "revoke", "--brand", "zs-reference", "--subject", "s1", "--scope", "operator", "--operator", "ops"}
	if code := run(args, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "grant revoke: --subject, --scope, --reason and --operator are required") {
		t.Fatalf("revoke without a reason: %d %q", code, errOut.String())
	}
}

// brand apply refuses a plugin repository that is not https before it
// opens the database or calls an engine.
func TestBrandRefusesPluginRepositoryWithoutHTTPS(t *testing.T) {
	dir := t.TempDir()
	ref, err := os.ReadFile(filepath.Join("..", "..", "brands", "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := strings.Replace(string(ref), "  currencies: [CAD, USD]\n",
		"  currencies: [CAD, USD]\n  payment_plugin:\n    name: stripe\n    mode: test\n    config_ref: env://BRAND_ZS_REFERENCE__STRIPE\n", 1)
	if manifest == string(ref) {
		t.Fatal("the reference manifest has no currencies line to extend")
	}
	if err := os.WriteFile(filepath.Join(dir, "zs-reference.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	pins := testComponents + `killbill_plugins:
  repository: http://repo1.maven.org/maven2
  approved_payment_plugins:
    - {name: stripe, version: 8.0.3, artifact: "org.kill-bill.billing.plugin.java:stripe-plugin", status: pinned_not_installed, sha256: "` + strings.Repeat("a", 64) + `"}
`
	componentsPath := filepath.Join(dir, "components.yaml")
	if err := os.WriteFile(componentsPath, []byte(pins), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"ZS_DATABASE_URL=postgres://u:pw@127.0.0.1:1/x", "ZS_KEYCLOAK_URL=http://127.0.0.1:1", "ZS_KEYCLOAK_ADMIN_USER=a",
		"ZS_KEYCLOAK_ADMIN_PASSWORD=b", "ZS_KILLBILL_URL=http://127.0.0.1:1", "ZS_KILLBILL_ADMIN_USER=a", "ZS_KILLBILL_ADMIN_PASSWORD=b"}
	var out, errOut bytes.Buffer
	code := run([]string{"brand", "diff", "--components", componentsPath, filepath.Join(dir, "zs-reference.yaml")}, env, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "not an https URL") {
		t.Fatalf("http repository: exit %d %q", code, errOut.String())
	}
}

func TestPluginUsageAndConfigurationErrors(t *testing.T) {
	componentsPath := filepath.Join("..", "..", "components.yaml")
	db := "ZS_DATABASE_URL=postgres://u:pw@127.0.0.1:1/x"
	cases := []struct {
		args []string
		env  []string
		want string
	}{
		{[]string{"plugin"}, nil, "Usage"},
		{[]string{"plugin", "ddl"}, nil, "Usage"},
		{[]string{"plugin", "ddl", "apply", "--components", componentsPath, "stripe"}, []string{db}, "--operator is required"},
		{[]string{"plugin", "ddl", "apply", "--components", componentsPath, "--operator", "x"}, []string{db}, "name one plugin, or pass --all"},
		{[]string{"plugin", "ddl", "apply", "--components", componentsPath, "--operator", "x", "--all", "stripe"}, []string{db}, "not both"},
		{[]string{"plugin", "ddl", "apply", "--components", componentsPath, "--operator", "x", "unknown"}, []string{db}, "is not pinned"},
		{[]string{"plugin", "ddl", "apply", "--components", componentsPath, "--operator", "x", "stripe"}, []string{db}, "needs ZS_KILLBILL_DATABASE_URL"},
		{[]string{"plugin", "ddl", "apply", "--components", componentsPath, "--operator", "x", "paypal-express"}, []string{db}, "has no Java artifact"},
		{[]string{"plugin", "ddl", "show", "--components", componentsPath, "unknown"}, nil, "is not pinned"},
		{[]string{"plugin", "ddl", "show", "--components", componentsPath, "stripe"}, []string{"ZS_PLUGIN_MIRROR_DIR=relative"}, "must be an absolute directory path"},
		{[]string{"plugin", "mirror", "sync", "--components", componentsPath}, nil, "needs ZS_PLUGIN_MIRROR_DIR"},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if code := run(c.args, c.env, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%v: exit %d %q, want %q", c.args, code, errOut.String(), c.want)
		}
	}
}

// start-closure needs its flags, and does not ask for engine settings: it only
// writes Core, so with no Keycloak or Kill Bill setting it gets as far as the
// database, where this configuration has none.
func TestTenantStartClosureFlagsAndNoEngineSettings(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"tenant", "start-closure", "--brand", "zs-reference"}, nil, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "--reason") {
		t.Fatalf("missing flags = %d %q", code, errOut.String())
	}
	errOut.Reset()
	environ := []string{"ZS_DATABASE_URL=postgres://u:pw-123@127.0.0.1:1/x?connect_timeout=1"}
	args := []string{"tenant", "start-closure", "--brand", "zs-reference", "--tenant", "tnt_01m5qxsm2vefytm7qehn7fnrx5", "--reason", "left", "--operator", "me"}
	if code := run(args, environ, &out, &errOut); code == 0 || strings.Contains(errOut.String(), "ZS_KEYCLOAK_URL") || strings.Contains(errOut.String(), "ZS_KILLBILL_URL") {
		t.Fatalf("start-closure = %d %q, want a database error and no engine settings asked for", code, errOut.String())
	}
	errOut.Reset()
	if code := run(append([]string{}, args[:1]...), nil, &out, &errOut); code != 2 {
		t.Fatalf("tenant alone = %d", code)
	}
	if code := run([]string{"tenant", "suspend", "--brand", "zs-reference", "--tenant", "tnt_01m5qxsm2vefytm7qehn7fnrx5", "--reason", "left", "--operator", "me"}, environ, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "ZS_KEYCLOAK_URL") {
		t.Fatalf("suspend without engine settings = %d %q, want the Keycloak setting asked for", code, errOut.String())
	}
}
