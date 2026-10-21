package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
)

func manifest() components.Manifest {
	return components.Manifest{
		CoreRelease: "1.0.0-test",
		Components: map[string]components.Component{
			components.Keycloak: {Version: "26.8.1", Image: "quay.io/keycloak/keycloak:26.8.1"},
			components.KillBill: {Version: "0.24.22", Image: "killbill/killbill:0.24.22"},
			components.KAUI:     {Version: "4.0.31", Image: "killbill/kaui:4.0.31"},
			components.Postgres: {Version: "18", Image: "postgres:18-alpine"},
		},
		KillBillPlugins: components.KillBillPlugins{
			ApprovedPaymentPlugins: []components.Plugin{
				{Name: "stripe", Version: "6.0.1", Status: components.PluginStatusRequired},
				{Name: "adyen", Version: "0.10.0", Status: components.PluginStatusPinnedNotInstalled},
				{Name: "paypal-express", Status: components.PluginStatusNoJavaArtifact},
			},
		},
	}
}

func find(t *testing.T, fs []Finding, component, check string) Finding {
	t.Helper()
	for _, f := range fs {
		if f.Component == component && f.Check == check {
			return f
		}
	}
	t.Fatalf("no finding %s/%s in %v", component, check, fs)
	return Finding{}
}

func keycloakServer(t *testing.T, version string, realms ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /realms/{realm}", func(w http.ResponseWriter, r *http.Request) {
		realm := r.PathValue("realm")
		if realm == "master" {
			w.WriteHeader(http.StatusOK)
			return
		}
		for _, known := range realms {
			if realm == known {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("POST /realms/master/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("username") != "admin" || r.Form.Get("password") != "pw" || r.Form.Get("client_id") != "admin-cli" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok"})
	})
	mux.HandleFunc("GET /admin/serverinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"systemInfo": map[string]string{"version": version}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestKeycloakMatchesPin(t *testing.T) {
	srv := keycloakServer(t, "26.8.1", "zs-reference")
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(),
		Keycloak: Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw", Realm: "zs-reference"}}
	report, fs := checkKeycloak(context.Background(), opts)
	if !report.Reachable || report.Version != "26.8.1" || report.MatchesPin == nil || !*report.MatchesPin {
		t.Fatalf("report = %+v", report)
	}
	if f := find(t, fs, "keycloak", "version"); f.Status != OK {
		t.Fatalf("version finding = %v", f)
	}
	if f := find(t, fs, "keycloak", "realm"); f.Status != OK {
		t.Fatalf("realm finding = %v", f)
	}
}

func TestKeycloakVersionMismatch(t *testing.T) {
	srv := keycloakServer(t, "26.8.0", "zs-reference")
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(),
		Keycloak: Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"}}
	report, fs := checkKeycloak(context.Background(), opts)
	f := find(t, fs, "keycloak", "version")
	if f.Status != Fail || !strings.Contains(f.Message, "26.8.0") || !strings.Contains(f.Message, "26.8.1") {
		t.Fatalf("version finding = %v", f)
	}
	if report.MatchesPin == nil || *report.MatchesPin {
		t.Fatalf("matches_pin = %v", report.MatchesPin)
	}
}

func TestKeycloakRealmMissingAndNoAdminCredentials(t *testing.T) {
	srv := keycloakServer(t, "26.8.1")
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(),
		Keycloak: Target{URL: srv.URL, Realm: "zs-reference"}}
	report, fs := checkKeycloak(context.Background(), opts)
	if f := find(t, fs, "keycloak", "realm"); f.Status != Fail {
		t.Fatalf("realm finding = %v", f)
	}
	if f := find(t, fs, "keycloak", "version"); f.Status != Skip || !strings.HasPrefix(f.String(), "SKIP keycloak: version not checked (no admin credentials)") {
		t.Fatalf("version finding = %v", f)
	}
	if report.MatchesPin != nil {
		t.Fatalf("matches_pin must be unknown, got %v", *report.MatchesPin)
	}
}

