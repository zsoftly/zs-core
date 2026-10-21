//go:build integration

package stacktest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/kbplugin"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
)

// Payment plugin deployment and the plugin mirror on Kill Bill 0.24.22.
// The test starts Stripe from scratch: it uninstalls the plugin and drops
// its tables. Then: brand apply refuses Stripe while its tables are
// missing; zs-core's DDL step creates them once; a caller pin that differs
// from the mirror's serving pin is refused and audited; Stripe is installed
// from Core's mirror
// while the repository is unreachable and its configuration uploaded;
// doctor is green with the table check; a dropped table makes doctor FAIL
// and brand apply refuse; the DDL step repairs it. A pin that differs from
// the installed version fails doctor and brand apply, and the mirror route
// refuses what it must refuse.
func TestPluginDeploymentOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()
	kbDB := killBillDB(t)
	m := pins(t)
	stripe := pin(t, m, "stripe")
	tables := tablesIn(kbDB)
	gated := deps
	gated.PluginTables = tables
	// This test installs from scratch, so the mirror must be configured
	// before either the missing-table or mismatched-pin refusal is checked.
	if gated.PluginMirrorURL == "" || len(gated.PluginMirrorSecret) == 0 {
		t.Fatal("plugin deployment needs ZS_TEST_PLUGIN_MIRROR_URL and ZS_TEST_PLUGIN_MIRROR_SECRET")
	}

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	second := secondManifest(t, ref)

	// A fresh start for Stripe: not installed, no tables, no recorded DDL.
	uninstallPlugin(t, s, stripe)
	for _, table := range stripe.Tables {
		if _, err := kbDB.Exec(ctx, `DROP TABLE IF EXISTS `+table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := brand.Apply(ctx, gated, second, cli); errName(err) != "plugin_tables_missing" || !strings.Contains(err.Error(), "zs-core plugin ddl apply stripe") {
		t.Fatalf("apply before the DDL: %v", err)
	}
	if v, _ := pluginOnNodes(t, s, "stripe"); v != "" {
		t.Fatal("Stripe was installed although its tables are missing")
	}

	// The DDL step, as zs-core plugin ddl apply runs it: the jar comes from
	// the repository and is checked against components.yaml.
	src := kbplugin.Source{Repository: m.KillBillPlugins.Repository}
	jar, _, err := src.Jar(ctx, stripe)
	if err != nil {
		t.Fatal(err)
	}
	d, err := kbplugin.ExtractDDL(jar, stripe)
	if err != nil {
		t.Fatal(err)
	}
	res, err := kbplugin.Apply(ctx, kbDB, db.Pool, d, stripe, kbplugin.ApplyOptions{AppliedBy: "zs-core-cli:stack-test"})
	if err != nil || strings.Join(res.Created, ",") != strings.Join(stripe.Tables, ",") {
		t.Fatalf("ddl apply = %+v %v", res, err)
	}
	if res, err := kbplugin.Apply(ctx, kbDB, db.Pool, d, stripe, kbplugin.ApplyOptions{AppliedBy: "zs-core-cli:stack-test"}); err != nil || res.Applied() {
		t.Fatalf("second ddl apply = %+v %v", res, err)
	}

	// A caller pin that differs from the serving mirror pin is refused
	// before KPM sees a jar. The mirror returns 404 before reading bytes.
	tampered := gated
	tampered.PaymentPlugins = append([]components.Plugin(nil), deps.PaymentPlugins...)
	for i := range tampered.PaymentPlugins {
		if tampered.PaymentPlugins[i].Name == "stripe" {
			tampered.PaymentPlugins[i].Sha256 = strings.Repeat("0", 64)
		}
	}
	_, err = brand.Apply(ctx, tampered, second, cli)
	var unavailable *killbill.Unavailable
	if errName(err) != "billing_unavailable" || !errors.As(err, &unavailable) || unavailable.Status != http.StatusNotFound {
		t.Fatalf("mismatched mirror pin: %v", err)
	}
	if v, _ := pluginOnNodes(t, s, "stripe"); v != "" {
		t.Fatal("a plugin whose caller pin differs from the mirror was installed")
	}
	rec, err := brand.GetBySlug(ctx, db.Pool, "zs-second")
	if err != nil {
		t.Fatal(err)
	}
	audited := false
	for _, e := range mustAllEvents(t, db, rec.ID) {
		audited = audited || (e.Action == "brand.killbill.plugin.created" && e.Outcome == audit.OutcomeFailure && e.Details["error"] == "billing_unavailable")
	}
	if !audited {
		t.Fatal("the mirror pin refusal is not audited")
	}

	// Stripe from Core's mirror, with the repository unreachable: KPM gets
	// the jar from the core container, which checks it before it serves it.
	mirrored := gated
	mirrored.PluginRepository = "https://127.0.0.1:9/maven2"
	diff := apply(t, mirrored, second)
	var source any
	for _, c := range diff.Changes {
		if c.Target == brand.TargetPlugin {
			for _, f := range c.Fields {
				if strings.HasSuffix(f.Field, ".source") {
					source = f.After
				}
			}
		}
	}
	if source != "mirror" {
		t.Fatalf("install source = %v in %s", source, mustJSON(diff))
	}
	if v, state := pluginOnNodes(t, s, "stripe"); v != stripe.Version || state != "RUNNING" {
		t.Fatalf("Stripe after the mirror install = %s %s", v, state)
	}
	if cfg, found, err := s.kb.PluginConfig(ctx, killbill.TenantCredentials{APIKey: "zs-second", APISecret: secondKillBillSecret}, "killbill-stripe"); err != nil || !found || cfg != secondPluginConfig {
		t.Fatalf("Stripe configuration of zs-second: found %v %v", found, err)
	}
	if again := apply(t, mirrored, second); len(again.Changes) != 0 {
		t.Fatalf("second apply = %s", mustJSON(again))
	}

	// Doctor is green for Kill Bill, with the table check.
	fs := killBillFindings(t, m, tables)
	for _, f := range fs {
		if f.Status == doctor.Fail || f.Status == doctor.Skip {
			t.Fatalf("doctor after the install:\n%s", findingLines(fs))
		}
	}
	if !strings.Contains(findingLines(fs), "OK   killbill: plugin stripe 8.0.3 tables present") {
		t.Fatalf("doctor has no Stripe table check:\n%s", findingLines(fs))
	}

	// A dropped table: doctor fails and brand apply refuses.
	if _, err := kbDB.Exec(ctx, `DROP TABLE stripe_responses`); err != nil {
		t.Fatal(err)
	}
	fs = killBillFindings(t, m, tables)
	if !strings.Contains(findingLines(fs), "FAIL killbill: plugin stripe 8.0.3 tables missing: stripe_responses") {
		t.Fatalf("doctor after the drop:\n%s", findingLines(fs))
	}
	if _, err := brand.Apply(ctx, mirrored, second, cli); errName(err) != "plugin_tables_missing" {
		t.Fatalf("apply after the drop: %v", err)
	}
	if res, err := kbplugin.Apply(ctx, kbDB, db.Pool, d, stripe, kbplugin.ApplyOptions{AppliedBy: "zs-core-cli:stack-test"}); err != nil || strings.Join(res.Created, ",") != "stripe_responses" {
		t.Fatalf("repair = %+v %v", res, err)
	}
	if missing, err := tables(ctx, stripe); err != nil || len(missing) != 0 {
		t.Fatalf("tables after the repair = %v %v", missing, err)
	}

	// A pin other than the installed version fails doctor and brand apply.
	other := m
	other.KillBillPlugins.ApprovedPaymentPlugins = append([]components.Plugin(nil), m.KillBillPlugins.ApprovedPaymentPlugins...)
	for i := range other.KillBillPlugins.ApprovedPaymentPlugins {
		if other.KillBillPlugins.ApprovedPaymentPlugins[i].Name == "stripe" {
			other.KillBillPlugins.ApprovedPaymentPlugins[i].Version = "8.0.2"
		}
	}
	if fs := killBillFindings(t, other, tables); !strings.Contains(findingLines(fs), "FAIL killbill: plugin stripe version 8.0.3 does not match the pin 8.0.2") {
		t.Fatalf("doctor with another pin:\n%s", findingLines(fs))
	}
	repinned := gated
	repinned.PaymentPlugins = other.KillBillPlugins.ApprovedPaymentPlugins
	if _, err := brand.Apply(ctx, repinned, second, cli); errName(err) != "brand_apply_rejected" || !strings.Contains(err.Error(), "but components.yaml pins 8.0.2") {
		t.Fatalf("apply with another pin: %v", err)
	}

	// A plugin outside the approved lists is refused.
	unknown := clone(t, second)
	unknown.Billing.PaymentPlugin.Name = "hello-world"
	if _, err := brand.Apply(ctx, gated, unknown, cli); errName(err) != "plugin_unavailable" {
		t.Fatalf("plugin outside the approved list: %v", err)
	}

	// The mirror route of the core container.
	core := strings.TrimRight(env(t, envCoreURL), "/")
	secret := env(t, envMirrorSecret)
	get := func(path, bearer string) (int, []byte) {
		req, err := http.NewRequest(http.MethodGet, core+path, nil)
		if err != nil {
			t.Fatal("invalid mirror request URL")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("mirror request for %s failed", req.URL.Path)
		}
		defer resp.Body.Close()
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", req.URL.Path, resp.Header.Get("Cache-Control"))
		}
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}
	jarPath := "/v1/plugins/stripe/8.0.3.jar?sha256=" + stripe.Sha256
	if status, _ := get(jarPath, ""); status != http.StatusUnauthorized {
		t.Fatalf("mirror without a credential = %d", status)
	}
	if status, _ := get(jarPath+"&token="+kbplugin.Token([]byte(secret), "stripe", "8.0.2", stripe.Sha256), ""); status != http.StatusUnauthorized {
		t.Fatalf("token of another version = %d", status)
	}
	status, body := get(jarPath+"&token="+kbplugin.Token([]byte(secret), "stripe", "8.0.3", stripe.Sha256), "")
	sum := sha256.Sum256(body)
	if status != http.StatusOK || hex.EncodeToString(sum[:]) != stripe.Sha256 {
		t.Fatalf("mirror jar = %d, sha256 %x", status, sum)
	}
	staleDigest := strings.Repeat("0", 64)
	stalePath := "/v1/plugins/stripe/8.0.3.jar?sha256=" + staleDigest + "&token=" + kbplugin.Token([]byte(secret), "stripe", "8.0.3", staleDigest)
	if status, _ := get(stalePath, ""); status != http.StatusNotFound {
		t.Fatalf("authenticated download with a different serving pin = %d", status)
	}
	for _, path := range []string{"/v1/plugins/stripe/8.0.2.jar", "/v1/plugins/hello-world/1.0.0.jar", "/v1/plugins/paypal-express/6.0.0.jar"} {
		if status, _ := get(path+"?sha256="+stripe.Sha256, secret); status != http.StatusNotFound {
			t.Fatalf("%s = %d", path, status)
		}
	}
}

