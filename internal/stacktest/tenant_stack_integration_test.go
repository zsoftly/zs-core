//go:build integration

package stacktest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/httpapi"
	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/apikey"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// deadURL is a loopback port nothing listens on.
const deadURL = "http://127.0.0.1:9"

func newTenantAPI(t *testing.T, s stack, db *postgrestest.DB, keysURL string, eng tenant.Engines) apiServer {
	t.Helper()
	reg := brand.NewRegistry(db.Pool, time.Second, nil)
	srv := httptest.NewServer(httpapi.New(httpapi.Options{
		DB: db.Pool, Migrations: migrations.FS, Brands: reg, Tenants: eng,
		Verifier: &identity.Verifier{Brands: reg, Keys: keycloak.NewKeySet(keysURL, keycloak.KeySetOptions{})},
	}))
	t.Cleanup(srv.Close)
	return apiServer{srv}
}

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	return []byte(mustJSON(v))
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// acceptAs accepts an invitation as the identity a real token recorded. A
// password-grant token of Keycloak 26.8.1 carries no auth_time, so the HTTP
// route refuses it with recent_auth_required; the library call states the
// recent authentication instead.
func acceptAs(t *testing.T, db *postgrestest.DB, brandID idgen.UUID, issuer string, tok tokenSet, token string) (tenant.Membership, error) {
	t.Helper()
	ctx := context.Background()
	ident, err := identity.Get(ctx, db.Pool, brandID, issuer, tok.subject)
	if err != nil {
		t.Fatal(err)
	}
	var m tenant.Membership
	err = pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		var err error
		_, m, err = tenant.AcceptInvitation(ctx, tx, tenant.Acceptor{Brand: brandID, IdentityID: ident.ID, Email: ident.Email,
			EmailVerified: ident.EmailVerified, RecentAuth: true, CorrelationID: "stack-test"}, token)
		return err
	})
	return m, err
}

func errName(err error) string {
	var e *zserrors.Error
	if err == nil {
		return "ok"
	}
	if !errors.As(err, &e) {
		return "untyped"
	}
	return zserrors.Lookup(e.Code).Name
}