func TestKeycloakBadAdminPasswordAndUnreachable(t *testing.T) {
	srv := keycloakServer(t, "26.8.1")
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(),
		Keycloak: Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "wrong"}}
	_, fs := checkKeycloak(context.Background(), opts)
	f := find(t, fs, "keycloak", "version")
	if f.Status != Fail || strings.Contains(f.Message, "wrong") {
		t.Fatalf("version finding = %v", f)
	}

	url := srv.URL
	srv.Close()
	opts.Keycloak.URL = url
	report, fs := checkKeycloak(context.Background(), opts)
	if report.Reachable || find(t, fs, "keycloak", "reachable").Status != Fail {
		t.Fatalf("unreachable not reported: %+v %v", report, fs)
	}
}

func killBillServer(t *testing.T, kbVersion string, healthy bool, plugins []map[string]any) *httptest.Server {
	t.Helper()
	return killBillNodesServer(t, healthy, []map[string]any{{"kbVersion": kbVersion, "pluginsInfo": plugins}})
}

func killBillNodesServer(t *testing.T, healthy bool, nodes []map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /1.0/healthcheck", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"main.pool.ConnectivityCheck": map[string]any{"healthy": true},
			"queues":                      map[string]any{"healthy": healthy},
		})
	})
	mux.HandleFunc("GET /1.0/kb/nodesInfo", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "pw" {
			w.Header().Set("WWW-Authenticate", `BASIC realm="application"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(nodes)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var platformBundle = map[string]any{
	"bundleSymbolicName": "org.kill-bill.billing.killbill-platform-osgi-bundles-kpm",
	"pluginKey":          nil, "pluginName": "org.kill-bill.billing.killbill-platform-osgi-bundles-kpm",
	"version": nil, "state": "RUNNING",
}

var runningStripe = map[string]any{"pluginKey": "stripe", "pluginName": "stripe-plugin", "version": "6.0.1", "state": "RUNNING"}

func TestKillBillMatchesPin(t *testing.T) {
	srv := killBillServer(t, "0.24.22", true, []map[string]any{platformBundle, runningStripe})
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(),
		KillBill: Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"}}
	report, fs := checkKillBill(context.Background(), opts)
	if report.Version != "0.24.22" || report.MatchesPin == nil || !*report.MatchesPin || !report.Reachable {
		t.Fatalf("report = %+v", report)
	}
	for _, check := range []string{"health", "version", "plugin", "plugins"} {
		if f := find(t, fs, "killbill", check); f.Status != OK {
			t.Errorf("%s finding = %v", check, f)
		}
	}
	for _, f := range fs {
		if f.Status != OK && f.Check != "plugin_tables" {
			t.Errorf("unexpected finding %v", f)
		}
	}
	if f := find(t, fs, "killbill", "plugin_tables"); f.Status != Skip || !strings.Contains(f.Message, "ZS_KILLBILL_DATABASE_URL") {
		t.Errorf("plugin tables without the database = %v", f)
	}
}

func TestKillBillRequiredPluginMissingOrStopped(t *testing.T) {
	opts := Options{Manifest: manifest(), KillBill: Target{AdminUser: "admin", AdminPassword: "pw"}}

	srv := killBillServer(t, "0.24.22", true, []map[string]any{platformBundle})
	opts.HTTPClient, opts.KillBill.URL = srv.Client(), srv.URL
	_, fs := checkKillBill(context.Background(), opts)
	var missing bool
	for _, f := range fs {
		if f.Check == "plugin" && f.Status == Fail && strings.Contains(f.Message, "stripe 6.0.1 is not installed") {
			missing = true
		}
		if strings.Contains(f.Message, "adyen") || strings.Contains(f.Message, "paypal") {
			t.Errorf("a plugin not expected on the stack was reported: %v", f)
		}
	}
	if !missing {
		t.Fatalf("missing required plugin is not a failure: %v", fs)
	}

	stopped := map[string]any{"pluginKey": "stripe", "pluginName": "stripe-plugin", "version": "6.0.1", "state": "STOPPED"}
	srv = killBillServer(t, "0.24.22", true, []map[string]any{platformBundle, stopped})
	opts.HTTPClient, opts.KillBill.URL = srv.Client(), srv.URL
	_, fs = checkKillBill(context.Background(), opts)
	var notRunning bool
	for _, f := range fs {
		if f.Check == "plugin" && f.Status == Fail && strings.Contains(f.Message, "stripe is STOPPED") {
			notRunning = true
		}
	}
	if !notRunning {
		t.Fatalf("stopped required plugin is not a failure: %v", fs)
	}
}

func TestPrintedURLsAreRedacted(t *testing.T) {
	srv := killBillServer(t, "0.24.22", true, []map[string]any{platformBundle, runningStripe})
	u := strings.Replace(srv.URL, "http://", "http://ops:hunter2@", 1)
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(), KillBill: Target{URL: u, AdminUser: "admin", AdminPassword: "pw"}}
	_, fs := checkKillBill(context.Background(), opts)
	for _, f := range fs {
		if strings.Contains(f.Message, "hunter2") {
			t.Fatalf("password printed: %v", f)
		}
	}
	if f := find(t, fs, "killbill", "health"); !strings.Contains(f.Message, "xxxxx") {
		t.Fatalf("health finding = %v", f)
	}
}

func TestKillBillMismatchedVersionAndPlugin(t *testing.T) {
	plugins := []map[string]any{platformBundle,
		{"pluginKey": "stripe", "pluginName": "stripe-plugin", "version": "8.0.3", "state": "RUNNING"},
		{"pluginKey": "custom", "pluginName": "custom-plugin", "version": "1.0.0", "state": "STOPPED"},
	}
	srv := killBillServer(t, "0.24.21", true, plugins)
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(),
		KillBill: Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"}}
	report, fs := checkKillBill(context.Background(), opts)
	if f := find(t, fs, "killbill", "version"); f.Status != Fail || !strings.Contains(f.Message, "0.24.21") {
		t.Fatalf("version finding = %v", f)
	}
	var stripeFail, customFail bool
	for _, f := range fs {
		if f.Check == "plugin" && f.Status == Fail && strings.Contains(f.Message, "stripe") && strings.Contains(f.Message, "6.0.1") {
			stripeFail = true
		}
		if f.Check == "plugin" && f.Status == Fail && strings.Contains(f.Message, "custom") && strings.Contains(f.Message, "not listed in components.yaml") {
			customFail = true
		}
	}
	if !stripeFail || !customFail {
		t.Fatalf("plugin findings missing (stripeFail=%v customFail=%v): %v", stripeFail, customFail, fs)
	}
	if len(report.Plugins) != 3 {
		t.Fatalf("plugins = %+v", report.Plugins)
	}
}

func TestUnlistedPluginsFailTheReleaseReport(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		wantFail    bool
	}{
		{name: "approved plugin and platform bundle"},
		{name: "unlisted running plugin", state: "RUNNING", wantFail: true},
		{name: "unlisted stopped plugin", state: "STOPPED", wantFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugins := []map[string]any{platformBundle, runningStripe}
			if tc.state != "" {
				plugins = append(plugins, map[string]any{
					"pluginKey": "custom", "pluginName": "custom-plugin", "version": "1.0.0", "state": tc.state,
				})
			}
			kb := killBillServer(t, "0.24.22", true, plugins)
			kc := keycloakServer(t, "26.8.1", "zs-reference")
			now := time.Now()
			m := manifest()
			m.KillBillPlugins.PlatformBundlesInImage = []string{"kpm 0.41.23 (KPM OSGi bundle)"}
			report := Run(context.Background(), Options{
				Manifest:   m,
				Database:   fakeDB{version: "18.1", dbNow: func() time.Time { return now }},
				Keycloak:   Target{URL: kc.URL, AdminUser: "admin", AdminPassword: "pw", Realm: "zs-reference"},
				KillBill:   Target{URL: kb.URL, AdminUser: "admin", AdminPassword: "pw"},
				HTTPClient: http.DefaultClient,
				Now:        func() time.Time { return now },
				PluginTables: func(context.Context, components.Plugin) ([]string, error) {
					return nil, nil
				},
				BillingRecipients: func(context.Context) ([]Finding, error) {
					return []Finding{{Component: "core", Check: "billing_recipients", Status: OK, Message: "fixture recipients checked"}}, nil
				},
			})
			if report.Failed() != tc.wantFail || report.OK == tc.wantFail || report.Skipped() {
				t.Fatalf("release gate: failed=%t ok=%t skipped=%t, findings=%v", report.Failed(), report.OK, report.Skipped(), report.Findings)
			}
			failures := 0
			for _, f := range report.Findings {
				if f.Status != Fail {
					continue
				}
				failures++
				if f.Component != components.KillBill || f.Check != "plugin" ||
					!strings.Contains(f.Message, "custom version 1.0.0 is not listed in components.yaml ("+tc.state+")") {
					t.Fatalf("unrelated failure masked the plugin gate: %v", f)
				}
			}
			if tc.wantFail && failures != 1 {
				t.Fatalf("want exactly one unlisted-plugin failure, got %d", failures)
			}
			if f := find(t, report.Findings, components.KillBill, "plugins"); f.Status != OK || !strings.Contains(f.Message, "1 platform bundle(s)") {
				t.Fatalf("platform bundle exemption changed: %v", f)
			}
			var output bytes.Buffer
			if err := report.WriteJSON(&output); err != nil {
				t.Fatal(err)
			}
			var decoded Report
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.OK == tc.wantFail || decoded.Failed() != tc.wantFail || decoded.CoreRelease != m.CoreRelease {
				t.Fatalf("JSON release result = %+v", decoded)
			}
		})
	}
}

func TestKillBillChecksPluginsOnEveryNode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plugins []map[string]any
		failure string
	}{
		{name: "all nodes match", plugins: []map[string]any{platformBundle, runningStripe}},
		{name: "second node has unknown running plugin", plugins: []map[string]any{platformBundle, runningStripe,
			{"pluginKey": "custom", "version": "1.0.0", "state": "RUNNING"}}, failure: "custom version 1.0.0 is not listed"},
		{name: "second node has unknown stopped plugin", plugins: []map[string]any{platformBundle, runningStripe,
			{"pluginKey": "custom", "version": "1.0.0", "state": "STOPPED"}}, failure: "custom version 1.0.0 is not listed"},
		{name: "second node lacks required plugin", plugins: []map[string]any{platformBundle}, failure: "stripe 6.0.1 is not installed"},
		{name: "second node has wrong plugin version", plugins: []map[string]any{platformBundle,
			{"pluginKey": "stripe", "version": "8.0.3", "state": "RUNNING"}}, failure: "stripe version 8.0.3 does not match the pin 6.0.1"},
		{name: "second node has stopped required plugin", plugins: []map[string]any{platformBundle,
			{"pluginKey": "stripe", "version": "6.0.1", "state": "STOPPED"}}, failure: "stripe is STOPPED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := killBillNodesServer(t, true, []map[string]any{
				{"nodeName": "node-a", "kbVersion": "0.24.22", "pluginsInfo": []map[string]any{platformBundle, runningStripe}},
				{"nodeName": "node-b", "kbVersion": "0.24.22", "pluginsInfo": tc.plugins},
			})
			m := manifest()
			m.KillBillPlugins.PlatformBundlesInImage = []string{"kpm 0.41.23 (KPM OSGi bundle)"}
			_, findings := checkKillBill(context.Background(), Options{
				Manifest: m, HTTPClient: srv.Client(),
				KillBill:     Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"},
				PluginTables: func(context.Context, components.Plugin) ([]string, error) { return nil, nil },
			})
			report := Report{Findings: findings}
			if report.Failed() != (tc.failure != "") || report.Skipped() {
				t.Fatalf("node-b gate: failed=%t skipped=%t findings=%v", report.Failed(), report.Skipped(), findings)
			}
			var failures int
			for _, f := range findings {
				if f.Status == Fail {
					failures++
					if f.Check != "plugin" || !strings.Contains(f.Message, tc.failure) || !strings.Contains(f.Message, `node "node-b"`) {
						t.Fatalf("unrelated failure masked the second-node defect: %v", f)
					}
				}
			}
			if tc.failure != "" && failures != 1 {
				t.Fatalf("expected one second-node failure, got %d", failures)
			}
		})
	}
}

func TestKillBillChecksTablesForOptionalPluginOnSecondNode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed bool
		missing   bool
	}{
		{name: "optional plugin absent"},
		{name: "optional plugin tables present", installed: true},
		{name: "optional plugin tables missing", installed: true, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tablesManifest()
			optional := m.KillBillPlugins.Installable[0]
			if optional.Required() {
				t.Fatal("fixture must not require the optional plugin")
			}
			secondPlugins := []map[string]any{platformBundle, runningStripe}
			if tc.installed {
				secondPlugins = append(secondPlugins, map[string]any{
					"pluginKey": optional.Name, "version": optional.Version, "state": "RUNNING",
				})
			}
			srv := killBillNodesServer(t, true, []map[string]any{
				{"nodeName": "node-a", "kbVersion": "0.24.22", "pluginsInfo": []map[string]any{platformBundle, runningStripe}},
				{"nodeName": "node-b", "kbVersion": "0.24.22", "pluginsInfo": secondPlugins},
			})
			var asked []string
			component, findings := checkKillBill(context.Background(), Options{
				Manifest: m, HTTPClient: srv.Client(),
				KillBill: Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"},
				PluginTables: func(_ context.Context, pin components.Plugin) ([]string, error) {
					asked = append(asked, pin.Name)
					if pin.Name == optional.Name && tc.missing {
						return optional.Tables, nil
					}
					return nil, nil
				},
			})
			wantAsked := "stripe"
			if tc.installed {
				wantAsked += "," + optional.Name
			}
			if strings.Join(asked, ",") != wantAsked {
				t.Fatalf("table checks = %v, want %s", asked, wantAsked)
			}
			if len(component.Plugins) != 2 || component.Plugins[0].Key != "" || component.Plugins[1].Key != "stripe" {
				t.Fatalf("first-node plugin summary changed: %+v", component.Plugins)
			}
			report := Report{Findings: findings}
			if report.Failed() != tc.missing || report.Skipped() {
				t.Fatalf("optional plugin table gate: failed=%t skipped=%t findings=%v", report.Failed(), report.Skipped(), findings)
			}
			var failures int
			for _, f := range findings {
				if f.Status != Fail {
					continue
				}
				failures++
				if f.Check != "plugin_tables" || !strings.Contains(f.Message, "plugin "+optional.Name+" "+optional.Version+" tables missing: "+optional.Tables[0]) {
					t.Fatalf("unrelated failure masked the optional plugin tables: %v", f)
				}
			}
			if tc.missing && failures != 1 {
				t.Fatalf("expected one optional plugin table failure, got %d", failures)
			}
		})
	}
}

func TestKillBillUnhealthyAndAuthFailure(t *testing.T) {
	srv := killBillServer(t, "0.24.22", false, nil)
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(),
		KillBill: Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "wrong"}}
	_, fs := checkKillBill(context.Background(), opts)
	if f := find(t, fs, "killbill", "health"); f.Status != Fail || !strings.Contains(f.Message, "queues") {
		t.Fatalf("health finding = %v", f)
	}
	if f := find(t, fs, "killbill", "version"); f.Status != Fail || !strings.Contains(f.Message, "401") {
		t.Fatalf("version finding = %v", f)
	}

	opts.KillBill.AdminUser, opts.KillBill.AdminPassword = "", ""
	_, fs = checkKillBill(context.Background(), opts)
	if f := find(t, fs, "killbill", "version"); f.Status != Skip || !strings.HasPrefix(f.String(), "SKIP killbill: version not checked (no admin credentials)") {
		t.Fatalf("version finding without credentials = %v", f)
	}
}

func TestKAUI(t *testing.T) {
	redirected := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			redirected = true
			http.Redirect(w, r, "/users/sign_in", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(), KAUI: Target{URL: srv.URL}}
	report, fs := checkKAUI(context.Background(), opts)
	if !report.Reachable || !redirected || find(t, fs, "kaui", "reachable").Status != OK {
		t.Fatalf("report = %+v findings = %v", report, fs)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	opts.KAUI.URL = broken.URL
	report, fs = checkKAUI(context.Background(), opts)
	if report.Reachable || find(t, fs, "kaui", "reachable").Status != Fail {
		t.Fatalf("500 must fail: %+v %v", report, fs)
	}

	opts.KAUI.URL = ""
	_, fs = checkKAUI(context.Background(), opts)
	if f := find(t, fs, "kaui", "reachable"); f.Status != OK || !strings.Contains(f.Message, "skipped") {
		t.Fatalf("unset URL must be skipped: %v", f)
	}
}

type fakeDB struct {
	pingErr    error
	pending    []int64
	version    string
	dbNow      func() time.Time
	pendingErr error
}

func (f fakeDB) Ping(context.Context) error { return f.pingErr }
func (f fakeDB) Pending(context.Context) ([]int64, error) {
	return f.pending, f.pendingErr
}
func (f fakeDB) ServerVersion(context.Context) (string, error) { return f.version, nil }
func (f fakeDB) Now(context.Context) (time.Time, error)        { return f.dbNow(), nil }

func TestCoreDatabaseChecks(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	opts := Options{Manifest: manifest(), Now: func() time.Time { return now }, MaxClockSkew: 5 * time.Second}

	opts.Database = fakeDB{version: "18.0", dbNow: func() time.Time { return now.Add(300 * time.Millisecond) }}
	reports, fs := checkCore(context.Background(), opts)
	for _, check := range []string{"database", "migrations", "clock"} {
		if f := find(t, fs, "core", check); f.Status != OK {
			t.Errorf("%s finding = %v", check, f)
		}
	}
	if f := find(t, fs, "postgres", "version"); f.Status != OK || !strings.Contains(f.Message, "18.0") {
		t.Errorf("postgres finding = %v", f)
	}
	if len(reports) != 2 || reports[1].MatchesPin == nil || !*reports[1].MatchesPin {
		t.Errorf("reports = %+v", reports)
	}

	opts.Database = fakeDB{version: "17.4", pending: []int64{3, 4}, dbNow: func() time.Time { return now.Add(42 * time.Second) }}
	_, fs = checkCore(context.Background(), opts)
	if f := find(t, fs, "core", "migrations"); f.Status != Fail || !strings.Contains(f.Message, "2 migration(s) pending") {
		t.Errorf("migrations finding = %v", f)
	}
	if f := find(t, fs, "core", "clock"); f.Status != Fail || !strings.Contains(f.Message, "42s") {
		t.Errorf("clock finding = %v", f)
	}
	if f := find(t, fs, "postgres", "version"); f.Status != Fail || !strings.Contains(f.Message, "17.4") {
		t.Errorf("postgres finding = %v", f)
	}

	opts.Database = fakeDB{pingErr: errors.New("connection refused"), dbNow: time.Now}
	reports, fs = checkCore(context.Background(), opts)
	if f := find(t, fs, "core", "database"); f.Status != Fail || reports[0].Reachable {
		t.Errorf("database finding = %v", f)
	}
}

func TestRunAggregatesAndRenders(t *testing.T) {
	kc := keycloakServer(t, "26.8.0", "zs-reference")
	kb := killBillServer(t, "0.24.22", true, []map[string]any{platformBundle})
	now := time.Now()
	opts := Options{
		Manifest:   manifest(),
		Database:   fakeDB{version: "18.1", dbNow: func() time.Time { return now }},
		Keycloak:   Target{URL: kc.URL, AdminUser: "admin", AdminPassword: "pw", Realm: "zs-reference"},
		KillBill:   Target{URL: kb.URL, AdminUser: "admin", AdminPassword: "pw"},
		HTTPClient: http.DefaultClient,
		Now:        func() time.Time { return now },
	}
	report := Run(context.Background(), opts)
	if report.OK || !report.Failed() {
		t.Fatal("a Keycloak mismatch must fail the run")
	}
	var text bytes.Buffer
	if err := report.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(text.String()), "\n")
	if len(lines) != len(report.Findings) {
		t.Fatalf("%d lines for %d findings", len(lines), len(report.Findings))
	}
	if !strings.Contains(text.String(), "FAIL keycloak: version 26.8.0 does not match the pin 26.8.1") {
		t.Fatalf("text output:\n%s", text.String())
	}

	var js bytes.Buffer
	if err := report.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OK || decoded.CoreRelease != "1.0.0-test" || len(decoded.Components) != 7 {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestSkippedChecksAreReportedAndNotOK(t *testing.T) {
	kc := keycloakServer(t, "26.8.1")
	kb := killBillServer(t, "0.24.22", true, []map[string]any{platformBundle, runningStripe})
	now := time.Now()
	report := Run(context.Background(), Options{
		Manifest:   manifest(),
		Database:   fakeDB{version: "18.1", dbNow: func() time.Time { return now }},
		Keycloak:   Target{URL: kc.URL},
		KillBill:   Target{URL: kb.URL},
		HTTPClient: http.DefaultClient,
		Now:        func() time.Time { return now },
	})
	if report.Failed() || !report.Skipped() || report.OK {
		t.Fatalf("failed=%v skipped=%v ok=%v: %v", report.Failed(), report.Skipped(), report.OK, report.Findings)
	}
	var text bytes.Buffer
	if err := report.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"SKIP keycloak: realm not checked",
		"SKIP keycloak: version not checked (no admin credentials)",
		"SKIP killbill: version not checked (no admin credentials)",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("missing %q in:\n%s", want, text.String())
		}
	}
}
