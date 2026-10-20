package brand

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
)

func referencePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "brands", "zs-reference.yaml")
}

func TestReferenceManifestIsValid(t *testing.T) {
	m, err := Load(referencePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if m.Slug != "zs-reference" || m.Identity.Realm != "zs-reference" || m.Billing.KillBillTenantKey != "zs-reference" ||
		m.Identity.MFA != MFAOptional || m.Identity.StaffClient.SecretRef != "env://BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET" {
		t.Fatalf("manifest = %+v", m)
	}
	if !strings.HasPrefix(m.Digest(), "sha256:") || len(m.Digest()) != 71 {
		t.Fatalf("digest = %s", m.Digest())
	}
	ms, err := LoadDir(filepath.Join("..", "..", "brands"))
	if err != nil || len(ms) != 1 {
		t.Fatalf("LoadDir = %d, %v", len(ms), err)
	}
}

func referenceYAML(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(referencePath(t))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func isInvalid(err error) bool {
	return zserrors.IsCode(err, zserrors.CodeBrandManifestInvalid)
}

func TestParseRejects(t *testing.T) {
	ref := referenceYAML(t)
	replace := func(old, new string) string {
		if !strings.Contains(ref, old) {
			t.Fatalf("reference manifest lacks %q", old)
		}
		return strings.Replace(ref, old, new, 1)
	}
	cases := map[string]struct {
		doc  string
		want string
	}{
		"unknown top-level key":     {ref + "extra: true\n", "/"},
		"unknown nested key":        {replace("  mfa: optional\n", "  mfa: optional\n  mfa_grace_days: 3\n"), "/identity"},
		"secret value not a ref":    {replace("env://BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET", "s3cr3t-value"), "/identity/staff_client/secret_ref"},
		"unsupported secret ref":    {replace("env://BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET", "vault://kv/kb"), "/billing/killbill_api_secret_ref"},
		"realm master":              {replace("  realm: zs-reference\n", "  realm: master\n"), "/identity/realm"},
		"realm with a dot":          {replace("  realm: zs-reference\n", "  realm: zs.reference\n"), "/identity/realm"},
		"realm too short":           {replace("  realm: zs-reference\n", "  realm: z\n"), "/identity/realm"},
		"acr value 0":               {replace("  mfa: optional\n", "  mfa: optional\n  mfa_acr_values: [\"0\"]\n"), "/identity/mfa_acr_values/0"},
		"acr value 1":               {replace("  mfa: optional\n", "  mfa: optional\n  mfa_acr_values: [\"1\"]\n"), "/identity/mfa_acr_values/0"},
		"acr value blank":           {replace("  mfa: optional\n", "  mfa: optional\n  mfa_acr_values: [\" \"]\n"), "/identity/mfa_acr_values/0"},
		"acr value too long":        {replace("  mfa: optional\n", "  mfa: optional\n  mfa_acr_values: [\""+strings.Repeat("a", 65)+"\"]\n"), "/identity/mfa_acr_values/0"},
		"bad mfa":                   {replace("mfa: optional", "mfa: sometimes"), "/identity/mfa"},
		"bad slug":                  {replace("slug: zs-reference", "slug: ZS_Reference"), "/slug"},
		"slug double hyphen":        {replace("slug: zs-reference", "slug: zs--reference"), "/slug"},
		"slug trailing hyphen":      {replace("slug: zs-reference", "slug: zs-"), "/slug"},
		"slug leading hyphen":       {replace("slug: zs-reference", "slug: -zs"), "/slug"},
		"wildcard after query":      {replace(`redirect_uris: ["http://127.0.0.1/*", "http://localhost/*"]`, `redirect_uris: ["http://127.0.0.1/cb?x=1/*"]`), "/identity/customer_client/redirect_uris/0"},
		"wrong schema version":      {replace("schema_version: 1", "schema_version: 2"), "/schema_version"},
		"default locale not listed": {replace("default_locale: en", "default_locale: de"), "/identity/default_locale"},
		"duplicate client ids":      {replace("client_id: zs-staff", "client_id: zs-customer"), "client_id"},
		"catalogue escapes dir":     {replace("catalogue: zs-reference/catalogue.xml", "catalogue: ../secrets/catalogue.xml"), "/billing/catalogue"},
		"absolute catalogue path":   {replace("catalogue: zs-reference/catalogue.xml", "catalogue: /etc/passwd"), "/billing/catalogue"},
		"bad currency":              {replace("currencies: [CAD, USD]", "currencies: [cad]"), "/billing/currencies"},
		"missing billing":           {strings.Split(ref, "billing:")[0], "billing"},
		"duplicate key":             {replace("  realm: zs-reference\n", "  realm: zs-reference\n  realm: zs-other\n"), "already defined"},
		"two documents":             {ref + "---\nslug: x\n", "exactly one document"},
		"not yaml":                  {"{[", "not valid YAML"},
		"token lifetime too low":    {replace("access_token_seconds: 300", "access_token_seconds: 5"), "/identity/token_lifetimes/access_token_seconds"},
		"idp missing secret":        {replace("  mfa: optional\n", "  mfa: optional\n  identity_providers:\n    - alias: corp\n      type: oidc\n      client_id: x\n      issuer: https://idp.example.test\n      authorization_url: https://idp.example.test/a\n      token_url: https://idp.example.test/t\n      jwks_url: https://idp.example.test/k\n"), "/identity/identity_providers/0"},
		"idp unknown type":          {replace("  mfa: optional\n", "  mfa: optional\n  identity_providers:\n    - alias: corp\n      type: ldap\n      client_id: x\n      client_secret_ref: env://X\n"), "/identity/identity_providers/0"},
	}
	for name, tc := range cases {
		_, err := Parse([]byte(tc.doc))
		if err == nil || !isInvalid(err) {
			t.Errorf("%s: err = %v", name, err)
			continue
		}
		var zsErr *zserrors.Error
		errors.As(err, &zsErr)
		if !strings.Contains(zsErr.SafeMessage(), tc.want) {
			t.Errorf("%s: message %q does not mention %q", name, zsErr.SafeMessage(), tc.want)
		}
		if strings.Contains(zsErr.SafeMessage(), "s3cr3t-value") && name != "secret value not a ref" {
			t.Errorf("%s: message echoes a value", name)
		}
	}
}

func TestParseAcceptsJSONAndAllProviderTypes(t *testing.T) {
	ref := referenceYAML(t)
	withIdPs := strings.Replace(ref, "  mfa: optional\n", `  mfa: required_for_staff
  identity_providers:
    - alias: google
      type: google
      client_id: g
      client_secret_ref: env://BRAND_ZS_REFERENCE__GOOGLE_SECRET
    - alias: corp-oidc
      type: oidc
      client_id: c
      client_secret_ref: file:///run/zs-core/secrets/zs-reference/corp
      issuer: https://idp.example.test
      authorization_url: https://idp.example.test/auth
      token_url: https://idp.example.test/token
      jwks_url: https://idp.example.test/jwks
    - alias: corp-saml
      type: saml
      idp_entity_id: https://saml.example.test
      sso_url: https://saml.example.test/sso
      name_id_format: email
`, 1)
	m, err := Parse([]byte(withIdPs))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Identity.IdentityProviders) != 3 || m.Identity.MFA != MFARequiredForStaff {
		t.Fatalf("manifest = %+v", m.Identity)
	}
	// JSON is YAML: the same manifest as JSON parses to the same digest.
	data, _ := jsonOf(m)
	m2, err := Parse(data)
	if err != nil || m2.Digest() != m.Digest() {
		t.Fatalf("JSON round trip: %v", err)
	}
}

func TestPaymentPluginKey(t *testing.T) {
	if (PaymentPlugin{Name: "stripe"}).Key() != "killbill-stripe" || (PaymentPlugin{Name: "x", ConfigKey: "y"}).Key() != "y" {
		t.Fatal("plugin key")
	}
}

func jsonOf(m *Manifest) ([]byte, error) { return json.Marshal(m) }

func TestAuditDetailsNeverCarryRedactedValues(t *testing.T) {
	ch := Change{Target: TargetClient, Name: "zs-staff", Action: ActionUpdate, Fields: []FieldChange{
		{Field: "identity.staff_client.secret", Redacted: true},
		{Field: "identity.staff_client.name", Before: "a", After: "b"},
	}}
	fields := changeDetails(ch)["fields"].([]map[string]any)
	if _, ok := fields[0]["before"]; ok || fields[0]["redacted"] != true {
		t.Fatalf("redacted field = %+v", fields[0])
	}
	if fields[1]["before"] != "a" || fields[1]["after"] != "b" {
		t.Fatalf("plain field = %+v", fields[1])
	}
	if !secretEqual("abc", "abc") || secretEqual("abc", "abd") || secretEqual("abc", "abcd") {
		t.Fatal("secretEqual")
	}
}

func TestCurrentMFA(t *testing.T) {
	ids := map[string]string{FlowMFA: "mfa-id", FlowOptional: "opt-id", FlowStaffMFA: "staff-id"}
	staff := func(override string) map[string]any {
		return map[string]any{"authenticationFlowBindingOverrides": map[string]any{"browser": override}}
	}
	cases := []struct {
		browser, override, want string
	}{
		{FlowOptional, "", MFAOptional},
		{FlowMFA, "staff-id", MFARequired},
		{FlowOptional, "staff-id", MFARequiredForStaff},
		// The staff client bound to a flow whose Cookie step can satisfy a
		// login alone does not implement required or required_for_staff,
		// so apply converges it to FlowStaffMFA.
		{FlowMFA, "", `unmanaged (browser flow "zs-browser-mfa")`},
		{FlowMFA, "mfa-id", `unmanaged (browser flow "zs-browser-mfa", staff override)`},
		{FlowOptional, "mfa-id", `unmanaged (browser flow "zs-browser", staff override)`},
		{"browser", "", `unmanaged (browser flow "browser")`},
	}
	for _, c := range cases {
		if got := currentMFA(map[string]any{"browserFlow": c.browser}, staff(c.override), ids); got != c.want {
			t.Errorf("%s/%s: %s, want %s", c.browser, c.override, got, c.want)
		}
	}
}
