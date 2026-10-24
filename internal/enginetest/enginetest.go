// Package enginetest is test support for the engines. It serves the
// Keycloak Organizations endpoints and the Kill Bill endpoints Core uses
// (accounts, catalogue, subscriptions, invoices, credits, external charges,
// usage records with tracking ID deduplication and rolled-up usage,
// payments, refunds, payment methods with the external payment plugin's
// semantics, overdue and blocking states, push notification callbacks and
// KPM node commands) from memory, over HTTP, so the real clients run against
// it. It also inserts applied brands and identities and signs access tokens
// for them. The endpoints and the behaviours it reproduces are the ones
// verified against Keycloak 26.8.1 and Kill Bill 0.24.22 on the reference
// stack.
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
	"net/url"
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
	Email       string
	Locale      string
	// AdditionalEmails are unmanaged fixture CCs, not the primary email.
	AdditionalEmails []string
	Blocking         []killbill.BlockingState
	Closed           bool
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
	// tenantIDs are the Kill Bill tenant UUIDs by API key.
	tenantIDs map[string]string
	env       map[string]string
	server    *httptest.Server
	// rejectedAccounts counts account creations refused for a column limit.
	rejectedAccounts int
	// billing is the billing state of each Kill Bill tenant, by API key.
	billing map[string]*kbTenant
	// nodes are the Kill Bill nodes and their plugins.
	nodes []killbill.NodeInfo
	// requests records the audit headers of every Kill Bill write.
	requests []KillBillWrite
	// reads records the path of every Kill Bill read.
	reads []string
	// lose counts the next writes, by path prefix, that take effect and
	// then answer 503, as a lost answer looks to Core.
	lose map[string]int
	// drop counts the next writes, by path prefix, that take effect and
	// then lose the connection without an answer.
	drop map[string]int
	// usageBodies are the exact bytes of every usage POST the fake received,
	// by API key, oldest first.
	usageBodies map[string][][]byte
	// cancelRequests are the subscription cancels the fake received, by API
	// key of the Kill Bill tenant.
	cancelRequests map[string][]CancelRequest
	// afterWrite is called after every Kill Bill write the fake took.
	afterWrite func(method, path string)
	// dryRunDown makes dry-run invoices answer 500.
	dryRunDown bool
	// delays slow the Kill Bill writes whose path starts with a prefix, as
	// a slow Kill Bill does. The wait happens before the fake is locked,
	// so other calls go on meanwhile.
	delays map[string]time.Duration
	// fail answers the next writes, by path prefix, with a status and
	// takes no effect.
	fail map[string]failure
}

type failure struct {
	n, status int
}

// FailWrites makes the next n Kill Bill writes whose path starts with
// prefix answer status and take no effect: 503 is a Kill Bill that cannot
// take the write, 400 one that refuses it.
func (f *Fake) FailWrites(prefix string, n, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail == nil {
		f.fail = map[string]failure{}
	}
	f.fail[prefix] = failure{n: n, status: status}
}

// SetWriteDelay makes every Kill Bill write whose path starts with prefix
// wait d before the fake serves it. Zero removes the delay.
func (f *Fake) SetWriteDelay(prefix string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delays == nil {
		f.delays = map[string]time.Duration{}
	}
	f.delays[prefix] = d
}

func (f *Fake) delayOf(r *http.Request) time.Duration {
	if r.Method == http.MethodGet {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/kb")
	for prefix, d := range f.delays {
		if strings.HasPrefix(path, prefix) {
			return d
		}
	}
	return 0
}

// LoseAnswers makes the next n Kill Bill writes whose path starts with
// prefix, such as /1.0/kb/usages, take effect and then answer 503, as when
// the answer is lost on the way back.
func (f *Fake) LoseAnswers(prefix string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lose == nil {
		f.lose = map[string]int{}
	}
	f.lose[prefix] = n
}

// DropAnswers makes the next n Kill Bill writes whose path starts with
// prefix, such as /1.0/kb/usages, take effect and then close the connection
// without an answer, as when Kill Bill resets the connection after it wrote.
// The client sees a transport error, not a status.
func (f *Fake) DropAnswers(prefix string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.drop == nil {
		f.drop = map[string]int{}
	}
	f.drop[prefix] = n
}

// UsageBodies returns the exact request bodies of every POST /1.0/kb/usages
// the fake received with the API key of a Kill Bill tenant, oldest first. A
// POST the fake refused, lost or dropped is included, since it reached the
// fake. The slices are copies.
func (f *Fake) UsageBodies(apiKey string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, 0, len(f.usageBodies[apiKey]))
	for _, b := range f.usageBodies[apiKey] {
		out = append(out, append([]byte(nil), b...))
	}
	return out
}

