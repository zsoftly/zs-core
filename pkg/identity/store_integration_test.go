//go:build integration

package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

func insertBrand(t *testing.T, db *postgrestest.DB, slug string) idgen.UUID {
	t.Helper()
	id := idgen.MustNewV7()
	_, err := db.Pool.Exec(context.Background(), `INSERT INTO brands (id, slug, name, keycloak_realm, killbill_api_key,
    killbill_api_secret_ref, manifest, manifest_digest)
VALUES ($1, $2, $2, $2, $2, 'env://X', '{}', 'sha256:' || repeat('0', 64))`, id, slug)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUpsertIdentity(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	brandID := insertBrand(t, db, "zs-a")
	p := identity.Principal{Brand: identity.BrandConfig{ID: brandID}, Kind: identity.KindUser, Issuer: "http://kc/realms/zs-a",
		Subject: "sub-1", Email: "first@example.test", EmailVerified: true, Name: "First"}

	upsert := func(p identity.Principal) (identity.Identity, bool) {
		t.Helper()
		var ident identity.Identity
		var ok bool
		err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
			var err error
			ident, ok, err = identity.Upsert(ctx, tx, p, "corr")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return ident, ok
	}
	first, ok := upsert(p)
	if !ok || first.ID.IsNil() || first.Email != "first@example.test" {
		t.Fatalf("first upsert = %+v %v", first, ok)
	}
	again, _ := upsert(p)
	if again.ID != first.ID {
		t.Fatal("second contact created a new identity")
	}
	p.Email = "changed@example.test"
	changed, _ := upsert(p)
	if changed.ID != first.ID || changed.Email != "changed@example.test" {
		t.Fatalf("email refresh = %+v", changed)
	}
	unverified := p
	unverified.Subject, unverified.EmailVerified = "sub-2", false
	if _, ok := upsert(unverified); ok {
		t.Fatal("an unverified email created an identity")
	}
	if _, err := identity.Get(ctx, db.Pool, brandID, p.Issuer, "sub-2"); !zserrors.IsCode(err, zserrors.CodeNotFound) {
		t.Fatalf("unverified identity stored: %v", err)
	}
	page, err := audit.QueryBrand(ctx, db.Pool, brandID, audit.Filter{Action: "identity.created"})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("identity.created events = %d, %v", len(page.Events), err)
	}
}

func TestStaffGrantsAreBrandScoped(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	a := insertBrand(t, db, "zs-a")
	b := insertBrand(t, db, "zs-b")
	const issuer, subject = "http://kc/realms/zs-b", "staff-1"
	add := func(g identity.StaffGrant) error {
		return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
			_, err := identity.AddStaffGrant(ctx, tx, g, events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}, "corr")
			return err
		})
	}
	grant := identity.StaffGrant{BrandID: b, Issuer: issuer, Subject: subject, Scope: identity.StaffOperator, GrantedBy: "ops", Reason: "on call"}
	if err := add(grant); err != nil {
		t.Fatal(err)
	}
	if err := add(grant); !zserrors.IsCode(err, zserrors.CodeConflict) {
		t.Fatalf("duplicate grant: %v", err)
	}
	if err := add(identity.StaffGrant{BrandID: b, Issuer: issuer, Subject: subject, Scope: "root", GrantedBy: "ops", Reason: "x"}); !zserrors.IsCode(err, zserrors.CodeValidation) {
		t.Fatalf("unknown scope: %v", err)
	}
	if err := add(identity.StaffGrant{BrandID: b, Issuer: issuer, Subject: subject, Scope: identity.StaffBilling, GrantedBy: "ops"}); !zserrors.IsCode(err, zserrors.CodeValidation) {
		t.Fatalf("missing reason: %v", err)
	}

	staffB := identity.Principal{Brand: identity.BrandConfig{ID: b}, Kind: identity.KindStaff, Issuer: issuer, Subject: subject}
	check := func(p identity.Principal, s identity.StaffScope) bool {
		ok, err := identity.HasStaffGrant(ctx, db.Pool, p, s)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !check(staffB, identity.StaffOperator) {
		t.Fatal("grant not found in its brand")
	}
	if check(staffB, identity.StaffBilling) {
		t.Fatal("operator grant gave billing")
	}
	// The same issuer and subject presented for brand A: nothing.
	staffA := staffB
	staffA.Brand.ID = a
	if check(staffA, identity.StaffOperator) {
		t.Fatal("a brand B grant gave authority in brand A")
	}
	// A customer token of the same subject: nothing.
	customer := staffB
	customer.Kind = identity.KindUser
	if check(customer, identity.StaffOperator) {
		t.Fatal("a customer token used a staff grant")
	}
	grants, err := identity.ListStaffGrants(ctx, db.Pool, b)
	if err != nil || len(grants) != 1 || grants[0].Scope != identity.StaffOperator {
		t.Fatalf("list = %+v, %v", grants, err)
	}
	if grants, _ := identity.ListStaffGrants(ctx, db.Pool, a); len(grants) != 0 {
		t.Fatalf("brand A lists brand B grants: %+v", grants)
	}
}

