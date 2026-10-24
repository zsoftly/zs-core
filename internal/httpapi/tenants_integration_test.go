//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

type tenantAPI struct {
	t      *testing.T
	db     *postgrestest.DB
	fake   *enginetest.Fake
	signer *enginetest.Signer
	eng    tenant.Engines
	srv    *httptest.Server
}

func newTenantAPI(t *testing.T) *tenantAPI {
	t.Helper()
	db := postgrestest.New(t)
	fake := enginetest.New(t)
	signer := enginetest.NewSigner(t)
	a := &tenantAPI{t: t, db: db, fake: fake, signer: signer,
		eng: tenant.Engines{Keycloak: fake.Keycloak(t), KillBill: fake.KillBill(t), Secrets: fake.Secrets()}}
	a.srv = httptest.NewServer(httpapi.New(httpapi.Options{DB: db.Pool, Migrations: migrations.FS,
		Verifier: signer.Verifier(db.Pool), Tenants: a.eng}))
	t.Cleanup(a.srv.Close)
	return a
}

type reply struct {
	status int
	body   map[string]any
}

func (r reply) code() string {
	e, _ := r.body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (r reply) str(key string) string {
	v, _ := r.body[key].(string)
	return v
}

func (a *tenantAPI) call(method, path, credential string, body any) reply {
	a.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, a.srv.URL+path, reader)
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := reply{status: resp.StatusCode, body: map[string]any{}}
	_ = json.NewDecoder(resp.Body).Decode(&out.body)
	if resp.Header.Get("Cache-Control") != "no-store" {
		a.t.Fatalf("%s %s: Cache-Control %q", method, path, resp.Header.Get("Cache-Control"))
	}
	return out
}

func (a *tenantAPI) token(p enginetest.Person, o enginetest.TokenOptions) string {
	return a.signer.Token(a.t, p, o)
}

func mfa() enginetest.TokenOptions { return enginetest.TokenOptions{MFA: true} }

func (a *tenantAPI) person(b enginetest.Brand, email string) enginetest.Person {
	return enginetest.InsertPerson(a.t, a.db.Pool, b, email)
}

func (a *tenantAPI) createTenant(owner enginetest.Person, name string) string {
	a.t.Helper()
	r := a.call(http.MethodPost, "/v1/tenants", a.token(owner, mfa()), map[string]any{
		"idempotency_key": "create-" + idgen.MustNewV7().String(), "name": name,
	})
	if r.status != http.StatusCreated {
		a.t.Fatalf("create tenant: %d %v", r.status, r.body)
	}
	return r.str("id")
}

// join invites a person with role through the owner and accepts over HTTP.
func (a *tenantAPI) join(tenantID string, owner, p enginetest.Person, role string) string {
	a.t.Helper()
	r := a.call(http.MethodPost, "/v1/tenants/"+tenantID+"/invitations", a.token(owner, mfa()), map[string]any{"email": p.Email, "role": role})
	if r.status != http.StatusCreated {
		a.t.Fatalf("invite: %d %v", r.status, r.body)
	}
	r = a.call(http.MethodPost, "/v1/invitations/accept", a.token(p, enginetest.TokenOptions{}), map[string]any{"token": r.str("token")})
	if r.status != http.StatusOK {
		a.t.Fatalf("accept: %d %v", r.status, r.body)
	}
	return r.body["membership"].(map[string]any)["id"].(string)
}

