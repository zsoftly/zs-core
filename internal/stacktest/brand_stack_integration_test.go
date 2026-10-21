//go:build integration

package stacktest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// The secrets brands/zs-reference.yaml references, in the BRAND_ZS_REFERENCE__
// namespace.
const (
	refStaffSecretVar    = "BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET"
	refKillBillSecretVar = "BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET"
)

// Fixed test secrets for the zs-second brand. The stack keeps the Kill Bill
// tenant between runs, so they must not change.
const (
	secondKillBillSecret = "zs-second-killbill-test-secret"
	secondStaffSecret    = "zs-second-staff-test-secret"
	secondIdPSecret      = "zs-second-idp-test-secret"
	secondPluginConfig   = "org.killbill.billing.plugin.stripe.apiKey=sk_test_zs_second_placeholder"
)

type stack struct {
	kcURL string
	kc    *keycloak.Client
	kb    *killbill.Client
}

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is required", name)
	}
	return v
}

func newStack(t *testing.T) stack {
	t.Helper()
	if os.Getenv("ZS_TEST_KEYCLOAK_URL") == "" {
		t.Skip("ZS_TEST_KEYCLOAK_URL is not set: the reference stack tests need a running stack (deploy/compose)")
	}
	kc, err := keycloak.New(keycloak.Config{URL: env(t, "ZS_TEST_KEYCLOAK_URL"),
		AdminUser: env(t, "ZS_TEST_KEYCLOAK_ADMIN_USER"), AdminPassword: env(t, "ZS_TEST_KEYCLOAK_ADMIN_PASSWORD")})
	if err != nil {
		t.Fatal(err)
	}
	kb, err := killbill.New(killbill.Config{URL: env(t, "ZS_TEST_KILLBILL_URL"), AdminUser: "admin",
		AdminPassword: env(t, "ZS_TEST_KILLBILL_ADMIN_PASSWORD")})
	if err != nil {
		t.Fatal(err)
	}
	env(t, refStaffSecretVar)
	env(t, refKillBillSecretVar)
	return stack{kcURL: os.Getenv("ZS_TEST_KEYCLOAK_URL"), kc: kc, kb: kb}
}

func brandsDir() string { return filepath.Join("..", "..", "brands") }