// stackCaller is the caller a library user builds for a person's membership,
// with recent MFA. The password grant the stack tests use cannot carry a
// second factor, so the operations that need one run through the library.
func stackCaller(t *testing.T, db *postgrestest.DB, brandID idgen.UUID, sc scope.Scope, issuer, subject string) tenant.Caller {
	t.Helper()
	ctx := context.Background()
	ident, err := identity.Get(ctx, db.Pool, brandID, issuer, subject)
	if err != nil {
		t.Fatal(err)
	}
	m, err := tenant.MembershipOf(ctx, db.Pool, sc, ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	perms, m, err := tenant.Effective(ctx, db.Pool, sc, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tenant.Caller{Membership: m, Permissions: perms, RecentMFA: true,
		Actor: events.Actor{Type: events.ActorUser, ID: ident.PublicID()}, CorrelationID: "stack-test"}
}

// The tenant gates of Phase 3 on the reference stack, with real tokens, a
// real Keycloak and a real Kill Bill.
func TestTenantsOnTheStack(t *testing.T) {
	s := newStack(t)
	db := postgrestest.New(t)
	deps := s.deps(t, db)
	ctx := context.Background()

	ref, err := brand.Load(filepath.Join(brandsDir(), "zs-reference.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	refDiff := apply(t, deps, devVariant(t, ref))
	refID, _ := idgen.ParseAs(idgen.Brand, refDiff.BrandID)
	t.Cleanup(func() { _, _ = brand.Apply(context.Background(), deps, ref, cli) })
	second := secondManifest(t, ref)
	secondDiff := apply(t, deps, second)
	secondID, _ := idgen.ParseAs(idgen.Brand, secondDiff.BrandID)
	if realm, err := s.kc.GetRealm(ctx, "zs-reference"); err != nil || realm["organizationsEnabled"] != true {
		t.Fatalf("organizations are not enabled in the realm: %v", err)
	}

	engines := tenant.Engines{Keycloak: s.kc, KillBill: s.kb, Secrets: secrets.Default{}}
	deadKB, err := killbill.New(killbill.Config{URL: deadURL, AdminUser: "admin", AdminPassword: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	api := newTenantAPI(t, s, db, s.kcURL, engines)
	apiNoKillBill := newTenantAPI(t, s, db, s.kcURL, tenant.Engines{Keycloak: s.kc, KillBill: deadKB, Secrets: secrets.Default{}})

	// The random tail of a UUIDv7: its first digits are a timestamp and
	// repeat between runs within a minute.
	suffix := strings.ToLower(idgen.MustNewV7().String()[28:])
	owner, member, other := "owner-"+suffix, "member-"+suffix, "other-"+suffix
	createUser(t, s.kc, "zs-reference", owner, "pw-"+suffix)
	createUser(t, s.kc, "zs-reference", member, "pw-"+suffix)
	createUser(t, s.kc, "zs-reference", other, "pw-"+suffix)
	createUser(t, s.kc, "zs-second", owner, "pw-"+suffix)
	refIssuer := strings.TrimRight(s.kcURL, "/") + "/realms/zs-reference"
	ownerTok := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", owner, "pw-"+suffix)
	memberTok := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", member, "pw-"+suffix)
	otherTok := passwordToken(t, s.kcURL, "zs-reference", "zs-customer", "", other, "pw-"+suffix)
	secondTok := passwordToken(t, s.kcURL, "zs-second", "zs-second-customer", "", owner, "pw-"+suffix)
	// The first request with each token records its identity.
	for _, tok := range []tokenSet{ownerTok, memberTok, otherTok, secondTok} {
		if status, out := api.call(t, http.MethodGet, "/v1/me", tok.access, nil, ""); status != http.StatusOK {
			t.Fatalf("/v1/me = %d %s", status, mustJSON(out))
		}
	}

	// JWKS unreachable: 503 authentication_unavailable, never 401.
	if status, out := newTenantAPI(t, s, db, deadURL, engines).call(t, http.MethodGet, "/v1/me", ownerTok.access, nil, ""); status != http.StatusServiceUnavailable || errCode(out) != "authentication_unavailable" {
		t.Fatalf("JWKS unreachable = %d %s", status, mustJSON(out))
	}

	// Kill Bill unreachable: no tenant, an audited failure, and the sweeper
	// completes the same tenant later. Both engines carry its ID.
	create := map[string]any{"idempotency_key": "stack-create-" + suffix, "name": "Stack Tenant " + suffix, "billing_country": "CA"}
	status, out := apiNoKillBill.call(t, http.MethodPost, "/v1/tenants", ownerTok.access, jsonBody(t, create), "application/json")
	if status != http.StatusServiceUnavailable || errCode(out) != "billing_unavailable" {
		t.Fatalf("create with Kill Bill down = %d %s", status, mustJSON(out))
	}
	var pendingID idgen.UUID
	if err := db.Pool.QueryRow(ctx, `SELECT tenant_id FROM tenant_provisioning WHERE status = 'pending'`).Scan(&pendingID); err != nil {
		t.Fatalf("pending creation: %v", err)
	}
	pendingPublic := idgen.Format(idgen.Tenant, pendingID)
	if _, err := tenant.Get(ctx, db.Pool, scope.Scope{Brand: refID, Tenant: pendingID}); err == nil {
		t.Fatal("a tenant row exists after the failure")
	}
	org, err := s.kc.FindOrganizationByAttribute(ctx, "zs-reference", tenant.OrganizationTenantAttribute, pendingPublic)
	if err != nil || org.Alias != pendingPublic {
		t.Fatalf("organization before the sweep = %+v %v", org, err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	report, err := (&tenant.Sweeper{DB: db.Pool, Engines: engines}).SweepOnce(ctx)
	if err != nil || report.Completed != 1 {
		t.Fatalf("sweep = %+v %v", report, err)
	}
	refCreds := killbill.TenantCredentials{APIKey: "zs-reference", APISecret: os.Getenv(refKillBillSecretVar)}
	tn, err := tenant.Get(ctx, db.Pool, scope.Scope{Brand: refID, Tenant: pendingID})
	if err != nil || tn.KeycloakOrganizationID != org.ID {
		t.Fatalf("tenant after the sweep = %+v %v", tn, err)
	}
	account, err := s.kb.AccountByExternalKey(ctx, refCreds, pendingPublic)
	if err != nil || account.AccountID != tn.KillBillAccountID || account.Currency != "CAD" {
		t.Fatalf("Kill Bill account = %+v %v", account, err)
	}
	// Idempotent creation: the same key and payload return that tenant, a
	// different payload is 409.
	status, out = api.call(t, http.MethodPost, "/v1/tenants", ownerTok.access, jsonBody(t, create), "application/json")
	if status != http.StatusCreated || out["id"] != pendingPublic {
		t.Fatalf("retry = %d %s", status, mustJSON(out))
	}
	create["name"] = "Something else"
	if status, out = api.call(t, http.MethodPost, "/v1/tenants", ownerTok.access, jsonBody(t, create), "application/json"); status != http.StatusConflict || errCode(out) != "idempotency_key_conflict" {
		t.Fatalf("different payload = %d %s", status, mustJSON(out))
	}
	// A Kill Bill call for brand zs-reference uses only its credentials.
	secondCreds := killbill.TenantCredentials{APIKey: "zs-second", APISecret: secondKillBillSecret}
	if _, err := s.kb.AccountByExternalKey(ctx, secondCreds, pendingPublic); err == nil {
		t.Fatal("the zs-reference tenant's account is visible in the zs-second Kill Bill tenant")
	}

	// Invitations: another verified email fails, the same email in another
	// case succeeds.
	sc := tn.Scope()
	base := "/v1/tenants/" + pendingPublic
	inviteBody := jsonBody(t, map[string]any{"email": strings.ToUpper(member) + "@Example.TEST", "role": "admin"})
	status, inv := api.call(t, http.MethodPost, base+"/invitations", ownerTok.access, inviteBody, "application/json")
	if status != http.StatusCreated {
		t.Fatalf("invite = %d %s", status, mustJSON(inv))
	}
	token, _ := inv["token"].(string)
	if status, out = api.call(t, http.MethodPost, "/v1/invitations/accept", otherTok.access, jsonBody(t, map[string]any{"token": token}), "application/json"); status != http.StatusForbidden || errCode(out) != "recent_auth_required" {
		t.Fatalf("accept with a password-grant token, which has no auth_time = %d %s", status, mustJSON(out))
	}
	if _, err := acceptAs(t, db, refID, refIssuer, otherTok, token); errName(err) != "invitation_email_mismatch" {
		t.Fatalf("accept with another email: %v", err)
	}
	// Brand zs-second cannot read, create in or accept into zs-reference.
	if _, err := acceptAs(t, db, secondID, strings.TrimRight(s.kcURL, "/")+"/realms/zs-second", secondTok, token); errName(err) != "invitation_not_found" {
		t.Fatalf("accept from another brand: %v", err)
	}
	if status, out = api.call(t, http.MethodGet, base, secondTok.access, nil, ""); status != http.StatusNotFound || errCode(out) != "tenant_not_found" {
		t.Fatalf("read from another brand = %d %s", status, mustJSON(out))
	}
	if status, out = api.call(t, http.MethodPost, base+"/teams", secondTok.access, jsonBody(t, map[string]any{"name": "x"}), "application/json"); status != http.StatusNotFound {
		t.Fatalf("create in another brand = %d %s", status, mustJSON(out))
	}
	status, out = api.call(t, http.MethodPost, "/v1/tenants", secondTok.access, jsonBody(t, map[string]any{"idempotency_key": "stack-second-" + suffix, "name": "Second " + suffix}), "application/json")
	if status != http.StatusCreated || out["brand_id"] != secondDiff.BrandID {
		t.Fatalf("zs-second creates in its own brand = %d %s", status, mustJSON(out))
	}
	secondTenant := out["id"].(string)
	if _, err := s.kb.AccountByExternalKey(ctx, refCreds, secondTenant); err == nil {
		t.Fatal("the zs-second tenant's account is in the zs-reference Kill Bill tenant")
	}
	if _, err := s.kb.AccountByExternalKey(ctx, secondCreds, secondTenant); err != nil {
		t.Fatalf("zs-second account: %v", err)
	}
	accepted, err := acceptAs(t, db, refID, refIssuer, memberTok, token)
	if err != nil {
		t.Fatalf("accept with the same email in another case: %v", err)
	}
	memberMem := accepted.ID

	// The member's live token and an API key it created both work, then
	// both fail on the next request once the member is removed. The key
	// shrinks first when the member is demoted.
	memberCaller := stackCaller(t, db, refID, sc, refIssuer, memberTok.subject)
	var key apikey.Created
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		var err error
		key, err = apikey.Create(ctx, tx, sc, memberCaller, apikey.CreateRequest{Name: "stack", Permissions: []string{"tenant:read", "team:manage"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status, _ = api.call(t, http.MethodPost, base+"/teams", key.Secret, jsonBody(t, map[string]any{"name": "Ops " + suffix}), "application/json"); status != http.StatusCreated {
		t.Fatalf("key creates a team = %d", status)
	}
	ownerCaller := stackCaller(t, db, refID, sc, refIssuer, ownerTok.subject)
	demoted := tenant.RoleMember
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, ownerCaller, memberMem, tenant.MemberChange{Role: &demoted})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status, out = api.call(t, http.MethodPost, base+"/teams", key.Secret, jsonBody(t, map[string]any{"name": "Ops 2 " + suffix}), "application/json"); status != http.StatusForbidden || errCode(out) != "permission_denied" {
		t.Fatalf("key after demotion = %d %s", status, mustJSON(out))
	}
	for _, cred := range []string{memberTok.access, key.Secret} {
		if status, out = api.call(t, http.MethodGet, base, cred, nil, ""); status != http.StatusOK {
			t.Fatalf("before removal = %d %s", status, mustJSON(out))
		}
	}
	if report, err = (&tenant.Sweeper{DB: db.Pool, Engines: engines}).SweepOnce(ctx); err != nil || report.Errors != 0 {
		t.Fatalf("sweep before removal = %+v %v", report, err)
	}
	orgMembers := func() []string {
		t.Helper()
		list, err := s.kc.OrganizationMembers(ctx, "zs-reference", tn.KeycloakOrganizationID)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, m := range list {
			ids = append(ids, m.ID)
		}
		return ids
	}
	if got := orgMembers(); !slices.Contains(got, memberTok.subject) || !slices.Contains(got, ownerTok.subject) {
		t.Fatalf("organization members before removal = %v", got)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, sc, ownerCaller, memberMem)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status, out = api.call(t, http.MethodGet, base, memberTok.access, nil, ""); status != http.StatusNotFound || errCode(out) != "tenant_not_found" {
		t.Fatalf("removed member's live token = %d %s", status, mustJSON(out))
	}
	// Removal revokes the key itself.
	if status, out = api.call(t, http.MethodGet, base, key.Secret, nil, ""); status != http.StatusUnauthorized || errCode(out) != "api_key_invalid" {
		t.Fatalf("removed member's live key = %d %s", status, mustJSON(out))
	}
	if report, err = (&tenant.Sweeper{DB: db.Pool, Engines: engines}).SweepOnce(ctx); err != nil || report.MembersRemoved < 1 {
		t.Fatalf("sweep after removal = %+v %v", report, err)
	}
	if got := orgMembers(); slices.Contains(got, memberTok.subject) || !slices.Contains(got, ownerTok.subject) {
		t.Fatalf("organization members after the sweep = %v", got)
	}
	// Invited again, the person gets a new membership and the old key stays
	// revoked.
	var again string
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		var err error
		_, again, err = tenant.CreateInvitation(ctx, tx, sc, ownerCaller, member+"@example.test", tenant.RoleAdmin)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	readmitted, err := acceptAs(t, db, refID, refIssuer, memberTok, again)
	if err != nil || readmitted.ID == memberMem {
		t.Fatalf("re-acceptance = %s %v, removed membership %s", readmitted.ID, err, memberMem)
	}
	if status, out = api.call(t, http.MethodGet, base, memberTok.access, nil, ""); status != http.StatusOK {
		t.Fatalf("re-admitted member's token = %d %s", status, mustJSON(out))
	}
	if status, out = api.call(t, http.MethodGet, base, key.Secret, nil, ""); status != http.StatusUnauthorized || errCode(out) != "api_key_invalid" {
		t.Fatalf("old key after re-acceptance = %d %s", status, mustJSON(out))
	}

	// Staff of zs-second with an activation grant act in zs-second only.
	createUser(t, s.kc, "zs-second", "staff-"+suffix, "pw-"+suffix)
	secStaff := passwordToken(t, s.kcURL, "zs-second", "zs-second-staff", secondStaffSecret, "staff-"+suffix, "pw-"+suffix)
	grant(t, db, secondID, strings.TrimRight(s.kcURL, "/")+"/realms/zs-second", secStaff.subject, identity.StaffActivation)
	decision := jsonBody(t, map[string]any{"state": "approved", "reason": "stack test", "request_id": "stack-activation-" + suffix})
	if status, out = api.call(t, http.MethodPost, "/v1/staff/tenants/"+pendingPublic+"/activation", secStaff.access, decision, "application/json"); status != http.StatusNotFound {
		t.Fatalf("zs-second staff on a zs-reference tenant = %d %s", status, mustJSON(out))
	}
	if status, out = api.call(t, http.MethodPost, "/v1/staff/tenants/"+secondTenant+"/activation", secStaff.access, decision, "application/json"); status != http.StatusCreated {
		t.Fatalf("zs-second staff on its own tenant = %d %s", status, mustJSON(out))
	}

	// Suspension blocks the Kill Bill account and disables the Organization;
	// reads continue and writes stop. Closing closes the account.
	operator := events.Actor{Type: events.ActorService, ID: "zs-core-cli:stack-test"}
	if _, err := tenant.Suspend(ctx, db.Pool, engines, sc, operator, "stack test", "stack-test"); err != nil {
		t.Fatal(err)
	}
	if states := blockingStates(t, s, refCreds, tn.KillBillAccountID); !slices.Contains(states, tenant.BlockingSuspended+":true") {
		t.Fatalf("blocking states after suspension = %v", states)
	}
	if org, err := s.kc.GetOrganization(ctx, "zs-reference", tn.KeycloakOrganizationID); err != nil || org.Enabled {
		t.Fatalf("organization after suspension = %+v %v", org, err)
	}
	if status, _ = api.call(t, http.MethodGet, base, ownerTok.access, nil, ""); status != http.StatusOK {
		t.Fatalf("read of a suspended tenant = %d", status)
	}
	if status, out = api.call(t, http.MethodPatch, base, ownerTok.access, jsonBody(t, map[string]any{"name": "x"}), "application/json"); status != http.StatusNotFound {
		t.Fatalf("write to a suspended tenant = %d %s", status, mustJSON(out))
	}
	if _, err := tenant.Reactivate(ctx, db.Pool, engines, sc, operator, "stack test", "stack-test"); err != nil {
		t.Fatal(err)
	}
	if states := blockingStates(t, s, refCreds, tn.KillBillAccountID); !slices.Contains(states, tenant.BlockingActive+":false") {
		t.Fatalf("blocking states after reactivation = %v", states)
	}
	if org, err := s.kc.GetOrganization(ctx, "zs-reference", tn.KeycloakOrganizationID); err != nil || !org.Enabled {
		t.Fatalf("organization after reactivation = %+v %v", org, err)
	}
	if _, err := tenant.Close(ctx, db.Pool, engines, sc, operator, "stack test", "stack-test"); err != nil {
		t.Fatal(err)
	}
	if states := blockingStates(t, s, refCreds, tn.KillBillAccountID); !slices.Contains(states, "CLOSE_ACCOUNT:false") {
		t.Fatalf("blocking states after closure = %v", states)
	}
	if status, _ = api.call(t, http.MethodGet, base, ownerTok.access, nil, ""); status != http.StatusNotFound {
		t.Fatalf("read of a closed tenant = %d", status)
	}
	if _, err := tenant.Reactivate(ctx, db.Pool, engines, sc, operator, "stack test", "stack-test"); errName(err) != "tenant_closed" {
		t.Fatalf("reactivating a closed tenant: %v", err)
	}
}

// blockingStates reads the account blocking states from Kill Bill as
// "<state>:<blocks billing>". Core itself never reads them.
func blockingStates(t *testing.T, s stack, creds killbill.TenantCredentials, accountID string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, os.Getenv("ZS_TEST_KILLBILL_URL")+"/1.0/kb/accounts/"+accountID+"/block?blockingStateTypes=ACCOUNT", nil)
	req.SetBasicAuth("admin", os.Getenv("ZS_TEST_KILLBILL_ADMIN_PASSWORD"))
	creds.Apply(req.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []killbill.BlockingState
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("blocking states: HTTP %d %v", resp.StatusCode, err)
	}
	var out []string
	for _, b := range list {
		out = append(out, b.StateName+":"+strconv.FormatBool(b.IsBlockBilling))
	}
	return out
}
