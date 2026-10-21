package brand

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/secrets"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
)

func codeOf(err error) string {
	var e *zserrors.Error
	if !errors.As(err, &e) {
		return "untyped"
	}
	return zserrors.Lookup(e.Code).Name
}

func messageOf(err error) string {
	var e *zserrors.Error
	if !errors.As(err, &e) {
		return ""
	}
	return e.SafeMessage()
}

func reference(t *testing.T) *Manifest {
	t.Helper()
	m, err := Load(referencePath(t))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRedirectURIRules(t *testing.T) {
	good := []string{
		"https://app.example.test/callback",
		"https://app.example.test/*",
		"https://app.example.test:8443/a/b/*",
		"https://app.example.test",
		"http://127.0.0.1/*",
		"http://127.0.0.1:5173/callback",
		"http://localhost:3000/*",
		"http://[::1]:8080/cb",
	}
	for _, u := range good {
		if err := checkRedirectURI(u); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	bad := map[string]string{
		"http://app.example.test/cb":        "plain http",
		"http://10.0.0.1/cb":                "plain http",
		"https://*.example.test/cb":         "wildcard",
		"https://app*.example.test/cb":      "wildcard",
		"http://127.0.0.1:*":                "wildcard",
		"https://app.example.test/*/x":      "wildcard",
		"https://app.example.test/a*":       "wildcard",
		"https://app.example.test/*/*":      "wildcard",
		"*":                                 "wildcard",
		"com.example.app:/callback":         "custom schemes",
		"myapp://callback":                  "custom schemes",
		"https://user@app.example.test":     "user information",
		"https://app.example.test/#x":       "fragment",
		"https://app.example.test/cb?x=1/*": "query or fragment",
		"https://app.example.test/?/*":      "query or fragment",
		"https://app.example.test/#/*":      "query or fragment",
		"http://127.0.0.1/cb?next=/*":       "query or fragment",
		"/relative":                         "https",
	}
	for u, want := range bad {
		err := checkRedirectURI(u)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", u, err, want)
		}
	}
}

func TestWebOriginRules(t *testing.T) {
	for _, o := range []string{"+", "https://app.example.test", "https://app.example.test:8443", "http://localhost:3000", "http://127.0.0.1"} {
		if err := checkWebOrigin(o); err != nil {
			t.Errorf("%s refused: %v", o, err)
		}
	}
	for _, o := range []string{"*", "http://app.example.test", "https://*.example.test", "https://app.example.test/", "https://app.example.test/path", "app://x"} {
		if err := checkWebOrigin(o); err == nil {
			t.Errorf("%s accepted", o)
		}
	}
}

// The validator and the schema agree on redirect URIs and web origins: a
// manifest with a refused value fails Parse either way.
func TestParseRejectsBadClientURIs(t *testing.T) {
	ref := referenceYAML(t)
	for _, c := range []struct{ old, new, want string }{
		{`redirect_uris: ["http://127.0.0.1/*", "http://localhost/*"]`, `redirect_uris: ["http://127.0.0.1:*"]`, "/identity/customer_client/redirect_uris/0"},
		{`redirect_uris: ["http://127.0.0.1/*", "http://localhost/*"]`, `redirect_uris: ["https://*.example.test/cb"]`, "/identity/customer_client/redirect_uris/0"},
		{`redirect_uris: ["http://127.0.0.1/*", "http://localhost/*"]`, `redirect_uris: ["myapp://cb"]`, "/identity/customer_client/redirect_uris/0"},
		{`redirect_uris: ["http://127.0.0.1/*", "http://localhost/*"]`, `redirect_uris: ["http://app.example.test/cb"]`, "/identity/customer_client/redirect_uris/0"},
		{`web_origins: ["+"]`, `web_origins: ["*"]`, "/identity/customer_client/web_origins/0"},
		{`web_origins: ["+"]`, `web_origins: ["http://app.example.test"]`, "/identity/customer_client/web_origins/0"},
	} {
		doc := strings.Replace(ref, c.old, c.new, 1)
		_, err := Parse([]byte(doc))
		if codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), c.want) {
			t.Errorf("%s: %v", c.new, err)
		}
	}
}