// CancelRequest is one DELETE of a subscription that reached the fake.
type CancelRequest struct {
	Path   string
	Query  url.Values
	Header http.Header
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

// CancelRequests returns every subscription cancel the fake received with the
// API key of a Kill Bill tenant, oldest first, with its path, query and
// headers. A cancel the fake refused, lost or dropped is included, since it
// reached the fake. The values are copies.
func (f *Fake) CancelRequests(apiKey string) []CancelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]CancelRequest, 0, len(f.cancelRequests[apiKey]))
	for _, c := range f.cancelRequests[apiKey] {
		out = append(out, CancelRequest{Path: c.Path, Query: cloneValues(c.Query), Header: c.Header.Clone()})
	}
	return out
}

// AfterWrite calls fn after every Kill Bill write the fake took, with the
// method and the path, while the fake is locked. fn must not call the fake.
func (f *Fake) AfterWrite(fn func(method, path string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.afterWrite = fn
}

// SetDryRunDown makes dry-run invoices answer 500 while the rest of Kill
// Bill answers.
func (f *Fake) SetDryRunDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dryRunDown = down
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
		tenantSecrets: map[string]string{}, tenantIDs: map[string]string{}, env: map[string]string{}, billing: map[string]*kbTenant{},
		nodes: []killbill.NodeInfo{{NodeName: "fake-node-1", KBVersion: "0.24.22", PluginsInfo: []killbill.PluginInfo{
			{PluginName: "org.kill-bill.billing.killbill-platform-osgi-bundles-kpm", State: "RUNNING"}}}}}
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
	copy := *a
	copy.AdditionalEmails = append([]string(nil), a.AdditionalEmails...)
	return copy, true
}

