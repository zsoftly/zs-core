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
			ApprovedPaymentPlugins: []components.Plugin{{Name: "stripe", Version: "6.0.1"}},
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
	if f := find(t, fs, "keycloak", "version"); f.Status != Warn || !strings.Contains(f.Message, "ZS_KEYCLOAK_ADMIN_USER") {
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
		_ = json.NewEncoder(w).Encode([]map[string]any{{"kbVersion": kbVersion, "pluginsInfo": plugins}})
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

func TestKillBillMatchesPin(t *testing.T) {
	srv := killBillServer(t, "0.24.22", true, []map[string]any{platformBundle})
	opts := Options{Manifest: manifest(), HTTPClient: srv.Client(),
		KillBill: Target{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"}}
	report, fs := checkKillBill(context.Background(), opts)
	if report.Version != "0.24.22" || report.MatchesPin == nil || !*report.MatchesPin || !report.Reachable {
		t.Fatalf("report = %+v", report)
	}
	for _, check := range []string{"health", "version", "plugins"} {
		if f := find(t, fs, "killbill", check); f.Status != OK {
			t.Errorf("%s finding = %v", check, f)
		}
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
	var stripeFail, customWarn bool
	for _, f := range fs {
		if f.Check == "plugin" && f.Status == Fail && strings.Contains(f.Message, "stripe") && strings.Contains(f.Message, "6.0.1") {
			stripeFail = true
		}
		if f.Check == "plugin" && f.Status == Warn && strings.Contains(f.Message, "custom") {
			customWarn = true
		}
	}
	if !stripeFail || !customWarn {
		t.Fatalf("plugin findings missing (stripeFail=%v customWarn=%v): %v", stripeFail, customWarn, fs)
	}
	if len(report.Plugins) != 3 {
		t.Fatalf("plugins = %+v", report.Plugins)
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
	if f := find(t, fs, "killbill", "version"); f.Status != Warn {
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
	if decoded.OK || decoded.CoreRelease != "1.0.0-test" || len(decoded.Components) != 5 {
		t.Fatalf("decoded = %+v", decoded)
	}
}
