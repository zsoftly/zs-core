//go:build integration

package stacktest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// adminDo sends one Keycloak Admin API request the Core client has no method
// for, such as deleting a realm, and returns the status.
func adminDo(t *testing.T, kcURL, method, path string) int {
	t.Helper()
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"},
		"username": {os.Getenv("ZS_TEST_KEYCLOAK_ADMIN_USER")}, "password": {os.Getenv("ZS_TEST_KEYCLOAK_ADMIN_PASSWORD")}}
	resp, err := http.PostForm(kcURL+"/realms/master/protocol/openid-connect/token", form)
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	req, _ := http.NewRequest(method, kcURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

var formAction = regexp.MustCompile(`<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)

// brokeredLogin signs in to zs-second through the identity provider
// "upstream" for client, as a browser would, and reports how the login ended:
// "code" when Keycloak redirected back with an authorization code, "otp"
// when it asked the user to set up a one-time password, or a description of
// anything else.
func brokeredLogin(t *testing.T, kcURL, client, user, password string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	callback := "http://127.0.0.1/zs-callback"
	var redirectedTo string
	hc := &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), callback) {
			redirectedTo = req.URL.String()
			return http.ErrUseLastResponse
		}
		return nil
	}}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {client}, "response_type": {"code"}, "redirect_uri": {callback}, "scope": {"openid"},
		"kc_idp_hint": {"upstream"}, "state": {"zs-state"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"}}
	resp, err := hc.Get(kcURL + "/realms/zs-second/protocol/openid-connect/auth?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := formAction.FindSubmatch(page)
	if m == nil {
		t.Fatalf("no upstream login form at %s: HTTP %d", resp.Request.URL, resp.StatusCode)
	}
	resp, err = hc.PostForm(html.UnescapeString(string(m[1])), url.Values{"username": {user}, "password": {password}, "credentialId": {""}})
	if err != nil {
		t.Fatal(err)
	}
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	switch {
	case redirectedTo != "" && strings.Contains(redirectedTo, "code="):
		return "code"
	case strings.Contains(resp.Request.URL.RawQuery, "execution=CONFIGURE_TOTP") || strings.Contains(string(page), "kc-totp"):
		return "otp"
	}
	title := pageTitle.FindSubmatch(page)
	var heading string
	if title != nil {
		heading = strings.Join(strings.Fields(html.UnescapeString(string(title[1]))), " ")
	}
	return "unexpected: HTTP " + resp.Status + " at " + resp.Request.URL.Path + ", page " + strconv.Quote(heading)
}

var pageTitle = regexp.MustCompile(`(?s)id="kc-page-title"[^>]*>(.*?)</h1>`)

// With mfa required_for_staff, Core binds every identity provider to the
// staff post-broker flow and marks the staff client with its client scope. A
// brokered login through the staff client is then asked for a second factor
// and one through the customer client is not. Back on optional, the staff
// client's brokered login is not asked either, so the binding is what asks.
func TestRequiredForStaffBindsBrokeredStaffLoginsOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()
	ref, err := brand.Load("../../brands/zs-reference.yaml")
	if err != nil {
		t.Fatal(err)
	}
	second := secondManifest(t, ref)
	upstreamIdP := brand.IdentityProvider{Alias: "upstream", Type: "oidc", ClientID: "zs-second-upstream",
		ClientSecretRef: "env://BRAND_ZS_SECOND__IDP_SECRET", Issuer: "https://upstream.example.test",
		AuthorizationURL: "https://upstream.example.test/auth", TokenURL: "https://upstream.example.test/token",
		JWKSURL: "https://upstream.example.test/certs"}
	optional := clone(t, second)
	optional.Identity.IdentityProviders = append(optional.Identity.IdentityProviders, upstreamIdP)
	staffMFA := clone(t, optional)
	staffMFA.Identity.MFA = brand.MFARequiredForStaff
	staffMFA.Identity.StaffClient.DirectAccessGrants = false

	// The random tail of a UUIDv7: its first digits are a timestamp and
	// repeat between runs within a minute.
	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	upstream := "zs-upstream-" + suffix
	t.Cleanup(func() {
		adminDo(t, s.kcURL, http.MethodDelete, "/admin/realms/zs-second/identity-provider/instances/upstream")
		adminDo(t, s.kcURL, http.MethodDelete, "/admin/realms/"+upstream)
		_, _ = brand.Apply(context.Background(), deps, second, cli)
	})

	apply(t, deps, staffMFA)
	if again := apply(t, deps, staffMFA); !again.Empty() {
		t.Fatalf("required_for_staff applied twice is not empty: %s", mustJSON(again))
	}
	for _, p := range staffMFA.Identity.IdentityProviders {
		idp, err := s.kc.GetIdentityProvider(ctx, "zs-second", p.Alias)
		if err != nil || idp.PostBrokerLoginFlowAlias != brand.FlowPostBrokerStaffMFA {
			t.Fatalf("%s post-broker flow = %q, %v", p.Alias, idp.PostBrokerLoginFlowAlias, err)
		}
	}
	staffClient, err := s.kc.FindClient(ctx, "zs-second", "zs-second-staff")
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := s.kc.DefaultClientScopes(ctx, "zs-second", staffClient["id"].(string))
	found := false
	for _, sc := range scopes {
		found = found || sc.Name == brand.StaffClientScope
	}
	if err != nil || !found {
		t.Fatalf("staff client default scopes = %+v %v", scopes, err)
	}
	customerClient, _ := s.kc.FindClient(ctx, "zs-second", "zs-second-customer")
	scopes, _ = s.kc.DefaultClientScopes(ctx, "zs-second", customerClient["id"].(string))
	for _, sc := range scopes {
		if sc.Name == brand.StaffClientScope {
			t.Fatal("the customer client carries the staff client scope")
		}
	}

	// A realm on the same Keycloak plays the upstream identity provider.
	if err := s.kc.CreateRealm(ctx, keycloak.Object{"realm": upstream, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.kc.CreateClient(ctx, upstream, keycloak.Object{
		"clientId": "zs-second-upstream", "secret": secondIdPSecret, "publicClient": false, "standardFlowEnabled": true,
		"directAccessGrantsEnabled": false, "redirectUris": []string{s.kcURL + "/realms/zs-second/broker/upstream/endpoint"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"bcust-", "bstaff-", "bstaff2-"} {
		createUser(t, s.kc, upstream, u+suffix, "pw-"+suffix)
	}
	// The manifest only accepts https provider URLs. Point the provider at
	// the upstream realm through the Admin API and leave Core's post-broker
	// binding as apply set it. Keycloak reaches itself on its container port.
	pointUpstream := func() {
		t.Helper()
		idp, err := s.kc.GetIdentityProvider(ctx, "zs-second", "upstream")
		if err != nil {
			t.Fatal(err)
		}
		idp.Config["issuer"] = s.kcURL + "/realms/" + upstream
		idp.Config["authorizationUrl"] = s.kcURL + "/realms/" + upstream + "/protocol/openid-connect/auth"
		idp.Config["tokenUrl"] = "http://127.0.0.1:8080/realms/" + upstream + "/protocol/openid-connect/token"
		idp.Config["jwksUrl"] = "http://127.0.0.1:8080/realms/" + upstream + "/protocol/openid-connect/certs"
		// Keycloak 26.8.1 asks for the secret again when the token URL changes.
		idp.Config["clientSecret"] = secondIdPSecret
		idp.TrustEmail = true
		if err := s.kc.UpdateIdentityProvider(ctx, "zs-second", idp); err != nil {
			t.Fatal(err)
		}
	}
	pointUpstream()

	if got := brokeredLogin(t, s.kcURL, "zs-second-customer", "bcust-"+suffix, "pw-"+suffix); got != "code" {
		t.Fatalf("brokered customer login = %s, want code", got)
	}
	if got := brokeredLogin(t, s.kcURL, "zs-second-staff", "bstaff-"+suffix, "pw-"+suffix); got != "otp" {
		t.Fatalf("brokered staff login = %s, want otp", got)
	}

	apply(t, deps, optional)
	idp, err := s.kc.GetIdentityProvider(ctx, "zs-second", "upstream")
	if err != nil || idp.PostBrokerLoginFlowAlias != "" {
		t.Fatalf("post-broker flow after optional = %q, %v", idp.PostBrokerLoginFlowAlias, err)
	}
	pointUpstream()
	if got := brokeredLogin(t, s.kcURL, "zs-second-staff", "bstaff2-"+suffix, "pw-"+suffix); got != "code" {
		t.Fatalf("brokered staff login with mfa optional = %s, want code", got)
	}
}

// browser is a cookie jar that signs in to a realm as a browser would and
// keeps the Keycloak SSO session between authorization requests.
type browser struct {
	t        *testing.T
	kcURL    string
	realm    string
	hc       *http.Client
	callback string
	landed   string
}

func newBrowser(t *testing.T, kcURL, realm string) *browser {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	b := &browser{t: t, kcURL: kcURL, realm: realm, callback: "http://127.0.0.1/zs-callback"}
	b.hc = &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), b.callback) {
			b.landed = req.URL.String()
			return http.ErrUseLastResponse
		}
		return nil
	}}
	return b
}

var anyForm = regexp.MustCompile(`(?s)<form[^>]*action="([^"]+)"[^>]*>(.*?)</form>`)