// A manifest for brand A may reference only its own secrets. Each case is
// refused at validation with secret_ref_invalid, before anything resolves.
func TestCheckSecretRefsRefusesOtherBrandsSecrets(t *testing.T) {
	dir := t.TempDir()
	resolver := secrets.Default{Dir: dir}
	if err := reference(t).CheckSecretRefs(resolver); err != nil {
		t.Fatalf("reference manifest: %v", err)
	}
	foreign := []string{"env://BRAND_ZS_SECOND__X", "file:///etc/passwd", "file://" + filepath.Join(dir, "zs-second", "x"), "env://KILLBILL_TENANT_API_SECRET"}
	set := map[string]func(m *Manifest, ref string){
		"/identity/staff_client/secret_ref": func(m *Manifest, ref string) { m.Identity.StaffClient.SecretRef = ref },
		"/billing/killbill_api_secret_ref":  func(m *Manifest, ref string) { m.Billing.KillBillAPISecretRef = ref },
		"/billing/payment_plugin/config_ref": func(m *Manifest, ref string) {
			m.Billing.PaymentPlugin = &PaymentPlugin{Name: "stripe", ConfigRef: ref}
		},
		"/identity/service_clients/0/secret_ref": func(m *Manifest, ref string) {
			m.Identity.ServiceClients = []ServiceClient{{ClientID: "svc", SecretRef: ref}}
		},
		"/identity/identity_providers/0/client_secret_ref": func(m *Manifest, ref string) {
			m.Identity.IdentityProviders = []IdentityProvider{{Alias: "corp", Type: "oidc", ClientSecretRef: ref}}
		},
	}
	for path, apply := range set {
		for _, ref := range foreign {
			m := reference(t)
			apply(m, ref)
			err := m.CheckSecretRefs(resolver)
			if codeOf(err) != "secret_ref_invalid" || !strings.Contains(messageOf(err), path) {
				t.Errorf("%s = %s: %v", path, ref, err)
			}
		}
	}
	// Its own directory is fine once ZS_SECRETS_DIR is set, and refused
	// when it is not.
	m := reference(t)
	m.Billing.KillBillAPISecretRef = "file://" + filepath.Join(dir, "zs-reference", "kb")
	if err := m.CheckSecretRefs(resolver); err != nil {
		t.Fatalf("own file: %v", err)
	}
	if err := m.CheckSecretRefs(secrets.Default{}); codeOf(err) != "secret_ref_invalid" {
		t.Fatalf("file reference without ZS_SECRETS_DIR: %v", err)
	}
}

// The resolver refuses a foreign reference even when validation was
// bypassed, and a valid reference that does not resolve gives one message
// whatever the cause.
func TestResolveSecretsEnforcesTheNamespace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "zs-reference"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zs-reference", "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := secrets.Default{Dir: dir, LookupEnv: func(name string) (string, bool) {
		return "value-of-" + name, name != "BRAND_ZS_REFERENCE__MISSING"
	}}
	ctx := context.Background()
	for _, ref := range []string{"env://BRAND_ZS_SECOND__X", "file:///etc/passwd", "file://" + filepath.Join(dir, "zs-second", "x")} {
		m := reference(t)
		m.Identity.StaffClient.SecretRef = ref
		if _, err := resolveSecrets(ctx, r, m); codeOf(err) != "secret_ref_invalid" {
			t.Errorf("%s: %v", ref, err)
		}
	}
	for _, ref := range []string{"env://BRAND_ZS_REFERENCE__MISSING", "file://" + filepath.Join(dir, "zs-reference", "missing"), "file://" + filepath.Join(dir, "zs-reference", "empty")} {
		m := reference(t)
		m.Identity.StaffClient.SecretRef = ref
		_, err := resolveSecrets(ctx, r, m)
		if codeOf(err) != "secret_unresolved" || messageOf(err) != "The secret reference "+ref+" could not be resolved." {
			t.Errorf("%s: %v", ref, err)
		}
	}
	got, err := resolveSecrets(ctx, r, reference(t))
	if err != nil || got["env://BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET"] != "value-of-BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET" {
		t.Fatalf("reference secrets: %v", err)
	}
}