func TestRevokeStaffGrant(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	b := insertBrand(t, db, "zs-b")
	a := insertBrand(t, db, "zs-a")
	const issuer, subject = "http://kc/realms/zs-b", "staff-1"
	actor := events.Actor{Type: events.ActorService, ID: "zs-core-cli:ops"}
	grant := identity.StaffGrant{BrandID: b, Issuer: issuer, Subject: subject, Scope: identity.StaffOperator, GrantedBy: "ops", Reason: "on call"}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, err := identity.AddStaffGrant(ctx, tx, grant, actor, "corr")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	revoke := func(g identity.StaffGrant) (identity.StaffGrant, error) {
		var out identity.StaffGrant
		err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
			var err error
			out, err = identity.RevokeStaffGrant(ctx, tx, g, "ops", actor, "corr-revoke")
			return err
		})
		return out, err
	}
	// The grant of another brand is untouched.
	other := grant
	other.BrandID, other.Reason = a, "left the team"
	if _, err := revoke(other); !zserrors.IsCode(err, zserrors.CodeNotFound) {
		t.Fatalf("revoke in another brand: %v", err)
	}
	if _, err := revoke(identity.StaffGrant{BrandID: b, Issuer: issuer, Subject: subject, Scope: identity.StaffOperator}); !zserrors.IsCode(err, zserrors.CodeValidation) {
		t.Fatalf("revoke without a reason: %v", err)
	}
	grant.Reason = "left the team"
	got, err := revoke(grant)
	if err != nil || got.Status != "revoked" || got.ID.IsNil() {
		t.Fatalf("revoke = %+v, %v", got, err)
	}
	staff := identity.Principal{Brand: identity.BrandConfig{ID: b}, Kind: identity.KindStaff, Issuer: issuer, Subject: subject}
	if ok, err := identity.HasStaffGrant(ctx, db.Pool, staff, identity.StaffOperator); err != nil || ok {
		t.Fatalf("revoked grant still counts: %v %v", ok, err)
	}
	if _, err := revoke(grant); !zserrors.IsCode(err, zserrors.CodeNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	page, err := audit.QueryBrand(ctx, db.Pool, b, audit.Filter{Action: "staff.grant.revoked"})
	if err != nil || len(page.Events) != 1 || page.Events[0].Reason != "left the team" || page.Events[0].Details["revoked_by"] != "ops" {
		t.Fatalf("staff.grant.revoked events = %+v, %v", page.Events, err)
	}
	// A new grant of the same scope can be added after the revocation.
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, err := identity.AddStaffGrant(ctx, tx, identity.StaffGrant{BrandID: b, Issuer: issuer, Subject: subject, Scope: identity.StaffOperator, GrantedBy: "ops", Reason: "back"}, actor, "corr")
		return err
	}); err != nil {
		t.Fatalf("re-grant after revoke: %v", err)
	}
}

// An identity is written only when it changed or was last seen five or more
// minutes ago; Upsert agrees with NeedsUpsert.
func TestIdentityReadFirst(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	brandID := insertBrand(t, db, "zs-a")
	p := identity.Principal{Brand: identity.BrandConfig{ID: brandID}, Kind: identity.KindUser, Issuer: "http://kc/realms/zs-a",
		Subject: "sub-1", Email: "first@example.test", EmailVerified: true, Name: "First"}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, _, err := identity.Upsert(ctx, tx, p, "corr")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := identity.Get(ctx, db.Pool, brandID, p.Issuer, p.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if identity.NeedsUpsert(stored, p, stored.LastSeenAt.Add(time.Minute)) {
		t.Fatal("an unchanged identity seen a minute ago needs a write")
	}
	if !identity.NeedsUpsert(stored, p, stored.LastSeenAt.Add(identity.LastSeenGranularity)) {
		t.Fatal("an identity last seen five minutes ago needs no write")
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE identities SET last_seen_at = now() - interval '6 minutes'`); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, _, err := identity.Upsert(ctx, tx, p, "corr")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	again, _ := identity.Get(ctx, db.Pool, brandID, p.Issuer, p.Subject)
	if !again.LastSeenAt.After(stored.LastSeenAt.Add(-time.Second)) {
		t.Fatalf("last_seen_at not refreshed: %v then %v", stored.LastSeenAt, again.LastSeenAt)
	}
}