// KPM uses the Compose network address, while the test process reaches the
// same Core server through its published address. Only preflight requests
// use this adapter; the URL sent to KPM keeps its original authority/token.
type mirrorEndpointTransport struct {
	mirror, core *url.URL
	next         http.RoundTripper
}

func (m mirrorEndpointTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	prefix := strings.TrimRight(m.mirror.Path, "/")
	if r.URL.Scheme != m.mirror.Scheme || r.URL.Host != m.mirror.Host || !strings.HasPrefix(r.URL.Path, prefix+"/") {
		return nil, errors.New("preflight request is outside the configured mirror")
	}
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Scheme, u.Host = m.core.Scheme, m.core.Host
	u.Path = strings.TrimRight(m.core.Path, "/") + strings.TrimPrefix(u.Path, prefix)
	u.RawPath = ""
	copy.URL, copy.Host = &u, ""
	return m.next.RoundTrip(copy)
}

func mirrorPreflightClient(t *testing.T, mirrorURL, coreURL string) *http.Client {
	t.Helper()
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
			t.Fatal("stack mirror and Core addresses must be HTTP(S) bases without credentials, queries or fragments")
		}
		return u
	}
	return &http.Client{Transport: mirrorEndpointTransport{mirror: parse(mirrorURL), core: parse(coreURL), next: http.DefaultTransport}, Timeout: 30 * time.Second}
}

