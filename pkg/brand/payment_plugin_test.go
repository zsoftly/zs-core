package brand

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/internal/components"
)

// javaCase is one row of testdata/java_properties.json: how
// java.util.Properties.load in Kill Bill's own JVM read a text.
type javaCase struct {
	In    string            `json:"in"`
	Props map[string]string `json:"props"`
	Error bool              `json:"error"`
}

func javaCases(t testing.TB) []javaCase {
	data, err := os.ReadFile("testdata/java_properties.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []javaCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// The parser reads every text of the reference table as Java does.
func TestParsePropertiesMatchesJava(t *testing.T) {
	cases := javaCases(t)
	if len(cases) < 50 {
		t.Fatalf("reference table has %d cases", len(cases))
	}
	for _, c := range cases {
		p, err := parseProperties(c.In)
		if c.Error {
			if err == nil {
				t.Errorf("%q: Java refuses it, the parser read %v", c.In, p.last)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.In, err)
			continue
		}
		if !maps.Equal(p.last, c.Props) {
			t.Errorf("%q = %q, Java reads %q", c.In, p.last, c.Props)
		}
	}
}

// simpleParse is an independent reader for the texts checkPluginConfigText
// accepts: ASCII lines that end with \n or \r\n, without continuations or
// \u escapes. The fuzz test holds parseProperties to it.
func simpleParse(text string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimLeft(strings.TrimSuffix(raw, "\r"), " \t\f")
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		var key, value strings.Builder
		i, sep := 0, false
		for ; i < len(line); i++ {
			c := line[i]
			if c == '\\' && i+1 < len(line) {
				i++
				key.WriteByte(simpleEscape(line[i]))
				continue
			}
			if c == '=' || c == ':' {
				sep = true
				i++
				break
			}
			if c == ' ' || c == '\t' || c == '\f' {
				i++
				break
			}
			key.WriteByte(c)
		}
		for ; i < len(line); i++ {
			c := line[i]
			if c == ' ' || c == '\t' || c == '\f' {
				continue
			}
			if !sep && (c == '=' || c == ':') {
				sep = true
				continue
			}
			break
		}
		for ; i < len(line); i++ {
			if line[i] == '\\' && i+1 < len(line) {
				i++
				value.WriteByte(simpleEscape(line[i]))
				continue
			}
			value.WriteByte(line[i])
		}
		out[key.String()] = value.String()
	}
	return out
}

func simpleEscape(c byte) byte {
	switch c {
	case 't':
		return '\t'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 'f':
		return '\f'
	}
	return c
}

// For any text Core accepts, the parser and the independent reader agree,
// and a Stripe configuration Core accepts holds only test-mode keys as Kill
// Bill reads them.
func FuzzParseProperties(f *testing.F) {
	for _, c := range javaCases(f) {
		f.Add(c.In)
	}
	f.Fuzz(func(t *testing.T, text string) {
		p, err := parseProperties(text)
		if checkPluginConfigText(text) != nil {
			return
		}
		if err != nil {
			t.Fatalf("%q: accepted text not parsed: %v", text, err)
		}
		if want := simpleParse(text); !maps.Equal(p.last, want) {
			t.Fatalf("%q = %q, the simple reader reads %q", text, p.last, want)
		}
		if CheckTestMode("stripe", text) == nil && !strings.HasPrefix(p.last[stripeAPIKey], stripeTestKeyPrefix) {
			t.Fatalf("%q: accepted with Stripe key %q", text, p.last[stripeAPIKey])
		}
		if CheckTestMode("adyen", text) == nil && p.last[adyenEnvironmentKey] != adyenTestEnvironment {
			t.Fatalf("%q: accepted with Adyen environment %q", text, p.last[adyenEnvironmentKey])
		}
	})
}

const (
	testStripe = "org.killbill.billing.plugin.stripe.apiKey=sk_test_123"
	testAdyen  = "org.killbill.billing.plugin.adyen.enviroment=TEST"
)

func TestCheckTestMode(t *testing.T) {
	for _, c := range []struct {
		plugin, config, want, rule string
	}{
		{"stripe", testStripe, "", ""},
		{"stripe", testStripe + "\r\nother=1\r\n", "", ""},
		{"stripe", "org.killbill.billing.plugin.stripe.apiKey=sk_live_123", "brand_apply_rejected", "not a test-mode key"},
		{"stripe", "org.killbill.billing.plugin.stripe.apiKey=rk_live_123", "brand_apply_rejected", "not a test-mode key"},
		{"stripe", testStripe + "\norg.killbill.billing.plugin.stripe.apiKey=sk_live_2", "brand_apply_rejected", "not a test-mode key"},
		{"stripe", "org.killbill.billing.plugin.stripe.apiKey : sk_live_123", "brand_apply_rejected", "not a test-mode key"},
		// No key at all: an environment-provided key cannot be relied on.
		{"stripe", "", "brand_apply_rejected", "must set"},
		{"stripe", "other=1", "brand_apply_rejected", "must set"},
		{"adyen", testAdyen, "", ""},
		{"adyen", testAdyen + "\norg.killbill.billing.plugin.adyen.environment=TEST", "", ""},
		// Unset means whatever the Kill Bill node's environment says.
		{"adyen", "org.killbill.billing.plugin.adyen.apiKey=x", "brand_apply_rejected", "must set"},
		{"adyen", "org.killbill.billing.plugin.adyen.environment=TEST", "brand_apply_rejected", "must set"},
		{"adyen", "org.killbill.billing.plugin.adyen.enviroment=LIVE", "brand_apply_rejected", "other than TEST"},
		{"adyen", testAdyen + "\norg.killbill.billing.plugin.adyen.environment=LIVE", "brand_apply_rejected", "other than TEST"},
		{"adyen", "org.killbill.billing.plugin.adyen.enviroment=test", "brand_apply_rejected", "other than TEST"},
		// Every known plugin's rule runs against every configuration.
		{"adyen", testAdyen + "\norg.killbill.billing.plugin.stripe.apiKey=sk_live_123", "brand_apply_rejected", "Stripe API key that is not a test-mode key"},
		{"stripe", testStripe + "\norg.killbill.billing.plugin.adyen.enviroment=LIVE", "brand_apply_rejected", "Adyen environment other than TEST"},
		{"paypal-express", "x=y", "plugin_unavailable", ""},
		{"custom", "x=y", "plugin_unavailable", ""},
	} {
		err := CheckTestMode(c.plugin, c.config)
		got := ""
		if err != nil {
			got = codeOf(err)
			if strings.Contains(messageOf(err), "_123") || strings.Contains(messageOf(err), "LIVE") {
				t.Errorf("%s %q: the message holds the configuration: %s", c.plugin, c.config, messageOf(err))
			}
			if !strings.Contains(messageOf(err), c.rule) {
				t.Errorf("%s %q: message %q does not name %q", c.plugin, c.config, messageOf(err), c.rule)
			}
		}
		if got != c.want {
			t.Errorf("%s %q = %q (%v), want %q", c.plugin, c.config, got, err, c.want)
		}
	}
}

// The texts that got a live key past the first parser are refused. Each
// would reach Kill Bill as a live key or a live environment, as the Java
// table shows.
func TestTestModeBypassesAreRefused(t *testing.T) {
	for _, c := range []struct {
		plugin, config, rule string
	}{
		{"stripe", testStripe + "\norg.killbill.billing.plugin.stripe.api\\u004bey=sk_live_b", `\u escape`},
		{"stripe", testStripe + "\rorg.killbill.billing.plugin.stripe.apiKey=sk_live_b", "carriage return"},
		{"adyen", testAdyen + "\norg.killbill.billing.plugin.adyen.envir\\u006fment=LIVE", `\u escape`},
		{"adyen", testAdyen + "\n\rorg.killbill.billing.plugin.adyen.enviroment=LIVE", "carriage return"},
		{"stripe", testStripe + "\\\n  9", "continues a line"},
		{"stripe", "org.killbill.billing.plugin.stripe.apiKey=sk_test_\u00e9", "not ASCII"},
	} {
		err := CheckTestMode(c.plugin, c.config)
		if codeOf(err) != "brand_apply_rejected" || !strings.Contains(messageOf(err), c.rule) {
			t.Errorf("%s %q = %v, want brand_apply_rejected naming %q", c.plugin, c.config, err, c.rule)
		}
	}
	// The table confirms what Java reads from the first four.
	live := 0
	for _, c := range javaCases(t) {
		if v := c.Props[stripeAPIKey]; strings.HasPrefix(v, "sk_live_") && strings.Contains(c.In, "sk_test_") {
			live++
		}
		if c.Props[adyenEnvironmentKey] == "LIVE" {
			live++
		}
	}
	if live < 4 {
		t.Fatalf("the reference table shows %d live readings, want at least 4", live)
	}
}

// config_key is the plugin's own service name or empty, in both directions
// of a swap.
func TestConfigKeyMustBeThePluginsOwn(t *testing.T) {
	base := referenceYAML(t) + "  payment_plugin:\n    mode: test\n    config_ref: env://BRAND_ZS_REFERENCE__PLUGIN\n"
	for _, c := range []struct {
		name, key string
		ok        bool
	}{
		{"stripe", "", true},
		{"stripe", "killbill-stripe", true},
		{"adyen", "killbill-adyen", true},
		{"stripe", "killbill-adyen", false},
		{"adyen", "killbill-stripe", false},
		{"stripe", "killbill-stripe-live", false},
	} {
		doc := base + "    name: " + c.name + "\n"
		if c.key != "" {
			doc += "    config_key: " + c.key + "\n"
		}
		m, err := Parse([]byte(doc))
		if c.ok {
			if err != nil || m.Billing.PaymentPlugin.Key() != "killbill-"+c.name {
				t.Errorf("%s under %q = %v", c.name, c.key, err)
			}
			continue
		}
		if codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), "/billing/payment_plugin/config_key") {
			t.Errorf("%s under %q: %v", c.name, c.key, err)
		}
	}
}

