package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/kbplugin"
)

func TestPluginMirrorRoute(t *testing.T) {
	jar := []byte("stand-in jar")
	sum := sha256.Sum256(jar)
	pin := components.Plugin{Name: "stripe", Version: "8.0.3", Artifact: "g:stripe-plugin", Sha256: hex.EncodeToString(sum[:])}
	repo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(jar) }))
	t.Cleanup(repo.Close)
	dir := t.TempDir()
	m := components.Manifest{KillBillPlugins: components.KillBillPlugins{Repository: repo.URL, ApprovedPaymentPlugins: []components.Plugin{pin}}}
	secret := []byte(strings.Repeat("m", 32))
	jarPath := "/v1/plugins/stripe/8.0.3.jar"
	jarURL := jarPath + "?sha256=" + pin.Sha256
	if _, err := kbplugin.Sync(context.Background(), dir, secret, m, repo.Client()); err != nil {
		t.Fatal(err)
	}
	get := func(h http.Handler, path, auth string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Body)
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", path, rec.Header().Get("Cache-Control"))
		}
		return rec.Code, string(body)
	}
	if code, _ := get(New(Options{}), jarURL, "Bearer "+string(secret)); code != http.StatusNotFound {
		t.Fatalf("without a mirror = %d", code)
	}
	h := New(Options{PluginMirror: &kbplugin.Mirror{Dir: dir, Manifest: m, Secret: secret}, PluginMirrorSecret: secret})
	if code, body := get(h, jarURL, ""); code != http.StatusUnauthorized || !strings.Contains(body, "unauthenticated") {
		t.Fatalf("without a credential = %d %s", code, body)
	}
	if code, _ := get(h, jarURL, "Bearer wrong"); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret = %d", code)
	}
	if code, body := get(h, jarURL, "Bearer "+string(secret)); code != http.StatusOK || body != string(jar) {
		t.Fatalf("bearer = %d %q", code, body)
	}
	tok := kbplugin.Token(secret, "stripe", "8.0.3", pin.Sha256)
	if code, body := get(h, jarURL+"&token="+tok, ""); code != http.StatusOK || body != string(jar) {
		t.Fatalf("token = %d %q", code, body)
	}
	// A token opens its own jar only.
	if code, _ := get(h, "/v1/plugins/stripe/8.0.4.jar?sha256="+pin.Sha256+"&token="+tok, ""); code != http.StatusUnauthorized {
		t.Fatalf("token of another version = %d", code)
	}
	if code, _ := get(h, "/v1/plugins/stripe/8.0.4.jar?sha256="+pin.Sha256, "Bearer "+string(secret)); code != http.StatusNotFound {
		t.Fatalf("version that is not pinned = %d", code)
	}
	if code, _ := get(h, "/v1/plugins/stripe/8.0.3.zip", "Bearer "+string(secret)); code != http.StatusNotFound {
		t.Fatalf("not a jar = %d", code)
	}
	wrongDigest := strings.Repeat("0", 64)
	for _, tc := range []struct {
		name, path, auth string
		want             int
	}{
		{"bearer without digest", jarPath, "Bearer " + string(secret), http.StatusNotFound},
		{"bearer with wrong digest", jarPath + "?sha256=" + wrongDigest, "Bearer " + string(secret), http.StatusNotFound},
		{"duplicate digest", jarURL + "&sha256=" + pin.Sha256, "Bearer " + string(secret), http.StatusNotFound},
		{"token without digest", jarPath + "?token=" + tok, "", http.StatusUnauthorized},
		{"token with changed digest", jarPath + "?sha256=" + wrongDigest + "&token=" + tok, "", http.StatusUnauthorized},
		{"token for another digest", jarURL + "&token=" + kbplugin.Token(secret, "stripe", "8.0.3", wrongDigest), "", http.StatusUnauthorized},
		{"valid token for stale pin", jarPath + "?sha256=" + wrongDigest + "&token=" + kbplugin.Token(secret, "stripe", "8.0.3", wrongDigest), "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := get(h, tc.path, tc.auth); code != tc.want || body == string(jar) {
				t.Fatalf("response = %d %q, want status %d without jar bytes", code, body, tc.want)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(dir, kbplugin.FileName(pin)), []byte("tampered jar bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := get(h, jarURL, "Bearer "+string(secret)); code != http.StatusInternalServerError || strings.Contains(body, "tampered") {
		t.Fatalf("tampered jar = %d %s", code, body)
	}
}