func (s stack) deps(t *testing.T, db *postgrestest.DB) brand.Deps {
	t.Helper()
	root, err := os.OpenRoot(brandsDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	pins, err := components.Load(filepath.Join("..", "..", "components.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	deps := brand.Deps{DB: db.Pool, Keycloak: s.kc, KillBill: s.kb, Secrets: secrets.Default{}, Files: root.FS(),
		PaymentPlugins: pins.KillBillPlugins.ApprovedPaymentPlugins, InvoicePlugins: pins.KillBillPlugins.ApprovedInvoicePlugins,
		InstallablePlugins: pins.KillBillPlugins.Installable, PluginRepository: pins.KillBillPlugins.Repository}
	if mirror := os.Getenv(envMirrorURL); mirror != "" {
		deps.PluginMirrorURL, deps.PluginMirrorSecret = mirror, []byte(env(t, envMirrorSecret))
		deps.PluginHTTP = mirrorPreflightClient(t, mirror, env(t, envCoreURL))
	}
	return deps
}

var cli = brand.Options{Actor: events.Actor{Type: events.ActorService, ID: "zs-core-cli:stack-test"}, CreatedBy: "zs-core-cli:stack-test", Reason: "stack test"}

func apply(t *testing.T, deps brand.Deps, m *brand.Manifest) brand.Diff {
	t.Helper()
	d, err := brand.Apply(context.Background(), deps, m, cli)
	if err != nil {
		t.Fatalf("apply %s: %v (diff so far %s)", m.Slug, err, mustJSON(d))
	}
	return d
}

func mustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func clone(t *testing.T, m *brand.Manifest) *brand.Manifest {
	t.Helper()
	var out brand.Manifest
	if err := json.Unmarshal([]byte(mustJSON(m)), &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// fieldsOf flattens a diff to target/name/field strings.
func fieldsOf(d brand.Diff) []string {
	var out []string
	for _, c := range d.Changes {
		for _, f := range c.Fields {
			out = append(out, c.Target+" "+c.Name+" "+f.Field)
		}
	}
	return out
}

func TestReferenceBrandApplyIsIdempotentAndAudited(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	first := apply(t, deps, ref)
	t.Logf("first apply: %d changes", len(first.Changes))
	if !first.Applied || first.BrandID == "" {
		t.Fatalf("first apply = %s", mustJSON(first))
	}
	second := apply(t, deps, ref)
	if !second.Empty() || !second.Applied {
		t.Fatalf("second apply is not empty: %s", mustJSON(second))
	}
	plan, err := brand.Plan(ctx, deps, ref, cli)
	if err != nil || !plan.Empty() {
		t.Fatalf("plan after apply: %v %s", err, mustJSON(plan))
	}

	// Change the login theme and the MFA policy: exactly those two fields.
	changed := clone(t, ref)
	changed.Identity.LoginTheme = "keycloak"
	changed.Identity.MFA = brand.MFARequired
	d := apply(t, deps, changed)
	got := fieldsOf(d)
	want := []string{"keycloak.realm zs-reference identity.login_theme", "keycloak.mfa_policy zs-reference identity.mfa"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("changed diff = %v, want %v\n%s", got, want, mustJSON(d))
	}
	if f := d.Changes[0].Fields[0]; f.Before != "keycloak.v2" || f.After != "keycloak" {
		t.Fatalf("theme change = %+v", f)
	}
	if f := d.Changes[1].Fields[0]; f.Before != brand.MFAOptional || f.After != brand.MFARequired {
		t.Fatalf("mfa change = %+v", f)
	}
	brandID, _ := idgen.ParseAs(idgen.Brand, d.BrandID)
	page, err := audit.QueryBrand(ctx, db.Pool, brandID, audit.Filter{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	// Newest first: the manifest record, then the two changes.
	actions := []string{}
	for _, e := range page.Events {
		actions = append(actions, e.Action)
	}
	if strings.Join(actions, ",") != "brand.manifest.recorded,brand.keycloak.mfa_policy.updated,brand.keycloak.realm.updated" {
		t.Fatalf("audit actions = %v", actions)
	}
	if fields := page.Events[2].Details["change"].(map[string]any)["fields"].([]any); len(fields) != 1 ||
		fields[0].(map[string]any)["after"] != "keycloak" {
		t.Fatalf("audit diff = %s", mustJSON(page.Events[2].Details))
	}
	if again := apply(t, deps, changed); !again.Empty() {
		t.Fatalf("changed manifest applied twice is not empty: %s", mustJSON(again))
	}
	back := apply(t, deps, ref)
	if len(fieldsOf(back)) != 2 {
		t.Fatalf("revert diff = %v", fieldsOf(back))
	}
	if final := apply(t, deps, ref); !final.Empty() {
		t.Fatalf("final apply not empty: %s", mustJSON(final))
	}
	for _, e := range mustAllEvents(t, db, brandID) {
		if strings.Contains(mustJSON(e.Details), os.Getenv(refStaffSecretVar)) ||
			strings.Contains(mustJSON(e.Details), os.Getenv(refKillBillSecretVar)) {
			t.Fatalf("audit event %s holds a secret", e.Action)
		}
	}
}

func mustAllEvents(t *testing.T, db *postgrestest.DB, brandID idgen.UUID) []audit.Event {
	t.Helper()
	var all []audit.Event
	f := audit.Filter{Limit: audit.MaxLimit}
	for {
		page, err := audit.QueryBrand(context.Background(), db.Pool, brandID, f)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Events...)
		if page.NextCursor == "" {
			return all
		}
		f.Cursor = page.NextCursor
	}
}

// devVariant enables the password grant on the customer and staff clients so
// the test can obtain real tokens without a browser, and sets ssl_required
// to external, which the HTTP route requires. Requests on the stack come
// from loopback and private addresses, which external allows over http.
func devVariant(t *testing.T, m *brand.Manifest) *brand.Manifest {
	out := clone(t, m)
	out.Identity.CustomerClient.DirectAccessGrants = true
	out.Identity.StaffClient.DirectAccessGrants = true
	out.Identity.SSLRequired = "external"
	return out
}

func secondManifest(t *testing.T, ref *brand.Manifest) *brand.Manifest {
	t.Setenv("BRAND_ZS_SECOND__KB_SECRET", secondKillBillSecret)
	t.Setenv("BRAND_ZS_SECOND__STAFF_SECRET", secondStaffSecret)
	t.Setenv("BRAND_ZS_SECOND__IDP_SECRET", secondIdPSecret)
	t.Setenv("BRAND_ZS_SECOND__PLUGIN_CONFIG", secondPluginConfig)
	t.Setenv("BRAND_ZS_SECOND__PLUGIN_CONFIG_COPY", secondPluginConfig)
	doc := `schema_version: 1
slug: zs-second
name: ZSoftly second test brand
identity:
  realm: zs-second
  display_name: ZSoftly second
  login_theme: keycloak
  email_theme: keycloak
  locales: [en, fr]
  default_locale: fr
  ssl_required: external
  token_lifetimes: {access_token_seconds: 600}
  mfa: optional
  required_actions: [verify_email]
  customer_client:
    client_id: zs-second-customer
    redirect_uris: ["http://127.0.0.1/*"]
    direct_access_grants: true
  staff_client:
    client_id: zs-second-staff
    secret_ref: env://BRAND_ZS_SECOND__STAFF_SECRET
    redirect_uris: ["http://127.0.0.1/*"]
    direct_access_grants: true
  service_clients:
    - client_id: zs-second-billing
      secret_ref: env://BRAND_ZS_SECOND__STAFF_SECRET
  identity_providers:
    - alias: corp-oidc
      type: oidc
      display_name: Corp
      client_id: zs-second
      client_secret_ref: env://BRAND_ZS_SECOND__IDP_SECRET
      issuer: https://idp.example.test
      authorization_url: https://idp.example.test/auth
      token_url: https://idp.example.test/token
      jwks_url: https://idp.example.test/jwks
    - alias: corp-saml
      type: saml
      idp_entity_id: https://saml.example.test
      sso_url: https://saml.example.test/sso
      name_id_format: email
    - alias: google
      type: google
      client_id: zs-second-google
      client_secret_ref: env://BRAND_ZS_SECOND__IDP_SECRET
billing:
  killbill_tenant_key: zs-second
  killbill_api_secret_ref: env://BRAND_ZS_SECOND__KB_SECRET
  catalogue: zs-reference/catalogue.xml
  currencies: [CAD]
  seller: {name: ZSoftly Technologies Inc., country: CA}
  payment_plugin:
    name: stripe
    mode: test
    config_ref: env://BRAND_ZS_SECOND__PLUGIN_CONFIG
`
	m, err := brand.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

type tokenSet struct{ access, subject string }

// passwordToken runs the password grant against a realm and returns the
// access token and its subject.
func passwordToken(t *testing.T, kcURL, realm, clientID, clientSecret, user, password string) tokenSet {
	t.Helper()
	form := url.Values{"grant_type": {"password"}, "client_id": {clientID}, "username": {user}, "password": {password}, "scope": {"openid"}}
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}
	resp, err := http.PostForm(kcURL+"/realms/"+realm+"/protocol/openid-connect/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" {
		t.Fatalf("password grant %s/%s: HTTP %d %s", realm, clientID, resp.StatusCode, body)
	}
	return tokenSet{access: tok.AccessToken, subject: subjectOf(t, tok.AccessToken)}
}

func subjectOf(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Sub string `json:"sub"`
	}
	_ = json.Unmarshal(data, &c)
	return c.Sub
}

func createUser(t *testing.T, kc *keycloak.Client, realm, username, password string) {
	t.Helper()
	ctx := context.Background()
	id, err := kc.CreateUser(ctx, realm, keycloak.Object{
		"username": username, "email": username + "@example.test", "emailVerified": true, "enabled": true,
		"firstName": "Stack", "lastName": "Test", "requiredActions": []string{},
		"credentials": []map[string]any{{"type": "password", "value": password, "temporary": false}},
	})
	if err != nil {
		t.Fatalf("create user %s in %s: %v", username, realm, err)
	}
	t.Cleanup(func() { _ = kc.DeleteUser(context.Background(), realm, id) })
}

type apiServer struct {
	*httptest.Server
}

func newAPI(t *testing.T, s stack, db *postgrestest.DB, deps brand.Deps) apiServer {
	t.Helper()
	reg := brand.NewRegistry(db.Pool, time.Second, nil)
	srv := httptest.NewServer(httpapi.New(httpapi.Options{
		DB: db.Pool, Migrations: migrations.FS, Brands: reg, BrandDeps: deps, BrandsDir: brandsDir(),
		Verifier: &identity.Verifier{Brands: reg, Keys: keycloak.NewKeySet(s.kcURL, keycloak.KeySetOptions{})},
	}))
	t.Cleanup(srv.Close)
	return apiServer{srv}
}

func (a apiServer) call(t *testing.T, method, path, token string, body []byte, contentType string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, a.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func grant(t *testing.T, db *postgrestest.DB, brandID idgen.UUID, issuer, subject string, s identity.StaffScope) {
	t.Helper()
	err := pgx.BeginFunc(context.Background(), db.Pool, func(tx pgx.Tx) error {
		_, err := identity.AddStaffGrant(context.Background(), tx, identity.StaffGrant{
			BrandID: brandID, Issuer: issuer, Subject: subject, Scope: s, GrantedBy: "stack-test", Reason: "stack test",
		}, events.Actor{Type: events.ActorService, ID: "zs-core-cli:stack-test"}, "stack-test")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTokensAndBrandIsolationOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	refDev := devVariant(t, ref)
	refDiff := apply(t, deps, refDev)
	refID, _ := idgen.ParseAs(idgen.Brand, refDiff.BrandID)
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), deps, ref, cli) })

	second := secondManifest(t, ref)
	secondDiff := apply(t, deps, second)
	secondID, _ := idgen.ParseAs(idgen.Brand, secondDiff.BrandID)
	if again := apply(t, deps, second); !again.Empty() {
		t.Fatalf("zs-second second apply not empty: %s", mustJSON(again))
	}

	api := newAPI(t, s, db, deps)
	suffix := strings.ToLower(idgen.MustNewV7().String()[:8])
	createUser(t, s.kc, "zs-reference", "customer-"+suffix, "pw-"+suffix)
	createUser(t, s.kc, "zs-reference", "staff-"+suffix, "pw-"+suffix)
	createUser(t, s.kc, "zs-reference", "staff2-"+suffix, "pw-"+suffix)
	createUser(t, s.kc, "zs-second", "staff-"+suffix, "pw-"+suffix)
	createUser(t, s.kc, "zs-second", "customer-"+suffix, "pw-"+suffix)

	refCustomer := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", "customer-"+suffix, "pw-"+suffix)
	refStaff := passwordToken(t, s.kcURL, "zs-reference", "zs-staff", os.Getenv(refStaffSecretVar), "staff-"+suffix, "pw-"+suffix)
	refStaff2 := passwordToken(t, s.kcURL, "zs-reference", "zs-staff", os.Getenv(refStaffSecretVar), "staff2-"+suffix, "pw-"+suffix)
	secStaff := passwordToken(t, s.kcURL, "zs-second", "zs-second-staff", secondStaffSecret, "staff-"+suffix, "pw-"+suffix)
	secCustomer := passwordToken(t, s.kcURL, "zs-second", "zs-second-customer", "", "customer-"+suffix, "pw-"+suffix)
	refIssuer := strings.TrimRight(s.kcURL, "/") + "/realms/zs-reference"
	secIssuer := strings.TrimRight(s.kcURL, "/") + "/realms/zs-second"

	// GET /v1/me with a real zs-reference token.
	status, me := api.call(t, http.MethodGet, "/v1/me", refCustomer.access, nil, "")
	ident, _ := me["identity"].(map[string]any)
	if status != http.StatusOK || ident["brand_id"] != refDiff.BrandID || ident["kind"] != "user" ||
		ident["email_verified"] != true || ident["id"] == nil || ident["subject"] != refCustomer.subject {
		t.Fatalf("/v1/me = %d %s", status, mustJSON(me))
	}
	if ms, ok := me["memberships"].([]any); !ok || len(ms) != 0 {
		t.Fatalf("memberships = %v", me["memberships"])
	}
	status, me = api.call(t, http.MethodGet, "/v1/me", secCustomer.access, nil, "")
	if ident, _ := me["identity"].(map[string]any); status != http.StatusOK || ident["brand_id"] != secondDiff.BrandID {
		t.Fatalf("zs-second /v1/me = %d %s", status, mustJSON(me))
	}

	refBrandPath := "/v1/brands/" + refDiff.BrandID
	secBrandPath := "/v1/brands/" + secondDiff.BrandID

	// Staff without a grant: 404. With an operator grant in its brand: 200.
	if status, _ := api.call(t, http.MethodGet, refBrandPath, refStaff.access, nil, ""); status != http.StatusNotFound {
		t.Fatalf("staff without grant: %d", status)
	}
	grant(t, db, refID, refIssuer, refStaff.subject, identity.StaffOperator)
	status, b := api.call(t, http.MethodGet, refBrandPath, refStaff.access, nil, "")
	if status != http.StatusOK || b["slug"] != "zs-reference" || b["killbill_tenant_id"] == nil {
		t.Fatalf("staff with grant: %d %s", status, mustJSON(b))
	}
	// A customer token never reaches the staff route.
	if status, _ := api.call(t, http.MethodGet, refBrandPath, refCustomer.access, nil, ""); status != http.StatusNotFound {
		t.Fatalf("customer on staff route: %d", status)
	}

	// HTTP apply of the applied manifest is an empty diff.
	body, _ := json.Marshal(refDev)
	status, out := api.call(t, http.MethodPost, refBrandPath+"/apply", refStaff.access, body, "application/json")
	if status != http.StatusOK || out["applied"] != true || len(out["changes"].([]any)) != 0 {
		t.Fatalf("HTTP apply = %d %s", status, mustJSON(out))
	}
	applyPath := refBrandPath + "/apply?reason=stack+test"
	post := func(token string, m *brand.Manifest) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(m)
		return api.call(t, http.MethodPost, applyPath, token, b, "application/json")
	}
	// ssl_required none is for the command line only.
	if status, out := post(refStaff.access, ref); status != http.StatusBadRequest || out["error"].(map[string]any)["code"] != "brand_manifest_invalid" {
		t.Fatalf("HTTP apply with ssl_required none: %d %s", status, mustJSON(out))
	}
	// A sensitive change needs recent MFA; a password-only token has none.
	mfa := clone(t, refDev)
	mfa.Identity.MFA = brand.MFAOptional
	mfa.Identity.MFAACRValues = []string{"gold"}
	if status, out := post(refStaff.access, mfa); status != http.StatusForbidden || out["error"].(map[string]any)["code"] != "recent_mfa_required" {
		t.Fatalf("HTTP apply of an MFA change without MFA: %d %s", status, mustJSON(out))
	}
	redirect := clone(t, refDev)
	redirect.Identity.CustomerClient.RedirectURIs = []string{"https://app.example.test/*"}
	if status, out := post(refStaff.access, redirect); status != http.StatusForbidden {
		t.Fatalf("HTTP apply of a redirect URI change without MFA: %d %s", status, mustJSON(out))
	}
	// Another brand's secrets and files are out of reach. BRAND_ZS__ is the
	// namespace of a brand zs, whose prefix the zs-reference prefix used to
	// extend.
	for _, ref := range []string{"env://BRAND_ZS_SECOND__STAFF_SECRET", "env://BRAND_ZS__REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET", "env://BRAND_ZS_REFERENCE_KEYCLOAK_STAFF_CLIENT_SECRET"} {
		foreignSecret := clone(t, refDev)
		foreignSecret.Identity.StaffClient.SecretRef = ref
		if status, out := post(refStaff.access, foreignSecret); status != http.StatusUnprocessableEntity || out["error"].(map[string]any)["code"] != "secret_ref_invalid" {
			t.Fatalf("HTTP apply with %s: %d %s", ref, status, mustJSON(out))
		}
	}
	foreignIdP := clone(t, refDev)
	foreignIdP.Identity.IdentityProviders = []brand.IdentityProvider{{Alias: "corp", Type: "oidc", ClientID: "x",
		ClientSecretRef: "file:///etc/passwd", Issuer: "https://idp.example.test", AuthorizationURL: "https://idp.example.test/a",
		TokenURL: "https://attacker.example.test/token", JWKSURL: "https://idp.example.test/k"}}
	if status, out := post(refStaff.access, foreignIdP); status != http.StatusUnprocessableEntity || out["error"].(map[string]any)["code"] != "secret_ref_invalid" {
		t.Fatalf("HTTP apply with a provider secret from a server file: %d %s", status, mustJSON(out))
	}
	foreignFile := clone(t, refDev)
	foreignFile.Billing.Catalogue = "zs-second/catalogue.xml"
	if status, out := post(refStaff.access, foreignFile); status != http.StatusBadRequest || out["error"].(map[string]any)["code"] != "brand_manifest_invalid" {
		t.Fatalf("HTTP apply with another brand's file: %d %s", status, mustJSON(out))
	}
	// The zs-second manifest cannot be applied through the zs-reference route.
	secBody, _ := json.Marshal(second)
	if status, out := api.call(t, http.MethodPost, refBrandPath+"/apply", refStaff.access, secBody, "application/json"); status != http.StatusBadRequest {
		t.Fatalf("cross-brand manifest: %d %s", status, mustJSON(out))
	}
	// Brand events are visible to the operator of the brand only.
	status, auditPage := api.call(t, http.MethodGet, "/v1/audit/events?action=staff.grant.added", refStaff.access, nil, "")
	if status != http.StatusOK || len(auditPage["data"].([]any)) != 1 {
		t.Fatalf("audit = %d %s", status, mustJSON(auditPage))
	}

	// zs-second staff with a zs-second operator grant: its own brand works,
	// zs-reference does not exist for it.
	grant(t, db, secondID, secIssuer, secStaff.subject, identity.StaffOperator)
	if status, _ := api.call(t, http.MethodGet, secBrandPath, secStaff.access, nil, ""); status != http.StatusOK {
		t.Fatalf("zs-second staff on its brand: %d", status)
	}
	// zs-second's manifest names zs-reference's catalogue. The command line
	// reads it from the manifest's directory; over HTTP it is out of reach.
	if status, out := api.call(t, http.MethodPost, secBrandPath+"/apply?reason=x", secStaff.access, secBody, "application/json"); status != http.StatusBadRequest ||
		!strings.Contains(out["error"].(map[string]any)["message"].(string), "/billing/catalogue") {
		t.Fatalf("zs-second reading zs-reference's catalogue over HTTP: %d %s", status, mustJSON(out))
	}
	// Changing only the payment plugin's config_ref, to a reference with the
	// same value, is gated like a plugin change: no MFA, no apply.
	pluginRef := clone(t, second)
	pluginRef.Billing.Catalogue = ""
	pluginRef.Billing.PaymentPlugin.ConfigRef = "env://BRAND_ZS_SECOND__PLUGIN_CONFIG_COPY"
	pluginBody, _ := json.Marshal(pluginRef)
	if status, out := api.call(t, http.MethodPost, secBrandPath+"/apply?reason=rotate", secStaff.access, pluginBody, "application/json"); status != http.StatusForbidden ||
		out["error"].(map[string]any)["code"] != "recent_mfa_required" {
		t.Fatalf("config_ref change without MFA: %d %s", status, mustJSON(out))
	}
	if status, out := api.call(t, http.MethodPost, secBrandPath+"/apply", secStaff.access, pluginBody, "application/json"); status != http.StatusBadRequest ||
		out["error"].(map[string]any)["code"] != "reason_required" {
		t.Fatalf("config_ref change without a reason: %d %s", status, mustJSON(out))
	}
	if status, _ := api.call(t, http.MethodGet, refBrandPath, secStaff.access, nil, ""); status != http.StatusNotFound {
		t.Fatalf("zs-second staff on zs-reference: %d", status)
	}
	if status, _ := api.call(t, http.MethodPost, refBrandPath+"/apply", secStaff.access, body, "application/json"); status != http.StatusNotFound {
		t.Fatalf("zs-second staff applying zs-reference: %d", status)
	}
	status, auditPage = api.call(t, http.MethodGet, "/v1/audit/events", secStaff.access, nil, "")
	for _, e := range auditPage["data"].([]any) {
		if e.(map[string]any)["brand_id"] != secondDiff.BrandID {
			t.Fatalf("zs-second audit shows another brand: %s", mustJSON(e))
		}
	}

	// A zs-second staff grant for a zs-reference principal gives nothing in
	// zs-reference (nor in zs-second, whose tokens it cannot present).
	grant(t, db, secondID, refIssuer, refStaff2.subject, identity.StaffOperator)
	if status, _ := api.call(t, http.MethodGet, refBrandPath, refStaff2.access, nil, ""); status != http.StatusNotFound {
		t.Fatalf("zs-second grant used in zs-reference: %d", status)
	}
	if status, _ := api.call(t, http.MethodGet, secBrandPath, refStaff2.access, nil, ""); status != http.StatusNotFound {
		t.Fatalf("zs-reference token on zs-second brand: %d", status)
	}

	// Kill Bill: each brand's tenant takes only its own secret.
	if err := s.kb.CheckTenantCredentials(ctx, killbill.TenantCredentials{APIKey: "zs-second", APISecret: os.Getenv(refKillBillSecretVar)}); err == nil {
		t.Fatal("zs-second tenant accepted the zs-reference secret")
	}
	if err := s.kb.CheckTenantCredentials(ctx, killbill.TenantCredentials{APIKey: "zs-reference", APISecret: secondKillBillSecret}); err == nil {
		t.Fatal("zs-reference tenant accepted the zs-second secret")
	}
	if err := s.kb.CheckTenantCredentials(ctx, killbill.TenantCredentials{APIKey: "zs-second", APISecret: secondKillBillSecret}); err != nil {
		t.Fatalf("zs-second own secret: %v", err)
	}
	cfg, found, err := s.kb.PluginConfig(ctx, killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)}, "killbill-stripe")
	if err != nil || (found && cfg == secondPluginConfig) {
		t.Fatalf("zs-second plugin configuration visible in zs-reference: %v", err)
	}
}

// With mfa required, every identity provider of the brand runs the
// post-broker MFA flow in Keycloak, so a brokered login also needs the second
// factor. Going back to optional removes the binding. Both applies are
// idempotent.
func TestRequiredMFABindsBrokeredLoginsOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	second := secondManifest(t, ref)
	apply(t, deps, second)
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), deps, second, cli) })

	required := clone(t, second)
	required.Identity.MFA = brand.MFARequired
	required.Identity.CustomerClient.DirectAccessGrants = false
	required.Identity.StaffClient.DirectAccessGrants = false
	d := apply(t, deps, required)
	bound := 0
	for _, f := range fieldsOf(d) {
		if strings.HasSuffix(f, ".post_broker_login_flow") {
			bound++
		}
	}
	if bound != len(second.Identity.IdentityProviders) {
		t.Fatalf("diff binds %d of %d providers: %v", bound, len(second.Identity.IdentityProviders), fieldsOf(d))
	}
	for _, p := range second.Identity.IdentityProviders {
		idp, err := s.kc.GetIdentityProvider(ctx, "zs-second", p.Alias)
		if err != nil || idp.PostBrokerLoginFlowAlias != brand.FlowPostBrokerMFA {
			t.Fatalf("%s post-broker flow = %q, %v", p.Alias, idp.PostBrokerLoginFlowAlias, err)
		}
	}
	execs, err := s.kc.Executions(ctx, "zs-second", brand.FlowPostBrokerMFA)
	if err != nil || len(execs) != 1 || execs[0].ProviderID != "auth-otp-form" || execs[0].Requirement != "REQUIRED" || execs[0].AuthenticationConfig == "" {
		t.Fatalf("post-broker flow = %s, %v", mustJSON(execs), err)
	}
	if again := apply(t, deps, required); !again.Empty() {
		t.Fatalf("required applied twice is not empty: %s", mustJSON(again))
	}

	back := apply(t, deps, second)
	if !strings.Contains(strings.Join(fieldsOf(back), "|"), ".post_broker_login_flow") {
		t.Fatalf("revert diff = %v", fieldsOf(back))
	}
	for _, p := range second.Identity.IdentityProviders {
		idp, err := s.kc.GetIdentityProvider(ctx, "zs-second", p.Alias)
		if err != nil || idp.PostBrokerLoginFlowAlias != "" {
			t.Fatalf("%s post-broker flow after revert = %q, %v", p.Alias, idp.PostBrokerLoginFlowAlias, err)
		}
	}
	if again := apply(t, deps, second); !again.Empty() {
		t.Fatalf("optional applied twice is not empty: %s", mustJSON(again))
	}
}