// SetAdditionalAccountEmails seeds unmanaged CCs behind Core for read-only
// inspection tests. It adds no HTTP mutation endpoint to the fake.
func (f *Fake) SetAdditionalAccountEmails(apiKey, externalKey string, emails []string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[apiKey][externalKey]
	if !ok {
		return false
	}
	a.AdditionalEmails = append([]string(nil), emails...)
	return true
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if d := f.delayOf(r); d > 0 {
		time.Sleep(d)
	}
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
		path := strings.TrimPrefix(r.URL.Path, "/kb")
		if r.Method == http.MethodPost && path == "/1.0/kb/usages" {
			if f.usageBodies == nil {
				f.usageBodies = map[string][][]byte{}
			}
			key := r.Header.Get(killbill.HeaderAPIKey)
			f.usageBodies[key] = append(f.usageBodies[key], append([]byte(nil), body...))
		}
		if r.Method == http.MethodDelete && strings.HasPrefix(path, "/1.0/kb/subscriptions/") {
			if f.cancelRequests == nil {
				f.cancelRequests = map[string][]CancelRequest{}
			}
			key := r.Header.Get(killbill.HeaderAPIKey)
			f.cancelRequests[key] = append(f.cancelRequests[key], CancelRequest{Path: path, Query: cloneValues(r.URL.Query()), Header: r.Header.Clone()})
		}
		for prefix, fl := range f.fail {
			if fl.n > 0 && r.Method != http.MethodGet && strings.HasPrefix(path, prefix) {
				f.fail[prefix] = failure{n: fl.n - 1, status: fl.status}
				if fl.status >= 500 {
					http.Error(w, "unavailable", fl.status)
				} else {
					reply(w, fl.status, map[string]any{"code": 2, "message": "refused"})
				}
				return
			}
		}
		lost := false
		for prefix, n := range f.lose {
			if n > 0 && r.Method != http.MethodGet && strings.HasPrefix(path, prefix) {
				f.lose[prefix] = n - 1
				lost = true
			}
		}
		dropped := false
		for prefix, n := range f.drop {
			if n > 0 && r.Method != http.MethodGet && strings.HasPrefix(path, prefix) {
				f.drop[prefix] = n - 1
				dropped = true
			}
		}
		if dropped {
			f.serveKillBill(httptest.NewRecorder(), r, path, body)
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		} else if lost {
			f.serveKillBill(httptest.NewRecorder(), r, path, body)
			http.Error(w, "answer lost", http.StatusServiceUnavailable)
		} else {
			f.serveKillBill(w, r, path, body)
		}
		if r.Method != http.MethodGet && f.afterWrite != nil {
			f.afterWrite(r.Method, path)
		}
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

// serveTenantLookup answers GET /1.0/kb/tenants?apiKey= with the admin
// credentials. Kill Bill 0.24.22 answers an unknown API key with HTTP 500 and
// a message that names the tenant cache, not 404.
func (f *Fake) serveTenantLookup(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "admin" || pass != "pw" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	apiKey := r.URL.Query().Get("apiKey")
	id, found := f.tenantIDs[apiKey]
	if !found {
		reply(w, http.StatusInternalServerError, map[string]string{"message": "TenantCacheLoader cannot find value for key " + apiKey})
		return
	}
	reply(w, http.StatusOK, killbill.Tenant{TenantID: id, ExternalKey: apiKey, APIKey: apiKey})
}

func (f *Fake) serveKillBill(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	if r.Method == http.MethodGet {
		f.reads = append(f.reads, path)
	}
	if r.Method != http.MethodGet {
		f.requests = append(f.requests, KillBillWrite{Method: r.Method, Path: path, APIKey: r.Header.Get(killbill.HeaderAPIKey),
			CreatedBy: r.Header.Get(killbill.HeaderCreatedBy), Reason: r.Header.Get(killbill.HeaderReason), Comment: r.Header.Get(killbill.HeaderComment)})
	}
	if path == "/1.0/kb/nodesInfo" {
		f.serveNodes(w, r, body)
		return
	}
	if path == "/1.0/kb/tenants" && r.Method == http.MethodGet {
		f.serveTenantLookup(w, r)
		return
	}
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
	if f.serveBilling(w, r, apiKey, path, body) {
		return
	}
	rest, _ := strings.CutPrefix(path, "/1.0/kb/accounts")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		a, ok := accounts[r.URL.Query().Get("externalKey")]
		if !ok {
			reply(w, http.StatusNotFound, map[string]any{"code": 3003})
			return
		}
		reply(w, http.StatusOK, killbill.Account{AccountID: a.ID, ExternalKey: a.ExternalKey, Name: a.Name, Currency: a.Currency, Country: a.Country, Email: a.Email, Locale: a.Locale})
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
		a := &Account{ID: idgen.MustNewV7().String(), ExternalKey: in.ExternalKey, Name: in.Name, Currency: in.Currency, Country: in.Country, Email: in.Email, Locale: in.Locale}
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
	// The fake uses the brand UUID as its stable Kill Bill tenant identity.
	if _, err := pool.Exec(context.Background(), `INSERT INTO brands (id, slug, name, keycloak_realm, killbill_tenant_id, killbill_api_key,
    killbill_api_secret_ref, manifest, manifest_digest, applied_at)
VALUES ($1, $2, $2, $2, $1, $2, $3, $4, $5, now())`, b.ID, slug, ref, manifest, m.Digest()); err != nil {
		t.Fatal(err)
	}
	f.AddKillBillTenant(slug, "kb-secret-"+slug)
	f.mu.Lock()
	f.tenantIDs[slug] = b.ID.String()
	f.env[strings.TrimPrefix(ref, "env://")] = "kb-secret-" + slug
	f.mu.Unlock()
	return b
}

// AddKillBillTenant adds a Kill Bill tenant with its secret and the default
// test catalogue, for tests that use the fake without a database.
func (f *Fake) AddKillBillTenant(apiKey, secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenantSecrets[apiKey] = secret
	if f.tenantIDs[apiKey] == "" {
		f.tenantIDs[apiKey] = idgen.MustNewV7().String()
	}
	if f.accounts[apiKey] == nil {
		f.accounts[apiKey] = map[string]*Account{}
	}
	if f.billing[apiKey] == nil {
		f.billing[apiKey] = newKBTenant()
	}
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