// outcome describes where a step of a login ended: "code" when Keycloak
// redirected back with an authorization code, "otp" when it asked for a
// one-time password or its setup, "login" when it showed a login form, or a
// description of anything else.
func (b *browser) outcome(resp *http.Response, page []byte) string {
	switch {
	case b.landed != "" && strings.Contains(b.landed, "code="):
		return "code"
	case strings.Contains(resp.Request.URL.RawQuery, "execution=CONFIGURE_TOTP") || strings.Contains(string(page), "kc-totp") ||
		strings.Contains(string(page), `name="otp"`):
		return "otp"
	case strings.Contains(string(page), `name="username"`) || strings.Contains(string(page), `name="password"`):
		return "login"
	}
	title := pageTitle.FindSubmatch(page)
	var heading string
	if title != nil {
		heading = strings.Join(strings.Fields(html.UnescapeString(string(title[1]))), " ")
	}
	var detail string
	if m := errorText.FindSubmatch(page); m != nil {
		detail = strings.Join(strings.Fields(html.UnescapeString(tags.ReplaceAllString(string(m[1]), " "))), " ")
	}
	return "unexpected: HTTP " + resp.Status + " at " + resp.Request.URL.Path + ", page " + strconv.Quote(heading) + ", " + strconv.Quote(detail)
}