// The defence-in-depth check refuses any reference inside the namespace of
// another brand that exists, should the namespace check ever let one through.
func TestForeignNamespaceRefs(t *testing.T) {
	m := reference(t)
	m.Slug = "zs"
	m.Identity.StaffClient.SecretRef = "env://BRAND_ZS_REFERENCE__STAFF"
	m.Billing.KillBillAPISecretRef = "env://BRAND_ZS__KB"
	got := foreignNamespaceRefs(m, []string{"zs-reference", "zs-other"})
	if len(got) != 1 || !strings.Contains(got[0], "/identity/staff_client/secret_ref") || !strings.Contains(got[0], "zs-reference") {
		t.Fatalf("problems = %v", got)
	}
	if got := foreignNamespaceRefs(m, []string{"zs-other"}); len(got) != 0 {
		t.Fatalf("no colliding brand: %v", got)
	}
	// The brand's own prefix never counts as foreign.
	if got := foreignNamespaceRefs(m, []string{"zs"}); len(got) != 0 {
		t.Fatalf("own brand: %v", got)
	}
}

// Brands zs and zs-reference cannot reach each other's variables, whichever
// was created first: the namespace check depends on the slug alone, and the
// brands-table check agrees whether or not the other brand exists yet.
func TestZsAndZsReferenceNamespacesAreDisjoint(t *testing.T) {
	resolver := secrets.Default{}
	manifest := func(slug, staffRef, kbRef string) *Manifest {
		m := reference(t)
		m.Slug = slug
		m.Identity.StaffClient.SecretRef = staffRef
		m.Billing.KillBillAPISecretRef = kbRef
		return m
	}
	zsOwn := manifest("zs", "env://BRAND_ZS__STAFF", "env://BRAND_ZS__KB")
	refOwn := manifest("zs-reference", "env://BRAND_ZS_REFERENCE__STAFF", "env://BRAND_ZS_REFERENCE__KB")
	zsReaching := manifest("zs", "env://BRAND_ZS_REFERENCE__STAFF", "env://BRAND_ZS__KB")
	refReaching := manifest("zs-reference", "env://BRAND_ZS__REFERENCE__STAFF", "env://BRAND_ZS_REFERENCE__KB")
	for _, existing := range [][]string{nil, {"zs"}, {"zs-reference"}, {"zs", "zs-reference"}} {
		for _, m := range []*Manifest{zsOwn, refOwn} {
			if err := m.CheckSecretRefs(resolver); err != nil {
				t.Fatalf("%s own references: %v", m.Slug, err)
			}
			if got := foreignNamespaceRefs(m, existing); len(got) != 0 {
				t.Fatalf("%s own references with %v existing: %v", m.Slug, existing, got)
			}
		}
		for _, m := range []*Manifest{zsReaching, refReaching} {
			err := m.CheckSecretRefs(resolver)
			if codeOf(err) != "secret_ref_invalid" || !strings.Contains(messageOf(err), "/identity/staff_client/secret_ref") {
				t.Fatalf("%s reaching the other brand with %v existing: %v", m.Slug, existing, err)
			}
		}
	}
}

// A policy that requires a second factor cannot leave the password grant
// open: it never runs the browser flow.
func TestMFAPolicyRefusesThePasswordGrant(t *testing.T) {
	cases := []struct {
		mfa                    string
		customerGrant, staffGr bool
		want                   []string
	}{
		{MFAOptional, true, true, nil},
		{MFARequired, false, false, nil},
		{MFARequired, true, false, []string{"/identity/customer_client/direct_access_grants"}},
		{MFARequired, false, true, []string{"/identity/staff_client/direct_access_grants"}},
		{MFARequired, true, true, []string{"/identity/customer_client/direct_access_grants", "/identity/staff_client/direct_access_grants"}},
		{MFARequiredForStaff, true, false, nil},
		{MFARequiredForStaff, false, true, []string{"/identity/staff_client/direct_access_grants"}},
	}
	for _, c := range cases {
		m := reference(t)
		m.Identity.MFA = c.mfa
		m.Identity.CustomerClient.DirectAccessGrants = c.customerGrant
		m.Identity.StaffClient.DirectAccessGrants = c.staffGr
		err := m.Validate()
		if c.want == nil {
			if err != nil {
				t.Errorf("%s customer=%v staff=%v: %v", c.mfa, c.customerGrant, c.staffGr, err)
			}
			continue
		}
		for _, w := range c.want {
			if codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), w) {
				t.Errorf("%s customer=%v staff=%v: %v, want %s", c.mfa, c.customerGrant, c.staffGr, err, w)
			}
		}
		if len(c.want) == 1 && strings.Count(messageOf(err), "direct_access_grants") != 1 {
			t.Errorf("%s customer=%v staff=%v: %v", c.mfa, c.customerGrant, c.staffGr, err)
		}
		// Parse refuses the same manifests.
		doc, _ := json.Marshal(m)
		if _, err := Parse(doc); codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), "direct_access_grants") {
			t.Errorf("Parse %s customer=%v staff=%v: %v", c.mfa, c.customerGrant, c.staffGr, err)
		}
	}
}