// The jar is downloaded and checked against the pin before KPM installs it.
func TestVerifyPluginJar(t *testing.T) {
	jar := []byte("not really a jar")
	sum := sha256.Sum256(jar)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.jar" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(jar)
	}))
	t.Cleanup(srv.Close)
	pin := components.Plugin{Name: "stripe", Sha256: hex.EncodeToString(sum[:])}
	ctx := context.Background()
	if err := verifyPluginJar(ctx, srv.Client(), srv.URL+"/a.jar", pin); err != nil {
		t.Fatalf("matching jar: %v", err)
	}
	other := pin
	other.Sha256 = strings.Repeat("0", 64)
	if err := verifyPluginJar(ctx, srv.Client(), srv.URL+"/a.jar", other); codeOf(err) != "brand_apply_rejected" || !strings.Contains(messageOf(err), "does not match") {
		t.Fatalf("other digest: %v", err)
	}
	if err := verifyPluginJar(ctx, srv.Client(), srv.URL+"/missing.jar", pin); err == nil {
		t.Fatal("missing jar accepted")
	}
	for _, repo := range []string{"http://repo1.maven.org/maven2", "ftp://repo.example.test", "https://user:pw@repo.example.test", "repo.example.test"} {
		if err := CheckPluginRepository(repo); err == nil {
			t.Errorf("%s accepted", repo)
		}
		if _, err := ArtifactURL(repo, components.Plugin{Name: "x", Version: "1", Artifact: "g:a"}); err == nil {
			t.Errorf("artifact URL under %s accepted", repo)
		}
	}
	if err := CheckPluginRepository("https://repo1.maven.org/maven2"); err != nil {
		t.Fatal(err)
	}
}

