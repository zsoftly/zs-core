// Package enginetest is test support for the tenant engines. It serves the
// Keycloak Organizations and Kill Bill account endpoints Core uses from
// memory, over HTTP, so the real clients run against it. It also inserts
// applied brands and identities and signs access tokens for them. It is used
// by integration tests only; the endpoints it serves are the ones verified
// against Keycloak 26.8.1 and Kill Bill 0.24.22 on the reference stack.
package enginetest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// The column limits of the Kill Bill accounts table the fake enforces.
const (
	MaxAccountName        = 100
	MaxAccountExternalKey = 255
)

// Account is a Kill Bill account held by the fake.
type Account struct {
	ID          string
	ExternalKey string
	Name        string
	Currency    string
	Country     string
	Blocking    []killbill.BlockingState
	Closed      bool
}

type organization struct {
	realm   string
	rep     keycloak.Organization
	members map[string]bool
}

// Fake is an in-memory Keycloak Organizations API and Kill Bill account API.
type Fake struct {
	mu           sync.Mutex
	keycloakDown bool
	killBillDown bool
	orgs         map[string]*organization
	// accounts are keyed by Kill Bill API key, then external key.
	accounts map[string]map[string]*Account
	// tenantSecrets are the Kill Bill API secrets by API key.
	tenantSecrets map[string]string
	env           map[string]string
	server        *httptest.Server
	// rejectedAccounts counts account creations refused for a column limit.
	rejectedAccounts int
}

// RejectedAccounts returns how many account creations the fake refused
// because the name or the external key was too long.
func (f *Fake) RejectedAccounts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejectedAccounts
}

