package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/kbplugin"
)

func tablesManifest() components.Manifest {
	m := manifest()
	m.KillBillPlugins.ApprovedPaymentPlugins[0].Tables = []string{"stripe_hpp_requests", "stripe_responses"}
	m.KillBillPlugins.Installable = []components.Plugin{{Name: "email-notifications", Version: "0.8.1", Status: components.PluginStatusPinnedNotInstalled, Tables: []string{"email_notifications_configuration"}}}
	return m
}

func TestPluginTablesChecksInstalledAndRequiredPlugins(t *testing.T) {
	missing := map[string][]string{}
	var asked []string
	opts := Options{Manifest: tablesManifest(), PluginTables: func(_ context.Context, p components.Plugin) ([]string, error) {
		asked = append(asked, p.Name)
		return missing[p.Name], nil
	}}
	// Stripe is required; email-notifications counts only once installed.
	fs := checkPluginTables(context.Background(), opts, map[string]bool{})
	if strings.Join(asked, ",") != "stripe" || len(fs) != 1 || fs[0].Status != OK || !strings.Contains(fs[0].Message, "plugin stripe 6.0.1 tables present") {
		t.Fatalf("findings = %v, asked %v", fs, asked)
	}
	asked = nil
	missing["stripe"] = []string{"stripe_responses"}
	fs = checkPluginTables(context.Background(), opts, map[string]bool{"email-notifications": true})
	if strings.Join(asked, ",") != "stripe,email-notifications" || fs[0].Status != Fail || fs[1].Status != OK {
		t.Fatalf("findings = %v, asked %v", fs, asked)
	}
	if got := fs[0].String(); !strings.HasPrefix(got, "FAIL killbill: plugin stripe 6.0.1 tables missing: stripe_responses") {
		t.Fatalf("line = %q", got)
	}
	opts.PluginTables = func(context.Context, components.Plugin) ([]string, error) {
		return nil, errors.New("connection refused")
	}
	if fs := checkPluginTables(context.Background(), opts, nil); fs[0].Status != Fail || !strings.Contains(fs[0].Message, "not readable") {
		t.Fatalf("unreadable database = %v", fs)
	}
	opts.PluginTables = nil
	if fs := checkPluginTables(context.Background(), opts, nil); len(fs) != 1 || fs[0].Status != Skip {
		t.Fatalf("without the database = %v", fs)
	}
}

func TestMirrorState(t *testing.T) {
	jar := []byte("a jar")
	sum := sha256.Sum256(jar)
	m := components.Manifest{KillBillPlugins: components.KillBillPlugins{
		Repository: "https://repo.example.test/maven2",
		ApprovedPaymentPlugins: []components.Plugin{
			{Name: "stripe", Version: "8.0.3", Artifact: "g:stripe-plugin", Sha256: hex.EncodeToString(sum[:])},
			{Name: "adyen", Version: "0.10.0", Artifact: "g:adyen-plugin", Sha256: strings.Repeat("a", 64)},
		},
	}}
	if _, fs := checkMirror(Options{Manifest: m}); len(fs) != 1 || fs[0].Status != OK || !strings.Contains(fs[0].Message, "not configured") {
		t.Fatalf("no mirror = %v", fs)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stripe-8.0.3.jar"), jar, 0o600); err != nil {
		t.Fatal(err)
	}
	_, fs := checkMirror(Options{Manifest: m, Mirror: MirrorOptions{Dir: dir}})
	statuses := map[string]Status{}
	for _, f := range fs {
		statuses[strings.SplitN(f.Message, " ", 3)[1]] = f.Status
	}
	if statuses["adyen"] != Warn || statuses["stripe"] != OK {
		t.Fatalf("partial mirror = %v", fs)
	}
	// Installs that use the mirror fail on a missing jar, and a jar that
	// does not match its pin always fails.
	if err := os.WriteFile(filepath.Join(dir, "adyen-0.10.0.jar"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fs = checkMirror(Options{Manifest: m, Mirror: MirrorOptions{Dir: dir, URL: "http://core:8080"}})
	failed := false
	for _, f := range fs {
		failed = failed || (f.Status == Fail && strings.Contains(f.Message, "adyen 0.10.0 does not match"))
	}
	if !failed {
		t.Fatalf("mismatched jar = %v", fs)
	}
	if err := os.Remove(filepath.Join(dir, "adyen-0.10.0.jar")); err != nil {
		t.Fatal(err)
	}
	_, fs = checkMirror(Options{Manifest: m, Mirror: MirrorOptions{Dir: dir, URL: "http://core:8080"}})
	failed = false
	for _, f := range fs {
		failed = failed || (f.Status == Fail && strings.Contains(f.Message, "adyen 0.10.0 is missing"))
	}
	if !failed {
		t.Fatalf("missing jar with installs from the mirror = %v", fs)
	}
	// The manifest's signature is checked when the secret is known.
	if _, err := kbplugin.ReadManifest(dir); err == nil {
		t.Fatal("no manifest was written")
	}
	_, fs = checkMirror(Options{Manifest: m, Mirror: MirrorOptions{Dir: dir, Secret: []byte(strings.Repeat("s", 32))}})
	if f := fs[0]; f.Status != Warn || !strings.Contains(f.Message, "not readable") {
		t.Fatalf("missing manifest = %v", fs)
	}
	if err := os.WriteFile(filepath.Join(dir, kbplugin.ManifestFile), []byte(`{"plugins":[],"signature":"00"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fs = checkMirror(Options{Manifest: m, Mirror: MirrorOptions{Dir: dir, Secret: []byte(strings.Repeat("s", 32))}})
	if f := fs[0]; f.Status != Fail || !strings.Contains(f.Message, "not signed") {
		t.Fatalf("forged manifest = %v", fs)
	}
}

func TestPDFRendererVersion(t *testing.T) {
	m := manifest()
	m.Components[components.Gotenberg] = components.Component{Version: "8.38.1", Image: "gotenberg/gotenberg:8.38.1", Optional: true}
	if _, fs := checkPDFRenderer(context.Background(), Options{Manifest: m}); fs[0].Status != OK || !strings.Contains(fs[0].Message, "not configured") {
		t.Fatalf("no renderer = %v", fs)
	}
	version := "8.38.1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(version + "\n"))
	}))
	t.Cleanup(srv.Close)
	report, fs := checkPDFRenderer(context.Background(), Options{Manifest: m, PDFRendererURL: srv.URL, HTTPClient: srv.Client()})
	if fs[0].Status != OK || report.Version != "8.38.1" {
		t.Fatalf("renderer = %+v %v", report, fs)
	}
	version = "8.37.0"
	if _, fs := checkPDFRenderer(context.Background(), Options{Manifest: m, PDFRendererURL: srv.URL, HTTPClient: srv.Client()}); fs[0].Status != Fail {
		t.Fatalf("renderer at another version = %v", fs)
	}
	srv.Close()
	if _, fs := checkPDFRenderer(context.Background(), Options{Manifest: m, PDFRendererURL: srv.URL, HTTPClient: srv.Client()}); fs[0].Status != Fail {
		t.Fatalf("unreachable renderer = %v", fs)
	}
}