func TestPostBrokerFlowFollowsTheMFAPolicy(t *testing.T) {
	for mfa, want := range map[string]string{MFAOptional: "", MFARequiredForStaff: FlowPostBrokerStaffMFA, MFARequired: FlowPostBrokerMFA} {
		m := reference(t)
		m.Identity.MFA = mfa
		if got := postBrokerFlow(m); got != want {
			t.Errorf("%s: %q, want %q", mfa, got, want)
		}
	}
}

func TestPaymentPluginChange(t *testing.T) {
	base := &PaymentPlugin{Name: "stripe", ConfigRef: "env://BRAND_ZS_REFERENCE__STRIPE"}
	with := func(pp *PaymentPlugin) *Manifest { m := &Manifest{}; m.Billing.PaymentPlugin = pp; return m }
	cases := []struct {
		name       string
		prev, next *PaymentPlugin
		changes    []Change
		want       bool
	}{
		{"unchanged", base, base, nil, false},
		{"none before or after", nil, nil, nil, false},
		{"added", nil, base, nil, true},
		{"removed", base, nil, nil, true},
		{"name", base, &PaymentPlugin{Name: "adyen", ConfigRef: base.ConfigRef}, nil, true},
		{"config_key", base, &PaymentPlugin{Name: "stripe", ConfigKey: "other", ConfigRef: base.ConfigRef}, nil, true},
		{"config_ref only", base, &PaymentPlugin{Name: "stripe", ConfigRef: "env://BRAND_ZS_REFERENCE__STRIPE_2"}, nil, true},
		{"config in Kill Bill", base, base, []Change{{Target: TargetPluginConfig, Name: "killbill-stripe"}}, true},
	}
	for _, c := range cases {
		if got := paymentPluginChanged(with(c.prev), with(c.next), c.changes); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	if paymentPluginChanged(nil, with(nil), nil) {
		t.Error("new brand without a plugin")
	}
}

func TestSensitiveFields(t *testing.T) {
	prev := reference(t)
	same := reference(t)
	themeOnly := []Change{{Target: TargetRealm, Name: "zs-reference", Fields: []FieldChange{{Field: "identity.login_theme"}}}}
	if got := sensitiveFields(prev, same, themeOnly); len(got) != 0 {
		t.Fatalf("theme change is sensitive: %v", got)
	}
	cases := map[string]struct {
		mutate  func(m *Manifest)
		changes []Change
		want    string
	}{
		"mfa":            {func(m *Manifest) { m.Identity.MFA = MFARequired }, nil, "identity.mfa"},
		"mfa acr values": {func(m *Manifest) { m.Identity.MFAACRValues = []string{"gold"} }, nil, "identity.mfa_acr_values"},
		"ssl required":   {func(m *Manifest) { m.Identity.SSLRequired = "external" }, nil, "identity.ssl_required"},
		"redirect uris":  {func(m *Manifest) { m.Identity.CustomerClient.RedirectURIs = []string{"https://evil.example.test/*"} }, nil, "identity.customer_client"},
		"web origins":    {func(m *Manifest) { m.Identity.StaffClient.WebOrigins = []string{"https://evil.example.test"} }, nil, "identity.staff_client"},
		"staff secret":   {func(m *Manifest) { m.Identity.StaffClient.SecretRef = "env://BRAND_ZS_REFERENCE__NEW" }, nil, "identity.staff_client"},
		"service client": {func(m *Manifest) {
			m.Identity.ServiceClients = []ServiceClient{{ClientID: "svc", SecretRef: "env://BRAND_ZS_REFERENCE__S"}}
		}, nil, "identity.service_clients"},
		"provider": {func(m *Manifest) { m.Identity.IdentityProviders = []IdentityProvider{{Alias: "corp", Type: "oidc"}} }, nil, "identity.identity_providers"},
		"payment plugin": {func(m *Manifest) {
			m.Billing.PaymentPlugin = &PaymentPlugin{Name: "stripe", ConfigRef: "env://BRAND_ZS_REFERENCE__P"}
		}, nil, "billing.payment_plugins"},
		"default payment plugin": {func(m *Manifest) {
			m.Billing.PaymentPlugins = []PaymentPlugin{{Name: "stripe", ConfigRef: "env://BRAND_ZS_REFERENCE__P"}, {Name: "adyen", ConfigRef: "env://BRAND_ZS_REFERENCE__A", Default: true}}
		}, nil, "billing.payment_plugins"},
		"invoice plugin": {func(m *Manifest) {
			m.Billing.InvoicePlugin = &InvoicePlugin{Name: "avatax", Mode: PaymentModeTest, ConfigRef: "env://BRAND_ZS_REFERENCE__T"}
		}, nil, "billing.invoice_plugin"},
		"email smtp": {func(m *Manifest) {
			m.Billing.Email = &Email{Sender: "billing@example.test", SMTP: SMTP{Host: "smtp.example.test", Port: 25}}
		}, nil, "billing.email.smtp"},
		// Drift in Keycloak with an unchanged manifest still counts.
		"client drift": {func(*Manifest) {}, []Change{{Target: TargetClient, Name: "zs-staff", Fields: []FieldChange{{Field: "identity.staff_client.secret", Redacted: true}}}}, "identity.staff_client.secret"},
		"flow drift":   {func(*Manifest) {}, []Change{{Target: TargetFlow, Name: FlowMFA, Fields: []FieldChange{{Field: "executions.auth-otp-form.requirement"}}}}, "executions.auth-otp-form.requirement"},
		"realm ssl":    {func(*Manifest) {}, []Change{{Target: TargetRealm, Name: "zs-reference", Fields: []FieldChange{{Field: "identity.ssl_required"}}}}, "identity.ssl_required"},
		"plugin drift": {func(*Manifest) {}, []Change{{Target: TargetPluginConfig, Name: "killbill-stripe", Fields: []FieldChange{{Field: "billing.payment_plugin.config", Redacted: true}}}}, "billing.payment_plugin.config"},
	}
	for name, c := range cases {
		m := reference(t)
		c.mutate(m)
		got := sensitiveFields(prev, m, c.changes)
		if !containsString(got, c.want) {
			t.Errorf("%s: %v lacks %s", name, got, c.want)
		}
	}
	if got := sensitiveFields(nil, reference(t), nil); !containsString(got, "identity.mfa") || !containsString(got, "identity.staff_client") {
		t.Fatalf("new brand: %v", got)
	}
}

func TestCheckGates(t *testing.T) {
	var confirmed []string
	httpOpts := Options{ConfirmSensitiveChange: func(f []string) error {
		confirmed = f
		return zserrors.New(zserrors.CodeRecentMFARequired, "")
	}}
	// Changing only the plugin's config_ref is gated like a name change.
	p := &plan{
		paymentPluginChange: paymentPluginChanged(
			&Manifest{Billing: Billing{PaymentPlugin: &PaymentPlugin{Name: "stripe", ConfigRef: "env://BRAND_ZS_REFERENCE__A"}}},
			&Manifest{Billing: Billing{PaymentPlugin: &PaymentPlugin{Name: "stripe", ConfigRef: "env://BRAND_ZS_REFERENCE__B"}}}, nil),
		sensitive: []string{"billing.payment_plugin"},
	}
	if err := p.checkGates(Options{}); codeOf(err) != "reason_required" {
		t.Fatalf("command line without a reason: %v", err)
	}
	if err := p.checkGates(Options{Reason: "rotate"}); err != nil {
		t.Fatalf("command line with a reason: %v", err)
	}
	httpOpts.Reason = "rotate"
	if err := p.checkGates(httpOpts); codeOf(err) != "recent_mfa_required" || len(confirmed) != 1 {
		t.Fatalf("HTTP without recent MFA: %v %v", err, confirmed)
	}
	if err := (&plan{}).checkGates(httpOpts); err != nil {
		t.Fatalf("nothing sensitive: %v", err)
	}
}

func TestRequireRealmSSLOverHTTP(t *testing.T) {
	m := reference(t) // ssl_required: none
	if err := checkRealmSSL(m, Options{RequireRealmSSL: true}); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("none over HTTP: %v", err)
	}
	m.Identity.SSLRequired = ""
	if err := checkRealmSSL(m, Options{RequireRealmSSL: true}); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("omitted over HTTP: %v", err)
	}
	for _, v := range []string{"external", "all"} {
		m.Identity.SSLRequired = v
		if err := checkRealmSSL(m, Options{RequireRealmSSL: true}); err != nil {
			t.Fatalf("%s over HTTP: %v", v, err)
		}
	}
	m.Identity.SSLRequired = "none"
	if err := checkRealmSSL(m, Options{}); err != nil {
		t.Fatalf("none on the command line: %v", err)
	}
}