// New starts a fake. It stops when the test ends.
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{orgs: map[string]*organization{}, accounts: map[string]map[string]*Account{},
		tenantSecrets: map[string]string{}, env: map[string]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

// Keycloak returns a client of the fake Keycloak.
func (f *Fake) Keycloak(t testing.TB) *keycloak.Client {
	t.Helper()
	kc, err := keycloak.New(keycloak.Config{URL: f.server.URL + "/kc", AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return kc
}

// KillBill returns a client of the fake Kill Bill.
func (f *Fake) KillBill(t testing.TB) *killbill.Client {
	t.Helper()
	kb, err := killbill.New(killbill.Config{URL: f.server.URL + "/kb", AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return kb
}

// Secrets resolves the Kill Bill secrets of the brands the fake inserted.
func (f *Fake) Secrets() secrets.Resolver {
	return secrets.Default{LookupEnv: func(name string) (string, bool) {
		f.mu.Lock()
		defer f.mu.Unlock()
		v, ok := f.env[name]
		return v, ok
	}}
}

// SetDown makes Keycloak or Kill Bill answer 503 to every request.
func (f *Fake) SetDown(keycloakDown, killBillDown bool) {
	f.mu.Lock()
	f.keycloakDown, f.killBillDown = keycloakDown, killBillDown
	f.mu.Unlock()
}

// Organization returns the organization whose zs_tenant_id attribute is
// tenantID in realm, with its members.
func (f *Fake) Organization(realm, tenantID string) (keycloak.Organization, []string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.orgs {
		if o.realm == realm && len(o.rep.Attributes["zs_tenant_id"]) == 1 && o.rep.Attributes["zs_tenant_id"][0] == tenantID {
			var members []string
			for m := range o.members {
				members = append(members, m)
			}
			return o.rep, members, true
		}
	}
	return keycloak.Organization{}, nil, false
}

// AddOrganizationMember adds a member behind Core's back, as an operator in
// the Keycloak console would.
func (f *Fake) AddOrganizationMember(orgID, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.orgs[orgID]; ok {
		o.members[user] = true
	}
}

// Account returns the account with externalKey in the Kill Bill tenant of
// apiKey.
func (f *Fake) Account(apiKey, externalKey string) (Account, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[apiKey][externalKey]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasPrefix(r.URL.Path, "/kc/"):
		if f.keycloakDown {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		f.serveKeycloak(w, r, strings.TrimPrefix(r.URL.Path, "/kc"), body)
	case strings.HasPrefix(r.URL.Path, "/kb/"):
		if f.killBillDown {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		f.serveKillBill(w, r, strings.TrimPrefix(r.URL.Path, "/kb"), body)
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) serveKeycloak(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	if path == "/realms/master/protocol/openid-connect/token" {
		reply(w, http.StatusOK, map[string]any{"access_token": "admin-token", "expires_in": 300})
		return
	}
	rest, ok := strings.CutPrefix(path, "/admin/realms/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[1] != "organizations" {
		reply(w, http.StatusNotFound, map[string]string{"error": "HTTP 404 Not Found"})
		return
	}
	realm := parts[0]
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
		name, value, _ := strings.Cut(r.URL.Query().Get("q"), ":")
		out := []keycloak.Organization{}
		for _, o := range f.orgs {
			for _, v := range o.rep.Attributes[name] {
				if o.realm == realm && v == value {
					out = append(out, o.rep)
				}
			}
		}
		reply(w, http.StatusOK, out)
	case len(parts) == 2 && r.Method == http.MethodPost:
		var rep keycloak.Organization
		_ = json.Unmarshal(body, &rep)
		for _, o := range f.orgs {
			if o.realm == realm && (o.rep.Name == rep.Name || o.rep.Alias == rep.Alias) {
				reply(w, http.StatusConflict, map[string]string{"errorMessage": "A organization with the same name already exists."})
				return
			}
		}
		rep.ID = idgen.MustNewV7().String()
		f.orgs[rep.ID] = &organization{realm: realm, rep: rep, members: map[string]bool{}}
		w.Header().Set("Location", "http://fake/admin/realms/"+realm+"/organizations/"+rep.ID)
		w.WriteHeader(http.StatusCreated)
	case len(parts) >= 3:
		o, ok := f.orgs[parts[2]]
		if !ok || o.realm != realm {
			reply(w, http.StatusNotFound, map[string]string{"errorMessage": "Organization not found."})
			return
		}
		f.serveOrganization(w, r, o, parts[3:], body)
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) serveOrganization(w http.ResponseWriter, r *http.Request, o *organization, rest []string, body []byte) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		reply(w, http.StatusOK, o.rep)
	case len(rest) == 0 && r.Method == http.MethodPut:
		var rep keycloak.Organization
		_ = json.Unmarshal(body, &rep)
		if rep.Alias != o.rep.Alias {
			reply(w, http.StatusBadRequest, map[string]string{"errorMessage": "Cannot change the alias"})
			return
		}
		for id, other := range f.orgs {
			if id != o.rep.ID && other.realm == o.realm && other.rep.Name == rep.Name {
				reply(w, http.StatusConflict, map[string]string{"errorMessage": "A organization with the same name already exists."})
				return
			}
		}
		rep.ID = o.rep.ID
		o.rep = rep
		w.WriteHeader(http.StatusNoContent)
	case len(rest) == 1 && rest[0] == "members" && r.Method == http.MethodGet:
		first, _ := strconv.Atoi(r.URL.Query().Get("first"))
		max, _ := strconv.Atoi(r.URL.Query().Get("max"))
		var all []string
		for m := range o.members {
			all = append(all, m)
		}
		out := []keycloak.OrganizationMember{}
		for i := first; i < len(all) && (max == 0 || i < first+max); i++ {
			out = append(out, keycloak.OrganizationMember{ID: all[i], Username: all[i]})
		}
		reply(w, http.StatusOK, out)
	case len(rest) == 1 && rest[0] == "members" && r.Method == http.MethodPost:
		var user string
		_ = json.Unmarshal(body, &user)
		if o.members[user] {
			reply(w, http.StatusConflict, map[string]string{"errorMessage": "User is already a member of the organization."})
			return
		}
		o.members[user] = true
		w.WriteHeader(http.StatusCreated)
	case len(rest) == 2 && rest[0] == "members" && r.Method == http.MethodDelete:
		if !o.members[rest[1]] {
			reply(w, http.StatusNotFound, map[string]string{"error": "HTTP 404 Not Found"})
			return
		}
		delete(o.members, rest[1])
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) serveKillBill(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	apiKey := r.Header.Get(killbill.HeaderAPIKey)
	if secret, ok := f.tenantSecrets[apiKey]; !ok || secret != r.Header.Get(killbill.HeaderAPISecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Header.Get(killbill.HeaderCreatedBy) == "" {
		reply(w, http.StatusBadRequest, map[string]any{"code": 1})
		return
	}
	accounts := f.accounts[apiKey]
	byID := func(id string) *Account {
		for _, a := range accounts {
			if a.ID == id {
				return a
			}
		}
		return nil
	}
	rest, _ := strings.CutPrefix(path, "/1.0/kb/accounts")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		a, ok := accounts[r.URL.Query().Get("externalKey")]
		if !ok {
			reply(w, http.StatusNotFound, map[string]any{"code": 3003})
			return
		}
		reply(w, http.StatusOK, killbill.Account{AccountID: a.ID, ExternalKey: a.ExternalKey, Name: a.Name, Currency: a.Currency, Country: a.Country})
	case rest == "" && r.Method == http.MethodPost:
		var in killbill.Account
		_ = json.Unmarshal(body, &in)
		// Kill Bill 0.24.22 stores the name in a varchar(100) and the
		// external key in a varchar(255), and refuses longer values.
		if utf8.RuneCountInString(in.Name) > MaxAccountName || utf8.RuneCountInString(in.ExternalKey) > MaxAccountExternalKey {
			f.rejectedAccounts++
			reply(w, http.StatusBadRequest, map[string]any{"code": 2, "message": "account name or external key too long"})
			return
		}
		if _, ok := accounts[in.ExternalKey]; ok {
			reply(w, http.StatusConflict, map[string]any{"code": 3000})
			return
		}
		a := &Account{ID: idgen.MustNewV7().String(), ExternalKey: in.ExternalKey, Name: in.Name, Currency: in.Currency, Country: in.Country}
		accounts[in.ExternalKey] = a
		w.Header().Set("Location", "http://fake/1.0/kb/accounts/"+a.ID)
		w.WriteHeader(http.StatusCreated)
	case strings.HasSuffix(rest, "/block") && r.Method == http.MethodPost:
		a := byID(strings.TrimSuffix(strings.TrimPrefix(rest, "/"), "/block"))
		if a == nil {
			reply(w, http.StatusNotFound, nil)
			return
		}
		var s killbill.BlockingState
		_ = json.Unmarshal(body, &s)
		a.Blocking = append(a.Blocking, s)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodDelete:
		a := byID(strings.TrimPrefix(rest, "/"))
		if a == nil {
			reply(w, http.StatusNotFound, nil)
			return
		}
		a.Closed = true
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// Brand is an applied brand inserted by the fake.
type Brand struct {
	ID       idgen.UUID
	Slug     string
	Realm    string
	Issuer   string
	Customer string
	Staff    string
}

// Issuer is the issuer of a fake realm.
func Issuer(realm string) string { return "http://keycloak.test/realms/" + realm }

// InsertBrand inserts an applied brand with the given registered
// permissions, a Kill Bill tenant of the fake with its secret, and CAD as the
// default currency.
func (f *Fake) InsertBrand(t testing.TB, pool *pgxpool.Pool, slug string, permissions ...string) Brand {
	t.Helper()
	b := Brand{ID: idgen.MustNewV7(), Slug: slug, Realm: slug, Issuer: Issuer(slug), Customer: slug + "-customer", Staff: slug + "-staff"}
	ref := "env://" + secrets.EnvPrefix(slug) + "KILLBILL_SECRET"
	m := &brand.Manifest{SchemaVersion: 1, Slug: slug, Name: slug, Permissions: permissions}
	m.Identity.Realm = slug
	m.Identity.CustomerClient.ClientID = b.Customer
	m.Identity.StaffClient.ClientID = b.Staff
	m.Identity.MFA = brand.MFAOptional
	m.Billing.KillBillTenantKey = slug
	m.Billing.KillBillAPISecretRef = ref
	m.Billing.Currencies = []string{"CAD", "USD"}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO brands (id, slug, name, keycloak_realm, killbill_api_key,
    killbill_api_secret_ref, manifest, manifest_digest, applied_at)
VALUES ($1, $2, $2, $2, $2, $3, $4, $5, now())`, b.ID, slug, ref, manifest, m.Digest()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.tenantSecrets[slug] = "kb-secret-" + slug
	f.env[strings.TrimPrefix(ref, "env://")] = "kb-secret-" + slug
	f.accounts[slug] = map[string]*Account{}
	f.mu.Unlock()
	return b
}

// Person is a customer of a brand: a Keycloak user, its recorded identity and
// its tokens.
type Person struct {
	Brand    Brand
	Subject  string
	Email    string
	Identity identity.Identity
}

// InsertPerson records an identity with a verified email in the brand.
func InsertPerson(t testing.TB, pool *pgxpool.Pool, b Brand, email string) Person {
	t.Helper()
	p := Person{Brand: b, Subject: idgen.MustNewV7().String(), Email: email}
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		ident, _, err := identity.Upsert(context.Background(), tx, identity.Principal{
			Brand: identity.BrandConfig{ID: b.ID, Slug: b.Slug, Realm: b.Realm}, Kind: identity.KindUser,
			Issuer: b.Issuer, Subject: p.Subject, Email: email, EmailVerified: true,
		}, "test")
		p.Identity = ident
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Signer signs access tokens for fake realms with one test key and verifies
// them through a brand registry over the database.
type Signer struct {
	key *rsa.PrivateKey
}

// NewSigner returns a signer with a fresh 2048-bit key.
func NewSigner(t testing.TB) *Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Signer{key: key}
}

type signerKeys struct{ key *rsa.PublicKey }

func (k signerKeys) Issuer(_ context.Context, realm string) (string, error) {
	return Issuer(realm), nil
}
func (k signerKeys) PublicKey(_ context.Context, _, kid string) (*rsa.PublicKey, error) {
	if kid != "test" {
		return nil, identity.ErrUnknownKey
	}
	return k.key, nil
}

// Verifier verifies the signer's tokens against the brands in pool.
func (s *Signer) Verifier(pool *pgxpool.Pool) *identity.Verifier {
	return &identity.Verifier{Brands: brand.NewRegistry(pool, time.Minute, nil), Keys: signerKeys{key: &s.key.PublicKey}}
}

// TokenOptions shape a token.
type TokenOptions struct {
	// Staff signs a staff client token instead of a customer one.
	Staff bool
	// MFA adds otp to amr.
	MFA bool
	// AuthAge is how long ago the user signed in. Zero means now.
	AuthAge time.Duration
	// Email overrides the person's email claim.
	Email string
	// Unverified sets email_verified false.
	Unverified bool
}

// Token signs an access token for the person.
func (s *Signer) Token(t testing.TB, p Person, o TokenOptions) string {
	t.Helper()
	client := p.Brand.Customer
	if o.Staff {
		client = p.Brand.Staff
	}
	email := p.Email
	if o.Email != "" {
		email = o.Email
	}
	amr := []string{"pwd"}
	if o.MFA {
		amr = append(amr, "otp")
	}
	now := time.Now()
	claims := map[string]any{
		"iss": p.Brand.Issuer, "sub": p.Subject, "aud": client, "azp": client, "typ": "Bearer",
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(), "auth_time": now.Add(-o.AuthAge).Unix(),
		"email": email, "email_verified": !o.Unverified, "amr": amr,
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	input := enc(map[string]any{"alg": "RS256", "kid": "test"}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}