func TestApprovedPlugin(t *testing.T) {
	approved := []components.Plugin{
		{Name: "stripe", Version: "8.0.3", Artifact: "org.kill-bill.billing.plugin.java:stripe-plugin", Sha256: strings.Repeat("a", 64), Status: "verified_install"},
		{Name: "adyen", Version: "0.10.0", Artifact: "org.kill-bill.billing.plugin.java:adyen-plugin", Status: "pinned_not_installed"},
		{Name: "paypal-express", Status: components.PluginStatusNoJavaArtifact},
	}
	if p, err := approvedPlugin(approved, &PaymentPlugin{Name: "stripe"}); err != nil || p.Version != "8.0.3" {
		t.Fatalf("stripe = %+v %v", p, err)
	}
	if _, err := approvedPlugin(approved, &PaymentPlugin{Name: "stripe", Version: "8.0.3"}); err != nil {
		t.Fatalf("pinned version: %v", err)
	}
	if _, err := approvedPlugin(approved, &PaymentPlugin{Name: "stripe", Version: "6.0.0"}); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("other version: %v", err)
	}
	if _, err := approvedPlugin(approved, &PaymentPlugin{Name: "paypal-express"}); codeOf(err) != "plugin_unavailable" {
		t.Fatalf("no Java artifact: %v", err)
	}
	if _, err := approvedPlugin(approved, &PaymentPlugin{Name: "adyen"}); codeOf(err) != "plugin_unavailable" {
		t.Fatalf("no sha256: %v", err)
	}
	if _, err := approvedPlugin(approved, &PaymentPlugin{Name: "unknown"}); codeOf(err) != "plugin_unavailable" {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := approvedPlugin(nil, &PaymentPlugin{Name: "stripe"}); codeOf(err) != "plugin_unavailable" {
		t.Fatalf("no components.yaml: %v", err)
	}
	// The pinned paypal-express of components.yaml has no Java artifact.
	m, err := components.Load("../../components.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approvedPlugin(m.KillBillPlugins.ApprovedPaymentPlugins, &PaymentPlugin{Name: "paypal-express"}); codeOf(err) != "plugin_unavailable" {
		t.Fatalf("components.yaml paypal-express: %v", err)
	}
	stripe, err := approvedPlugin(m.KillBillPlugins.ApprovedPaymentPlugins, &PaymentPlugin{Name: "stripe"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := ArtifactURL(m.KillBillPlugins.Repository, stripe)
	if err != nil || u != "https://repo1.maven.org/maven2/org/kill-bill/billing/plugin/java/stripe-plugin/8.0.3/stripe-plugin-8.0.3.jar" {
		t.Fatalf("artifact URL = %q %v", u, err)
	}
	if _, err := ArtifactURL("", stripe); err == nil {
		t.Fatal("no repository accepted")
	}
	if _, err := ArtifactURL("https://repo.example.test", components.Plugin{Name: "x", Version: "1", Artifact: "bad"}); err == nil {
		t.Fatal("artifact without a group accepted")
	}
}

// v1 refuses a live payment plugin with a clear error, and the mode is
// required.
func TestPaymentPluginMode(t *testing.T) {
	base := referenceYAML(t) + "  payment_plugin:\n    name: stripe\n    config_ref: env://BRAND_ZS_REFERENCE__STRIPE\n"
	if _, err := Parse([]byte(base + "    mode: test\n")); err != nil {
		t.Fatalf("test mode: %v", err)
	}
	_, err := Parse([]byte(base + "    mode: live\n"))
	if codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), "/billing/payment_plugin/mode: must be test") {
		t.Fatalf("live mode: %v", err)
	}
	if _, err := Parse([]byte(base)); codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), "/billing/payment_plugin") {
		t.Fatalf("no mode: %v", err)
	}
	if _, err := Parse([]byte(base + "    mode: sandbox\n")); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("unknown mode: %v", err)
	}
}

// A change of mode or version is a payment plugin change, and every plugin
// target is a sensitive field.
func TestPluginChangesAreGated(t *testing.T) {
	prev := &Manifest{}
	prev.Billing.PaymentPlugin = &PaymentPlugin{Name: "stripe", Mode: "test", ConfigRef: "env://BRAND_ZS_REFERENCE__X"}
	next := &Manifest{}
	next.Billing.PaymentPlugin = &PaymentPlugin{Name: "stripe", Mode: "test", Version: "8.0.3", ConfigRef: "env://BRAND_ZS_REFERENCE__X"}
	if !paymentPluginChanged(prev, next, nil) {
		t.Fatal("a version change is not a plugin change")
	}
	if !paymentPluginChanged(prev, prev, []Change{{Target: TargetPlugin}}) {
		t.Fatal("an install is not a plugin change")
	}
	fields := sensitiveFields(prev, prev, []Change{{Target: TargetPlugin, Name: "stripe", Fields: []FieldChange{{Field: "billing.payment_plugin.state"}}}})
	if strings.Join(fields, ",") != "billing.payment_plugin.state" {
		t.Fatalf("sensitive = %v", fields)
	}
}