// This fixture check uses HTTP only; it never calls newStack or Postgres.
func TestMirrorPreflightPreservesAuthenticatedDownloadURL(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.RequestURI()
		_, _ = w.Write([]byte("verified jar"))
	}))
	t.Cleanup(srv.Close)
	client := mirrorPreflightClient(t, "http://core:8080/mirror", srv.URL+"/published")
	uri := "http://core:8080/mirror/v1/plugins/stripe/8.0.3.jar?sha256=pinned&token=bound-token"
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "verified jar" {
		t.Fatalf("preflight response = %d, %q, %v", response.StatusCode, body, err)
	}
	if got := <-seen; got != "/published/v1/plugins/stripe/8.0.3.jar?sha256=pinned&token=bound-token" {
		t.Fatalf("mapped preflight route lost its digest or credential: %q", got)
	}
	if req.URL.String() != uri {
		t.Fatal("preflight changed the authenticated URL KPM must use")
	}
	for _, outside := range []string{"http://repository.invalid/mirror/v1/plugins/stripe/8.0.3.jar", "http://core:8080/mirror-other/v1/plugins/stripe/8.0.3.jar"} {
		req, err := http.NewRequest(http.MethodGet, outside, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Do(req); err == nil {
			t.Fatal("preflight accepted an address outside the mirror")
		}
	}
	select {
	case route := <-seen:
		t.Fatalf("an outside-mirror request reached Core: %s", route)
	default:
	}
}