func TestTenantRoutesEndToEnd(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-api", "vm:create")
	owner := a.person(b, "owner@example.test")
	tok := a.token(owner, mfa())

	body := map[string]any{"idempotency_key": "create-key-0001", "name": "Acme", "billing_country": "CA"}
	created := a.call(http.MethodPost, "/v1/tenants", tok, body)
	if created.status != http.StatusCreated || created.str("kind") != "organization" || created.str("billing_country") != "CA" {
		t.Fatalf("create = %d %v", created.status, created.body)
	}
	id := created.str("id")
	if again := a.call(http.MethodPost, "/v1/tenants", tok, body); again.status != http.StatusCreated || again.str("id") != id {
		t.Fatalf("replay = %d %v", again.status, again.body)
	}
	body["name"] = "Acme Two"
	if other := a.call(http.MethodPost, "/v1/tenants", tok, body); other.status != http.StatusConflict || other.code() != "idempotency_key_conflict" {
		t.Fatalf("other payload = %d %v", other.status, other.body)
	}
	if bad := a.call(http.MethodPost, "/v1/tenants", tok, map[string]any{"name": "x"}); bad.code() != "idempotency_key_invalid" {
		t.Fatalf("no key = %d %v", bad.status, bad.body)
	}
	unverified := a.token(owner, enginetest.TokenOptions{Unverified: true})
	if r := a.call(http.MethodPost, "/v1/tenants", unverified, map[string]any{"idempotency_key": "create-key-0002", "name": "x"}); r.code() != "email_not_verified" {
		t.Fatalf("unverified email = %d %v", r.status, r.body)
	}

	base := "/v1/tenants/" + id
	if r := a.call(http.MethodGet, base, tok, nil); r.status != http.StatusOK || r.str("name") != "Acme" {
		t.Fatalf("get = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPatch, base, tok, map[string]any{"name": "Acme Renamed"}); r.status != http.StatusOK || r.str("name") != "Acme Renamed" {
		t.Fatalf("patch = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPatch, base, tok, map[string]any{"name": "x", "status": "closed"}); r.code() != "validation_failed" {
		t.Fatalf("unknown field = %d %v", r.status, r.body)
	}
	members := a.call(http.MethodGet, base+"/members", tok, nil)
	if data, _ := members.body["data"].([]any); members.status != http.StatusOK || len(data) != 1 || members.body["next_cursor"] != nil {
		t.Fatalf("members = %d %v", members.status, members.body)
	}
	teams := a.call(http.MethodGet, base+"/teams?limit=2", tok, nil)
	if data, _ := teams.body["data"].([]any); teams.status != http.StatusOK || len(data) != 2 || teams.body["next_cursor"] == nil {
		t.Fatalf("teams page = %d %v", teams.status, teams.body)
	}
	rest := a.call(http.MethodGet, base+"/teams?cursor="+teams.body["next_cursor"].(string), tok, nil)
	if data, _ := rest.body["data"].([]any); len(data) != 1 {
		t.Fatalf("teams second page = %v", rest.body)
	}
	if r := a.call(http.MethodGet, base+"/activation", tok, nil); r.status != http.StatusOK || r.str("state") != "pending" {
		t.Fatalf("activation = %d %v", r.status, r.body)
	}
	events := a.call(http.MethodGet, base+"/audit/events?action=tenant.updated", tok, nil)
	if data, _ := events.body["data"].([]any); events.status != http.StatusOK || len(data) != 1 {
		t.Fatalf("tenant audit = %d %v", events.status, events.body)
	}
	if r := a.call(http.MethodGet, base+"/audit/events?tenant_id="+id, tok, nil); r.code() != "validation_failed" {
		t.Fatalf("tenant_id on the member audit route = %d %v", r.status, r.body)
	}
	me := a.call(http.MethodGet, "/v1/me", tok, nil)
	ms, _ := me.body["memberships"].([]any)
	if len(ms) != 1 || ms[0].(map[string]any)["role"] != "owner" || ms[0].(map[string]any)["tenant"].(map[string]any)["id"] != id {
		t.Fatalf("me = %v", me.body)
	}
	for _, path := range []string{"/v1/tenants/" + idgen.Format(idgen.Tenant, idgen.MustNewV7()), "/v1/tenants/tnt_nope", "/v1/tenants/personal"} {
		if r := a.call(http.MethodGet, path, tok, nil); r.status != http.StatusNotFound || r.code() != "tenant_not_found" {
			t.Fatalf("%s = %d %v", path, r.status, r.body)
		}
	}
	personal := a.call(http.MethodPost, "/v1/tenants/personal", tok, nil)
	if personal.status != http.StatusCreated || personal.str("kind") != "personal" {
		t.Fatalf("personal = %d %v", personal.status, personal.body)
	}
	if again := a.call(http.MethodPost, "/v1/tenants/personal", tok, nil); again.status != http.StatusOK || again.str("id") != personal.str("id") {
		t.Fatalf("personal again = %d %v", again.status, again.body)
	}
	// A member without audit:read cannot read the tenant's audit trail.
	member := a.person(b, "member@example.test")
	a.join(id, owner, member, "member")
	memberTok := a.token(member, mfa())
	if r := a.call(http.MethodGet, base+"/audit/events", memberTok, nil); r.status != http.StatusForbidden || r.code() != "permission_denied" {
		t.Fatalf("member audit = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/members", memberTok, nil); r.status != http.StatusOK {
		t.Fatalf("member reads members = %d %v", r.status, r.body)
	}
}

// A removed member's live token and live API key both fail on the next
// request.
func TestRemovedMemberLosesTokenAndKeyAtOnce(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-api")
	owner, admin := a.person(b, "owner@example.test"), a.person(b, "admin@example.test")
	id := a.createTenant(owner, "Removal")
	mem := a.join(id, owner, admin, "admin")
	adminTok := a.token(admin, mfa())
	key := a.call(http.MethodPost, "/v1/tenants/"+id+"/api-keys", adminTok, map[string]any{"name": "ci", "permissions": []string{"tenant:read", "member:read"}})
	if key.status != http.StatusCreated {
		t.Fatalf("create key = %d %v", key.status, key.body)
	}
	secret := key.str("secret")
	if !strings.HasPrefix(secret, "zs_zs-api_") {
		t.Fatalf("secret = %q", secret)
	}
	for _, cred := range []string{adminTok, secret} {
		if r := a.call(http.MethodGet, "/v1/tenants/"+id, cred, nil); r.status != http.StatusOK {
			t.Fatalf("before removal = %d %v", r.status, r.body)
		}
	}
	if r := a.call(http.MethodDelete, "/v1/tenants/"+id+"/members/"+mem, a.token(owner, enginetest.TokenOptions{}), nil); r.code() != "recent_mfa_required" {
		t.Fatalf("remove without MFA = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodDelete, "/v1/tenants/"+id+"/members/"+mem, a.token(owner, mfa()), nil); r.status != http.StatusNoContent {
		t.Fatalf("remove = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, "/v1/tenants/"+id, adminTok, nil); r.status != http.StatusNotFound || r.code() != "tenant_not_found" {
		t.Fatalf("removed member's token = %d %v", r.status, r.body)
	}
	// Removal revokes the key itself.
	if r := a.call(http.MethodGet, "/v1/tenants/"+id, secret, nil); r.status != http.StatusUnauthorized || r.code() != "api_key_invalid" {
		t.Fatalf("removed member's key = %d %v", r.status, r.body)
	}
	// Invited again, the person gets a new membership, and the old key stays
	// revoked.
	again := a.join(id, owner, admin, "admin")
	if again == mem {
		t.Fatalf("re-acceptance reused membership %s", mem)
	}
	if r := a.call(http.MethodGet, "/v1/tenants/"+id, adminTok, nil); r.status != http.StatusOK {
		t.Fatalf("re-invited member's token = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, "/v1/tenants/"+id, secret, nil); r.status != http.StatusUnauthorized || r.code() != "api_key_invalid" {
		t.Fatalf("old key after re-acceptance = %d %v", r.status, r.body)
	}
}

// An API key never holds more than its creator holds now.
func TestAPIKeyShrinksWithItsCreator(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-api")
	owner, admin := a.person(b, "owner@example.test"), a.person(b, "admin@example.test")
	id := a.createTenant(owner, "Shrink")
	mem := a.join(id, owner, admin, "admin")
	base := "/v1/tenants/" + id
	if r := a.call(http.MethodPost, base+"/api-keys", a.token(admin, mfa()), map[string]any{"name": "ci", "permissions": []string{"billing:read"}}); r.code() != "permission_denied" {
		t.Fatalf("key above the creator = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/api-keys", a.token(admin, enginetest.TokenOptions{}), map[string]any{"name": "ci", "permissions": []string{"team:manage"}}); r.code() != "recent_mfa_required" {
		t.Fatalf("key without MFA = %d %v", r.status, r.body)
	}
	key := a.call(http.MethodPost, base+"/api-keys", a.token(admin, mfa()), map[string]any{"name": "ci", "permissions": []string{"team:manage", "member:read", "apikey:create"}})
	secret := key.str("secret")
	if r := a.call(http.MethodPost, base+"/teams", secret, map[string]any{"name": "Ops"}); r.status != http.StatusCreated {
		t.Fatalf("key creates a team = %d %v", r.status, r.body)
	}
	// An API key can never create another key: it never has recent MFA.
	if r := a.call(http.MethodPost, base+"/api-keys", secret, map[string]any{"name": "child", "permissions": []string{"member:read"}}); r.code() != "recent_mfa_required" {
		t.Fatalf("key creating a key = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPatch, base+"/members/"+mem, a.token(owner, mfa()), map[string]any{"role": "member"}); r.status != http.StatusOK || r.str("role") != "member" {
		t.Fatalf("demote = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/teams", secret, map[string]any{"name": "Ops 2"}); r.status != http.StatusForbidden || r.code() != "permission_denied" {
		t.Fatalf("key after demotion = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/members", secret, nil); r.status != http.StatusOK {
		t.Fatalf("key keeps what the creator still holds = %d %v", r.status, r.body)
	}
	list := a.call(http.MethodGet, base+"/api-keys", a.token(owner, mfa()), nil)
	data, _ := list.body["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["secret"] != nil || data[0].(map[string]any)["last_used_at"] == nil {
		t.Fatalf("list = %v", list.body)
	}
	keyID := data[0].(map[string]any)["id"].(string)
	if r := a.call(http.MethodDelete, base+"/api-keys/"+keyID+"?reason=rotate", a.token(owner, mfa()), nil); r.status != http.StatusNoContent {
		t.Fatalf("revoke = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base+"/members", secret, nil); r.status != http.StatusUnauthorized || r.code() != "api_key_invalid" {
		t.Fatalf("revoked key = %d %v", r.status, r.body)
	}
}

func TestInvitationsOverHTTP(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-api")
	owner := a.person(b, "owner@example.test")
	id := a.createTenant(owner, "Invites")
	inv := a.call(http.MethodPost, "/v1/tenants/"+id+"/invitations", a.token(owner, mfa()), map[string]any{"email": "Mixed.Case@Example.TEST", "role": "member"})
	if inv.status != http.StatusCreated || inv.str("token") == "" {
		t.Fatalf("invite = %d %v", inv.status, inv.body)
	}
	token := inv.str("token")
	list := a.call(http.MethodGet, "/v1/tenants/"+id+"/invitations", a.token(owner, mfa()), nil)
	if strings.Contains(strings.ToLower(jsonOf(list.body)), strings.ToLower(token)) {
		t.Fatal("the invitation list shows the token")
	}
	wrong := a.person(b, "someone@example.test")
	if r := a.call(http.MethodPost, "/v1/invitations/accept", a.token(wrong, enginetest.TokenOptions{}), map[string]any{"token": token}); r.status != http.StatusForbidden || r.code() != "invitation_email_mismatch" {
		t.Fatalf("different email = %d %v", r.status, r.body)
	}
	right := a.person(b, "mixed.case@example.test")
	if r := a.call(http.MethodPost, "/v1/invitations/accept", a.token(right, enginetest.TokenOptions{AuthAge: time.Hour}), map[string]any{"token": token}); r.code() != "recent_auth_required" {
		t.Fatalf("old authentication = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/invitations/accept", a.token(right, enginetest.TokenOptions{}), map[string]any{"token": token}); r.status != http.StatusOK {
		t.Fatalf("same email in another case = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/invitations/accept", a.token(right, enginetest.TokenOptions{}), map[string]any{"token": token}); r.code() != "invitation_not_found" {
		t.Fatalf("second acceptance = %d %v", r.status, r.body)
	}
}

func TestLastOwnerOverHTTP(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-api")
	owner, admin := a.person(b, "owner@example.test"), a.person(b, "admin@example.test")
	id := a.createTenant(owner, "Owners")
	base := "/v1/tenants/" + id
	me := a.call(http.MethodGet, "/v1/me", a.token(owner, mfa()), nil)
	ownerMem := me.body["memberships"].([]any)[0].(map[string]any)["id"].(string)
	tok := a.token(owner, mfa())
	if r := a.call(http.MethodDelete, base+"/members/"+ownerMem, tok, nil); r.status != http.StatusConflict || r.code() != "last_owner" {
		t.Fatalf("remove the last owner = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPatch, base+"/members/"+ownerMem, tok, map[string]any{"role": "admin"}); r.code() != "last_owner" {
		t.Fatalf("demote the last owner = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPatch, base+"/members/"+ownerMem, tok, map[string]any{"status": "suspended"}); r.code() != "last_owner" {
		t.Fatalf("suspend the last owner = %d %v", r.status, r.body)
	}
	adminMem := a.join(id, owner, admin, "admin")
	if r := a.call(http.MethodPost, base+"/ownership/transfer", a.token(owner, enginetest.TokenOptions{}), map[string]any{"membership_id": adminMem}); r.code() != "recent_mfa_required" {
		t.Fatalf("transfer without MFA = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, base+"/ownership/transfer", a.token(admin, mfa()), map[string]any{"membership_id": ownerMem}); r.code() != "permission_denied" {
		t.Fatalf("admin transfers = %d %v", r.status, r.body)
	}
	r := a.call(http.MethodPost, base+"/ownership/transfer", tok, map[string]any{"membership_id": adminMem})
	if r.status != http.StatusOK || r.body["to"].(map[string]any)["role"] != "owner" || r.body["from"].(map[string]any)["role"] != "admin" {
		t.Fatalf("transfer = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodDelete, base+"/members/"+adminMem, a.token(admin, mfa()), nil); r.code() != "last_owner" {
		t.Fatalf("remove the new last owner = %d %v", r.status, r.body)
	}
}

// Credentials never cross brands or tenants, and API keys work on tenant
// routes only.
func TestCredentialBoundaries(t *testing.T) {
	a := newTenantAPI(t)
	brandA := a.fake.InsertBrand(t, a.db.Pool, "zs-alpha")
	brandB := a.fake.InsertBrand(t, a.db.Pool, "zs-beta")
	ownerA := a.person(brandA, "owner@example.test")
	ownerB := a.person(brandB, "owner@example.test")
	tenantA := a.createTenant(ownerA, "Alpha")
	tenantA2 := a.createTenant(ownerA, "Alpha Two")
	tenantB := a.createTenant(ownerB, "Beta")
	tokA, tokB := a.token(ownerA, mfa()), a.token(ownerB, mfa())

	// A token of brand B reads, changes and accepts nothing in brand A.
	if r := a.call(http.MethodGet, "/v1/tenants/"+tenantA, tokB, nil); r.code() != "tenant_not_found" {
		t.Fatalf("brand B token reads brand A = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/tenants/"+tenantA+"/teams", tokB, map[string]any{"name": "x"}); r.code() != "tenant_not_found" {
		t.Fatalf("brand B token writes brand A = %d %v", r.status, r.body)
	}
	inv := a.call(http.MethodPost, "/v1/tenants/"+tenantA+"/invitations", tokA, map[string]any{"email": "owner@example.test", "role": "member"})
	if r := a.call(http.MethodPost, "/v1/invitations/accept", tokB, map[string]any{"token": inv.str("token")}); r.code() != "invitation_not_found" {
		t.Fatalf("brand B token accepts brand A's invitation = %d %v", r.status, r.body)
	}
	// Kill Bill accounts live in each brand's own Kill Bill tenant.
	if _, ok := a.fake.Account(brandA.Slug, tenantA); !ok {
		t.Fatal("brand A account missing")
	}
	if _, ok := a.fake.Account(brandB.Slug, tenantA); ok {
		t.Fatal("brand A tenant has an account in brand B's Kill Bill tenant")
	}

	key := a.call(http.MethodPost, "/v1/tenants/"+tenantA+"/api-keys", tokA, map[string]any{"name": "ci", "permissions": []string{"tenant:read"}})
	secret := key.str("secret")
	if r := a.call(http.MethodGet, "/v1/tenants/"+tenantA2, secret, nil); r.status != http.StatusNotFound || r.code() != "tenant_not_found" {
		t.Fatalf("key on another tenant = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, "/v1/tenants/"+tenantB, secret, nil); r.code() != "tenant_not_found" {
		t.Fatalf("key on another brand = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, "/v1/me", secret, nil); r.status != http.StatusForbidden || r.code() != "permission_denied" {
		t.Fatalf("key on /v1/me = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/tenants", secret, map[string]any{"idempotency_key": "k-00000001", "name": "x"}); r.code() != "permission_denied" {
		t.Fatalf("key creating a tenant = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, "/v1/audit/events", secret, nil); r.status != http.StatusNotFound {
		t.Fatalf("key on a staff route = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, "/v1/tenants/"+tenantA, "zs_zs-alpha_"+strings.Repeat("A", 43), nil); r.status != http.StatusUnauthorized || r.code() != "api_key_invalid" {
		t.Fatalf("unknown key = %d %v", r.status, r.body)
	}

	// Staff: an activation grant in brand A acts in brand A only.
	staffA := a.person(brandA, "staff@example.test")
	staffTok := a.token(staffA, enginetest.TokenOptions{Staff: true})
	decision := map[string]any{"state": "approved", "reason": "KYC passed", "request_id": "activation-0001"}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+tenantA+"/activation", staffTok, decision); r.status != http.StatusNotFound {
		t.Fatalf("staff without the grant = %d %v", r.status, r.body)
	}
	grantStaff(t, a.db, brandA, staffA, identity.StaffActivation)
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+tenantA+"/activation", staffTok, decision); r.status != http.StatusCreated || r.str("state") != "approved" {
		t.Fatalf("decision = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+tenantA+"/activation", staffTok, decision); r.status != http.StatusOK {
		t.Fatalf("replayed decision = %d %v", r.status, r.body)
	}
	decision["state"] = "rejected"
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+tenantA+"/activation", staffTok, decision); r.code() != "idempotency_key_conflict" {
		t.Fatalf("conflicting decision = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+tenantB+"/activation", staffTok, decision); r.status != http.StatusNotFound {
		t.Fatalf("brand A staff on brand B = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPost, "/v1/staff/tenants/"+tenantA+"/activation", tokA, decision); r.status != http.StatusNotFound {
		t.Fatalf("customer on the staff route = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, "/v1/tenants/"+tenantA, staffTok, nil); r.code() != "tenant_not_found" {
		t.Fatalf("staff token on a tenant route = %d %v", r.status, r.body)
	}
}

func TestSuspendedTenantKeepsReadsAndRefusesWrites(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-api")
	owner := a.person(b, "owner@example.test")
	id := a.createTenant(owner, "Suspended")
	tid, _ := idgen.ParseAs(idgen.Tenant, id)
	if _, err := tenant.Suspend(context.Background(), a.db.Pool, a.eng, scope.Scope{Brand: b.ID, Tenant: tid},
		events.Actor{Type: events.ActorService, ID: "test"}, "overdue", "test"); err != nil {
		t.Fatal(err)
	}
	tok := a.token(owner, mfa())
	if r := a.call(http.MethodGet, "/v1/tenants/"+id, tok, nil); r.status != http.StatusOK || r.str("status") != "suspended" {
		t.Fatalf("read = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPatch, "/v1/tenants/"+id, tok, map[string]any{"name": "x"}); r.status != http.StatusNotFound || r.code() != "tenant_not_found" {
		t.Fatalf("write = %d %v", r.status, r.body)
	}
}

func TestTenantCreationNeedsTheEngines(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-api")
	owner := a.person(b, "owner@example.test")
	a.fake.SetDown(false, true)
	r := a.call(http.MethodPost, "/v1/tenants", a.token(owner, mfa()), map[string]any{"idempotency_key": "down-key-01", "name": "Down"})
	if r.status != http.StatusServiceUnavailable || r.code() != "billing_unavailable" || r.body["error"].(map[string]any)["retryable"] != true {
		t.Fatalf("Kill Bill down = %d %v", r.status, r.body)
	}
	bare := httptest.NewServer(httpapi.New(httpapi.Options{DB: a.db.Pool, Migrations: migrations.FS, Verifier: a.signer.Verifier(a.db.Pool)}))
	defer bare.Close()
	a.srv = bare
	if r := a.call(http.MethodPost, "/v1/tenants/personal", a.token(owner, mfa()), nil); r.status != http.StatusServiceUnavailable || r.code() != "not_ready" {
		t.Fatalf("no engines = %d %v", r.status, r.body)
	}
}

func grantStaff(t *testing.T, db *postgrestest.DB, b enginetest.Brand, p enginetest.Person, s identity.StaffScope) {
	t.Helper()
	err := pgx.BeginFunc(context.Background(), db.Pool, func(tx pgx.Tx) error {
		_, err := identity.AddStaffGrant(context.Background(), tx, identity.StaffGrant{
			BrandID: b.ID, Issuer: b.Issuer, Subject: p.Subject, Scope: s, GrantedBy: "test", Reason: "test",
		}, events.Actor{Type: events.ActorService, ID: "test"}, "corr")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A tenant whose closure has started answers nothing: its members and its API
// keys are refused on reads and on writes alike.
func TestClosingTenantRefusesMembersAndAPIKeys(t *testing.T) {
	a := newTenantAPI(t)
	postgrestest.EnableSettlementStartRule(t, a.db)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-api")
	owner := a.person(b, "owner@example.test")
	id := a.createTenant(owner, "Closing")
	base := "/v1/tenants/" + id
	key := a.call(http.MethodPost, base+"/api-keys", a.token(owner, mfa()), map[string]any{"name": "ci", "permissions": []string{"team:manage", "member:read"}})
	secret := key.str("secret")
	tid, _ := idgen.ParseAs(idgen.Tenant, id)
	if r := a.call(http.MethodGet, base+"/members", secret, nil); r.status != http.StatusOK {
		t.Fatalf("key before the closure = %d %v", r.status, r.body)
	}
	if _, err := tenant.StartClosure(context.Background(), a.db.Pool, scope.Scope{Brand: b.ID, Tenant: tid},
		events.Actor{Type: events.ActorService, ID: "test"}, "customer left", "test"); err != nil {
		t.Fatal(err)
	}
	tok := a.token(owner, mfa())
	for name, r := range map[string]reply{
		"member read":  a.call(http.MethodGet, base, tok, nil),
		"member write": a.call(http.MethodPatch, base, tok, map[string]any{"name": "x"}),
		"key read":     a.call(http.MethodGet, base+"/members", secret, nil),
		"key write":    a.call(http.MethodPost, base+"/teams", secret, map[string]any{"name": "Ops"}),
	} {
		if r.status != http.StatusNotFound || r.code() != "tenant_not_found" {
			t.Errorf("%s of a closing tenant = %d %v, want tenant_not_found", name, r.status, r.body)
		}
	}
}