var (
	errorText = regexp.MustCompile(`(?s)id="kc-error-message"[^>]*>(.*?)</div>`)
	tags      = regexp.MustCompile(`<[^>]*>`)
)

// authorize starts an authorization code request with PKCE for client and
// returns the outcome and the page it ended on.
func (b *browser) authorize(client string) (string, []byte, *http.Response) {
	b.t.Helper()
	b.landed = ""
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {client}, "response_type": {"code"}, "redirect_uri": {b.callback}, "scope": {"openid"},
		"state": {"zs-state"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	resp, err := b.hc.Get(b.kcURL + "/realms/" + b.realm + "/protocol/openid-connect/auth?" + q.Encode())
	if err != nil {
		b.t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return b.outcome(resp, page), page, resp
}

// signIn fills the login forms of page with user and password until
// Keycloak leaves the login forms, and returns the outcome. It handles both
// the username and password form and an identity-first username form.
func (b *browser) signIn(page []byte, user, password string) string {
	b.t.Helper()
	for range 3 {
		m := anyForm.FindSubmatch(page)
		if m == nil {
			b.t.Fatalf("no login form on the page")
		}
		form := url.Values{"username": {user}, "credentialId": {""}}
		if strings.Contains(string(m[2]), `name="password"`) {
			form.Set("password", password)
		}
		resp, err := b.hc.PostForm(html.UnescapeString(string(m[1])), form)
		if err != nil {
			b.t.Fatal(err)
		}
		page, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if got := b.outcome(resp, page); got != "login" {
			return got
		}
	}
	return "login"
}

// With mfa required_for_staff, a person signed in to the customer client
// without a second factor holds a Keycloak SSO session. Authorizing the
// staff client in the same browser must not reuse that session: Keycloak asks
// for the second factor. A staff token without a second factor, here one the
// password grant issued while the brand was optional, is refused by Core on
// every staff route once the brand requires MFA for staff. With mfa required,
// a password login asks for the second factor after the password.
func TestStaffClientRefusesSSOWithoutSecondFactorOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ref, err := brand.Load("../../brands/zs-reference.yaml")
	if err != nil {
		t.Fatal(err)
	}
	second := secondManifest(t, ref)
	staffMFA := clone(t, second)
	staffMFA.Identity.MFA = brand.MFARequiredForStaff
	staffMFA.Identity.StaffClient.DirectAccessGrants = false
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), deps, second, cli) })

	secondDiff := apply(t, deps, second)
	secondID, _ := idgen.ParseAs(idgen.Brand, secondDiff.BrandID)
	api := newAPI(t, s, db, deps)
	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	user, password := "sso-"+suffix, "pw-"+suffix
	createUser(t, s.kc, "zs-second", user, password)
	secIssuer := strings.TrimRight(s.kcURL, "/") + "/realms/zs-second"

	// A staff token without a second factor, issued while the brand is
	// optional, with an operator grant.
	plain := passwordToken(t, s.kcURL, "zs-second", "zs-second-staff", secondStaffSecret, user, password)
	grant(t, db, secondID, secIssuer, plain.subject, identity.StaffOperator)
	brandPath := "/v1/brands/" + secondDiff.BrandID
	if status, out := api.call(t, http.MethodGet, brandPath, plain.access, nil, ""); status != http.StatusOK {
		t.Fatalf("staff route with mfa optional: %d %s", status, mustJSON(out))
	}

	apply(t, deps, staffMFA)
	if again := apply(t, deps, staffMFA); !again.Empty() {
		t.Fatalf("required_for_staff applied twice is not empty: %s", mustJSON(again))
	}
	// The registry of newAPI refreshes after a second.
	deadline := time.Now().Add(10 * time.Second)
	var status int
	var out map[string]any
	for {
		status, out = api.call(t, http.MethodGet, brandPath, plain.access, nil, "")
		if status != http.StatusOK || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if status != http.StatusForbidden || errCode(out) != "mfa_required" {
		t.Errorf("staff token without a second factor under required_for_staff: %d %s, want 403 mfa_required", status, mustJSON(out))
	}

	// The customer client signs in with a password only.
	b := newBrowser(t, s.kcURL, "zs-second")
	got, page, _ := b.authorize("zs-second-customer")
	if got != "login" {
		t.Fatalf("customer authorization = %s, want login", got)
	}
	if got := b.signIn(page, user, password); got != "code" {
		t.Fatalf("customer sign-in = %s, want code", got)
	}
	// The same browser authorizes the staff client. The SSO session has no
	// second factor, so Keycloak must not answer with a code.
	got, page, _ = b.authorize("zs-second-staff")
	if got == "code" {
		t.Fatalf("staff authorization reused the customer SSO session without a second factor")
	}
	if got == "login" {
		got = b.signIn(page, user, password)
	}
	if got != "otp" {
		t.Fatalf("staff authorization after a customer sign-in = %s, want otp", got)
	}

	// With mfa required, a password login on the customer client reaches the
	// second factor after the password form. The second-factor step must not
	// run before the form, where Keycloak has no user yet.
	required := clone(t, staffMFA)
	required.Identity.MFA = brand.MFARequired
	required.Identity.CustomerClient.DirectAccessGrants = false
	apply(t, deps, required)
	if again := apply(t, deps, required); !again.Empty() {
		t.Fatalf("required applied twice is not empty: %s", mustJSON(again))
	}
	fresh := newBrowser(t, s.kcURL, "zs-second")
	got, page, _ = fresh.authorize("zs-second-customer")
	if got != "login" {
		t.Fatalf("customer authorization with mfa required = %s, want login", got)
	}
	if got := fresh.signIn(page, user, password); got != "otp" {
		t.Fatalf("customer sign-in with mfa required = %s, want otp", got)
	}
}
