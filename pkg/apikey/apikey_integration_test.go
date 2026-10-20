//go:build integration

package apikey_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/apikey"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func code(err error) string {
	if err == nil {
		return "ok"
	}
	var e *zserrors.Error
	if !errors.As(err, &e) {
		return "untyped: " + err.Error()
	}
	return zserrors.Lookup(e.Code).Name
}

type fixture struct {
	t      *testing.T
	db     *postgrestest.DB
	brand  enginetest.Brand
	tenant tenant.Tenant
	owner  enginetest.Person
	eng    tenant.Engines
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := postgrestest.New(t)
	fake := enginetest.New(t)
	b := fake.InsertBrand(t, db.Pool, "zs-keys", "vm:create")
	owner := enginetest.InsertPerson(t, db.Pool, b, "owner@example.test")
	eng := tenant.Engines{Keycloak: fake.Keycloak(t), KillBill: fake.KillBill(t), Secrets: fake.Secrets()}
	tn, err := tenant.CreateOrganization(context.Background(), db.Pool, eng, tenant.CreateRequest{
		Brand: b.ID, Creator: owner.Identity.ID, Name: "Keys", IdempotencyKey: "keys-create-1",
		Actor: events.Actor{Type: events.ActorUser, ID: owner.Identity.PublicID()}, CorrelationID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, db: db, brand: b, tenant: tn, owner: owner, eng: eng}
}

func (f *fixture) caller(p enginetest.Person, mfa bool) tenant.Caller {
	f.t.Helper()
	sc := f.tenant.Scope()
	m, err := tenant.MembershipOf(context.Background(), f.db.Pool, sc, p.Identity.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	perms, m, err := tenant.Effective(context.Background(), f.db.Pool, sc, m.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return tenant.Caller{Membership: m, Permissions: perms, RecentMFA: mfa,
		Actor: events.Actor{Type: events.ActorUser, ID: p.Identity.PublicID()}, CorrelationID: "test"}
}

func (f *fixture) tx(fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(context.Background(), f.db.Pool, fn)
}

func (f *fixture) create(c tenant.Caller, req apikey.CreateRequest) (apikey.Created, error) {
	var out apikey.Created
	err := f.tx(func(tx pgx.Tx) error {
		var err error
		out, err = apikey.Create(context.Background(), tx, f.tenant.Scope(), c, req)
		return err
	})
	return out, err
}

func (f *fixture) verify(secret string) (apikey.Verified, error) {
	var v apikey.Verified
	err := f.tx(func(tx pgx.Tx) error {
		var err error
		v, err = apikey.Verify(context.Background(), tx, secret)
		return err
	})
	return v, err
}

// join adds a person to the tenant with role, through an invitation.
func (f *fixture) join(p enginetest.Person, role tenant.Role) tenant.Membership {
	f.t.Helper()
	owner := f.caller(f.owner, true)
	var m tenant.Membership
	err := f.tx(func(tx pgx.Tx) error {
		_, token, err := tenant.CreateInvitation(context.Background(), tx, f.tenant.Scope(), owner, p.Email, role)
		if err != nil {
			return err
		}
		_, m, err = tenant.AcceptInvitation(context.Background(), tx, tenant.Acceptor{
			Brand: f.brand.ID, IdentityID: p.Identity.ID, Email: p.Email, EmailVerified: true, RecentAuth: true, CorrelationID: "test",
		}, token)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func TestCreateRules(t *testing.T) {
	f := newFixture(t)
	owner := f.caller(f.owner, true)
	if _, err := f.create(f.caller(f.owner, false), apikey.CreateRequest{Name: "ci", Permissions: []string{"tenant:read"}}); code(err) != "recent_mfa_required" {
		t.Fatalf("without MFA: %v", err)
	}
	if _, err := f.create(owner, apikey.CreateRequest{Name: "ci", Permissions: []string{"vm:delete"}}); code(err) != "permission_unknown" {
		t.Fatalf("unregistered permission: %v", err)
	}
	if _, err := f.create(owner, apikey.CreateRequest{Name: "ci", Permissions: []string{"tenant:read"}, ExpiresAt: time.Now().Add(366 * 24 * time.Hour)}); code(err) != "validation_failed" {
		t.Fatalf("366 days: %v", err)
	}
	member := enginetest.InsertPerson(t, f.db.Pool, f.brand, "member@example.test")
	f.join(member, tenant.RoleMember)
	if _, err := f.create(f.caller(member, true), apikey.CreateRequest{Name: "ci", Permissions: []string{"tenant:read"}}); code(err) != "permission_denied" {
		t.Fatalf("member without apikey:create: %v", err)
	}
	admin := enginetest.InsertPerson(t, f.db.Pool, f.brand, "admin@example.test")
	f.join(admin, tenant.RoleAdmin)
	if _, err := f.create(f.caller(admin, true), apikey.CreateRequest{Name: "ci", Permissions: []string{"billing:read"}}); code(err) != "permission_denied" {
		t.Fatalf("admin asking for billing: %v", err)
	}

	created, err := f.create(owner, apikey.CreateRequest{Name: "ci", Permissions: []string{"tenant:read", "vm:create", "tenant:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, "zs_zs-keys_") || len(created.Secret) != len("zs_zs-keys_")+43 ||
		!strings.HasPrefix(created.Secret, created.Prefix) || strings.Join(created.Permissions, ",") != "tenant:read,vm:create" {
		t.Fatalf("created = %+v", created.Key)
	}
	if d := time.Until(created.ExpiresAt); d < 89*24*time.Hour || d > 90*24*time.Hour {
		t.Fatalf("default expiry in %s", d)
	}
	var stored []byte
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT digest FROM api_keys WHERE id = $1`, created.ID).Scan(&stored); err != nil ||
		len(stored) != 32 || strings.Contains(string(stored), created.Secret) {
		t.Fatalf("stored digest: %d bytes, %v", len(stored), err)
	}
	var auditDetails string
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT details::text FROM audit_events WHERE action = 'tenant.api_key.created'`).Scan(&auditDetails); err != nil ||
		strings.Contains(auditDetails, created.Secret) {
		t.Fatalf("audit holds the secret or is missing: %v", err)
	}
	list, _, err := apikey.List(context.Background(), f.db.Pool, f.tenant.Scope(), owner, tenant.Page{})
	if err != nil || len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
}

// Effective permissions at use are the key's permissions intersected with
// the creator's current ones: they shrink when the creator is demoted and the
// key stops when the creator is removed.
func TestVerifyFollowsTheCreator(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	adminP := enginetest.InsertPerson(t, f.db.Pool, f.brand, "admin@example.test")
	adminM := f.join(adminP, tenant.RoleAdmin)
	admin := f.caller(adminP, true)
	created, err := f.create(admin, apikey.CreateRequest{Name: "deploy", Permissions: []string{"tenant:read", "member:update", "vm:create"}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := f.verify(created.Secret)
	if err != nil || v.Key.ID != created.ID || v.Scope() != f.tenant.Scope() || v.BrandSlug != "zs-keys" ||
		v.Membership.ID != adminM.ID || strings.Join(v.Permissions, ",") != "member:update,tenant:read,vm:create" {
		t.Fatalf("verify = %+v %v", v, err)
	}
	// last_used_at is written on use, and at most once a minute.
	var first, second *time.Time
	_ = f.db.Pool.QueryRow(ctx, `SELECT last_used_at FROM api_keys WHERE id = $1`, created.ID).Scan(&first)
	if first == nil {
		t.Fatal("last_used_at not written")
	}
	if _, err := f.verify(created.Secret); err != nil {
		t.Fatal(err)
	}
	_ = f.db.Pool.QueryRow(ctx, `SELECT last_used_at FROM api_keys WHERE id = $1`, created.ID).Scan(&second)
	if second == nil || !first.Equal(*second) {
		t.Fatalf("last_used_at moved within a minute: %s -> %s", first, second)
	}

	owner := f.caller(f.owner, true)
	member := tenant.RoleMember
	if err := f.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, f.tenant.Scope(), owner, adminM.ID, tenant.MemberChange{Role: &member})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	v, err = f.verify(created.Secret)
	if err != nil || strings.Join(v.Permissions, ",") != "tenant:read" {
		t.Fatalf("after demotion = %v %v", v.Permissions, err)
	}
	if err := f.tx(func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, f.tenant.Scope(), owner, adminM.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Removal revokes the key, so it is invalid rather than orphaned.
	if _, err := f.verify(created.Secret); code(err) != "api_key_invalid" {
		t.Fatalf("after removal: %v", err)
	}
}

func TestVerifyRefusesBadKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	owner := f.caller(f.owner, true)
	created, err := f.create(owner, apikey.CreateRequest{Name: "ci", Permissions: []string{"tenant:read"}})
	if err != nil {
		t.Fatal(err)
	}
	random := strings.TrimPrefix(created.Secret, "zs_zs-keys_")
	for name, secret := range map[string]string{
		"malformed":      "zs_zs-keys_short",
		"unknown":        "zs_zs-keys_" + strings.Repeat("A", 43),
		"another brand":  "zs_zs-other_" + random,
		"not an API key": "eyJhbGciOiJSUzI1NiJ9.e30.sig",
	} {
		if _, err := f.verify(secret); code(err) != "api_key_invalid" {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Revocation is immediate and audited.
	if err := f.tx(func(tx pgx.Tx) error {
		_, err := apikey.Revoke(ctx, tx, f.tenant.Scope(), owner, created.ID, "rotated")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(created.Secret); code(err) != "api_key_invalid" {
		t.Fatalf("revoked: %v", err)
	}
	expired, err := f.create(owner, apikey.CreateRequest{Name: "short", Permissions: []string{"tenant:read"}, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx, `ALTER TABLE api_keys DROP CONSTRAINT api_keys_expiry`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(expired.Secret); code(err) != "api_key_invalid" {
		t.Fatalf("expired: %v", err)
	}
	// A key is not found through another tenant.
	other := scope.Scope{Brand: f.brand.ID, Tenant: f.tenant.ID}
	other.Tenant[15] ^= 0xff
	if err := f.tx(func(tx pgx.Tx) error {
		_, err := apikey.Revoke(ctx, tx, other, owner, created.ID, "")
		return err
	}); code(err) != "not_found" {
		t.Fatalf("revoke through another tenant: %v", err)
	}
	var actions []string
	rows, _ := f.db.Pool.Query(ctx, `SELECT action FROM audit_events WHERE action LIKE 'tenant.api_key.%' ORDER BY occurred_at`)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	if !slices.Contains(actions, "tenant.api_key.revoked") {
		t.Fatalf("audit = %v", actions)
	}
}

func (f *fixture) revokedReason(id idgen.UUID) string {
	f.t.Helper()
	var reason *string
	if err := f.db.Pool.QueryRow(context.Background(), `SELECT revoked_reason FROM api_keys WHERE id = $1`, id).Scan(&reason); err != nil {
		f.t.Fatal(err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

// A removed member's keys are revoked with the removal. When the person is
// invited again and accepts, they get a new membership, and the old key
// stays revoked: Verify never brings it back. Suspension revokes keys the
// same way, and reactivation does not restore them. Closing the tenant
// revokes every key.
func TestCoreRevokesKeysOfRemovedSuspendedAndClosed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sc := f.tenant.Scope()
	owner := f.caller(f.owner, true)
	adminP := enginetest.InsertPerson(t, f.db.Pool, f.brand, "admin@example.test")
	first := f.join(adminP, tenant.RoleAdmin)
	old, err := f.create(f.caller(adminP, true), apikey.CreateRequest{Name: "deploy", Permissions: []string{"tenant:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.tx(func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, sc, owner, first.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.revokedReason(old.ID); got != tenant.KeyRevokedCreatorRemoved {
		t.Fatalf("revoked_reason after removal = %q", got)
	}
	if _, err := f.verify(old.Secret); code(err) != "api_key_invalid" {
		t.Fatalf("after removal: %v", err)
	}

	second := f.join(adminP, tenant.RoleAdmin)
	if second.ID == first.ID {
		t.Fatal("re-acceptance reused the removed membership")
	}
	var rows int
	if err := f.db.Pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE identity_id = $1`, adminP.Identity.ID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("membership rows = %d %v", rows, err)
	}
	if m, err := tenant.GetMembership(ctx, f.db.Pool, sc, first.ID); err != nil || m.Status != tenant.MembershipRemoved {
		t.Fatalf("old membership = %+v %v", m, err)
	}
	if _, err := f.verify(old.Secret); code(err) != "api_key_invalid" {
		t.Fatalf("old key after re-acceptance: %v", err)
	}

	suspendedKey, err := f.create(f.caller(adminP, true), apikey.CreateRequest{Name: "ops", Permissions: []string{"tenant:read"}})
	if err != nil {
		t.Fatal(err)
	}
	suspended, active := tenant.MembershipSuspended, tenant.MembershipActive
	for _, status := range []*tenant.MembershipStatus{&suspended, &active} {
		if err := f.tx(func(tx pgx.Tx) error {
			_, err := tenant.UpdateMember(ctx, tx, sc, owner, second.ID, tenant.MemberChange{Status: status})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.revokedReason(suspendedKey.ID); got != tenant.KeyRevokedCreatorSuspended {
		t.Fatalf("revoked_reason after suspension = %q", got)
	}
	if _, err := f.verify(suspendedKey.Secret); code(err) != "api_key_invalid" {
		t.Fatalf("after suspension and reactivation: %v", err)
	}

	ownerKey, err := f.create(owner, apikey.CreateRequest{Name: "owner", Permissions: []string{"tenant:read"}})
	if err != nil {
		t.Fatal(err)
	}
	operator := events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}
	if _, err := tenant.Close(ctx, f.db.Pool, f.eng, sc, operator, "customer left", "test"); err != nil {
		t.Fatal(err)
	}
	if got := f.revokedReason(ownerKey.ID); got != tenant.KeyRevokedTenantClosed {
		t.Fatalf("revoked_reason after closure = %q", got)
	}
	var audited int
	if err := f.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'tenant.api_key.revoked'
AND reason IN ('creator_removed', 'creator_suspended', 'tenant_closed')`).Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("revocations audited = %d %v", audited, err)
	}
}