// An HTTP apply of one brand may read only files under that brand's
// directory; the command line reads the manifest's directory.
func TestReadFilesIsBrandScoped(t *testing.T) {
	brandRoot := os.DirFS(filepath.Join("..", "..", "brands", "zs-reference"))
	m := reference(t) // catalogue: zs-reference/catalogue.xml
	got, err := readFiles(brandRoot, "zs-reference/", m)
	if err != nil || !strings.Contains(string(got["zs-reference/catalogue.xml"]), "<catalog") {
		t.Fatalf("own file: %v", err)
	}
	// The email templates directory is read per locale. Files the brand
	// does not override are absent and skipped.
	if _, ok := got["zs-reference/email/fr/InvoiceCreation.mustache"]; !ok {
		t.Fatalf("email template not read: %v", keysOf(got))
	}
	if _, ok := got["zs-reference/email/en/UpcomingInvoice.mustache"]; ok {
		t.Fatal("an absent email template was read")
	}
	if !strings.Contains(string(got["zs-reference/invoices/translation-fr.properties"]), "Facture") {
		t.Fatalf("invoice translation not read: %v", keysOf(got))
	}
	m.Billing.Catalogue = "zs-second/catalogue.xml"
	if _, err := readFiles(brandRoot, "zs-reference/", m); codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), "zs-reference/") {
		t.Fatalf("another brand's file: %v", err)
	}
	m.Billing.Catalogue = "catalogue.xml"
	if _, err := readFiles(brandRoot, "zs-reference/", m); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("file outside the brand directory: %v", err)
	}
	if _, err := readFiles(nil, "zs-reference/", reference(t)); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("no brand directory: %v", err)
	}
	if _, err := readFiles(os.DirFS(brandsDirForTest()), "", reference(t)); err != nil {
		t.Fatalf("command line: %v", err)
	}
	// A required template that is missing, and a template that is not
	// UTF-8, are refused.
	missing := reference(t)
	missing.Billing.InvoiceTemplates.Template = "zs-reference/invoices/none.html"
	if _, err := readFiles(brandRoot, "zs-reference/", missing); codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), "/billing/invoice_templates/template") {
		t.Fatalf("missing template: %v", err)
	}
	latin1 := fstest.MapFS{"invoice.html": {Data: []byte("caf\xe9")}}
	bad := &Manifest{Identity: Identity{Locales: []string{"en"}}, Billing: Billing{InvoiceTemplates: &InvoiceTemplates{Template: "invoice.html"}}}
	if _, err := readFiles(latin1, "", bad); codeOf(err) != "brand_manifest_invalid" || !strings.Contains(messageOf(err), "not UTF-8") {
		t.Fatalf("template that is not UTF-8: %v", err)
	}
}

func brandsDirForTest() string { return filepath.Join("..", "..", "brands") }

func keysOf(m files) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestKillBillErrorsMapToCodes(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&killbill.Unavailable{Op: "upload overdue", Status: 500, Err: errors.New("HTTP 500")}, "billing_unavailable"},
		{&killbill.Unavailable{Op: "get tenant", Err: errors.New("dial")}, "billing_unavailable"},
		{&killbill.Error{Op: "upload catalogue", Status: 400, Code: 2021}, "brand_apply_rejected"},
		{errors.Join(killbill.ErrCredentialsRejected), "brand_apply_rejected"},
	}
	for _, c := range cases {
		if got := codeOf(mapUpstream(c.err)); got != c.want {
			t.Errorf("%v: %s, want %s", c.err, got, c.want)
		}
	}
}
