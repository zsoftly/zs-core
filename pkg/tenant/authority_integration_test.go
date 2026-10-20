//go:build integration

package tenant_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// holdTenant begins a transaction that locks the tenant row, as every write
// does, and returns it with its backend process ID.
func (e *env) holdTenant(sc scope.Scope) (pgx.Tx, int) {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		e.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tenants WHERE brand_id = $1 AND id = $2 FOR UPDATE`, sc.Brand, sc.Tenant); err != nil {
		e.t.Fatal(err)
	}
	return tx, pid
}

// waitBlocked waits until n sessions wait, directly or through each other,
// on the session holder.
func (e *env) waitBlocked(holder, n int) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked int
		err := e.db.Pool.QueryRow(context.Background(), `WITH RECURSIVE waiting(pid) AS (
    SELECT $1::int
    UNION
    SELECT a.pid FROM pg_stat_activity a JOIN waiting w ON w.pid = ANY(pg_blocking_pids(a.pid))
)
SELECT count(*) - 1 FROM waiting`, holder).Scan(&blocked)
		if err != nil {
			e.t.Fatal(err)
		}
		if blocked >= n {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%d sessions wait on %d, want %d", blocked, holder, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// async runs fn in its own transaction in the background and returns its
// result channel.
func (e *env) async(fn func(tx pgx.Tx) error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- e.tx(fn) }()
	return done
}

func (e *env) membership(sc scope.Scope, id idgen.UUID) tenant.Membership {
	e.t.Helper()
	m, err := tenant.GetMembership(context.Background(), e.db.Pool, sc, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// Two owners demote each other at the same time. The tenant lock orders the
// two changes, and the second one is checked against its caller's role as it
// is after the first: a demoted owner has no authority over an owner. One
// owner remains.
func TestOwnersDemotingEachOtherConcurrently(t *testing.T) {
	e := newEnv(t)
	aP, bP := e.person("a@example.test"), e.person("b@example.test")
	tn := e.mustOrg(aP, "Two owners")
	sc := tn.Scope()
	ctx := context.Background()
	a := e.caller(sc, aP, true)
	bM := e.join(sc, a, bP, tenant.RoleAdmin)
	ownerRole, adminRole := tenant.RoleOwner, tenant.RoleAdmin
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, a, bM.ID, tenant.MemberChange{Role: &ownerRole})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Both callers are built before either change, as scope resolution
	// builds them before the handler's transaction.
	a, b := e.caller(sc, aP, true), e.caller(sc, bP, true)

	holder, pid := e.holdTenant(sc)
	aDone := e.async(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, a, b.Membership.ID, tenant.MemberChange{Role: &adminRole})
		return err
	})
	e.waitBlocked(pid, 1)
	bDone := e.async(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, b, a.Membership.ID, tenant.MemberChange{Role: &adminRole})
		return err
	})
	e.waitBlocked(pid, 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-aDone; err != nil {
		t.Fatalf("first demotion: %v", err)
	}
	if err := <-bDone; code(err) != "permission_denied" {
		t.Fatalf("demotion by a demoted owner: %v", err)
	}
	if got := e.membership(sc, a.Membership.ID); got.Role != tenant.RoleOwner {
		t.Fatalf("a = %s", got.Role)
	}
	if got := e.membership(sc, b.Membership.ID); got.Role != tenant.RoleAdmin {
		t.Fatalf("b = %s", got.Role)
	}

	// Without ordering them by hand, at least one owner always remains.
	bM2 := b.Membership.ID
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, e.caller(sc, aP, true), bM2, tenant.MemberChange{Role: &ownerRole})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a, b = e.caller(sc, aP, true), e.caller(sc, bP, true)
	var wg sync.WaitGroup
	for _, pair := range [][2]tenant.Caller{{a, b}, {b, a}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.tx(func(tx pgx.Tx) error {
				_, err := tenant.UpdateMember(ctx, tx, sc, pair[0], pair[1].Membership.ID, tenant.MemberChange{Role: &adminRole})
				return err
			})
		}()
	}
	wg.Wait()
	var owners int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner' AND status = 'active'`, sc.Tenant).Scan(&owners); err != nil || owners != 1 {
		t.Fatalf("owners after concurrent demotions = %d %v", owners, err)
	}
}

// Owner A demotes owner B while B promotes member M to owner. When the
// demotion commits first, B's promotion runs with B's role as it is now,
// admin, and is refused: M never becomes an owner.
func TestDemotionWinsOverAConcurrentPromotion(t *testing.T) {
	e := newEnv(t)
	aP, bP, mP := e.person("a@example.test"), e.person("b@example.test"), e.person("m@example.test")
	tn := e.mustOrg(aP, "Promotion race")
	sc := tn.Scope()
	ctx := context.Background()
	a := e.caller(sc, aP, true)
	bM := e.join(sc, a, bP, tenant.RoleAdmin)
	mM := e.join(sc, a, mP, tenant.RoleMember)
	ownerRole, adminRole := tenant.RoleOwner, tenant.RoleAdmin
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, a, bM.ID, tenant.MemberChange{Role: &ownerRole})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a, b := e.caller(sc, aP, true), e.caller(sc, bP, true)

	holder, pid := e.holdTenant(sc)
	demote := e.async(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, a, bM.ID, tenant.MemberChange{Role: &adminRole})
		return err
	})
	e.waitBlocked(pid, 1)
	promote := e.async(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, b, mM.ID, tenant.MemberChange{Role: &ownerRole})
		return err
	})
	e.waitBlocked(pid, 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-demote; err != nil {
		t.Fatalf("demotion: %v", err)
	}
	if err := <-promote; code(err) != "permission_denied" {
		t.Fatalf("promotion by a demoted owner: %v", err)
	}
	if got := e.membership(sc, mM.ID); got.Role != tenant.RoleMember {
		t.Fatalf("m became %s", got.Role)
	}

	// A caller removed before its write runs gets tenant_not_found.
	b = e.caller(sc, bP, true)
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, sc, e.caller(sc, aP, true), bM.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.CreateTeam(ctx, tx, sc, b, "Late")
		return err
	}); code(err) != "tenant_not_found" {
		t.Fatalf("write by a removed caller: %v", err)
	}
}

// Two acceptances of one invitation at the same time: one adds the member,
// the other finds no pending invitation.
func TestConcurrentAcceptancesOfOneInvitation(t *testing.T) {
	e := newEnv(t)
	ownerP, inviteeP := e.person("owner@example.test"), e.person("invitee@example.test")
	tn := e.mustOrg(ownerP, "Accept race")
	sc := tn.Scope()
	ctx := context.Background()
	owner := e.caller(sc, ownerP, true)
	var token string
	if err := e.tx(func(tx pgx.Tx) error {
		var err error
		_, token, err = tenant.CreateInvitation(ctx, tx, sc, owner, inviteeP.Email, tenant.RoleMember)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	holder, pid := e.holdTenant(sc)
	var results []<-chan error
	for range 2 {
		results = append(results, e.async(func(tx pgx.Tx) error {
			_, _, err := tenant.AcceptInvitation(ctx, tx, acceptor(inviteeP), token)
			return err
		}))
	}
	e.waitBlocked(pid, 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range results {
		got[code(<-r)]++
	}
	if got["ok"] != 1 || got["invitation_not_found"] != 1 {
		t.Fatalf("acceptances = %v", got)
	}
	var memberships int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE identity_id = $1`, inviteeP.Identity.ID).Scan(&memberships); err != nil || memberships != 1 {
		t.Fatalf("memberships = %d %v", memberships, err)
	}
}

// An invitation carries its inviter's authority only while the inviter still
// has it. Removing or demoting the inviter revokes what it can no longer
// send. An inviter that lost member:invite another way, through a team, is
// caught at acceptance: the invitation is revoked, the refusal audited, and
// the answer is invitation_not_found.
func TestInvitationsNeedTheInvitersCurrentAuthority(t *testing.T) {
	e := newEnv(t)
	ownerP, adminP, memberP := e.person("owner@example.test"), e.person("admin@example.test"), e.person("member@example.test")
	tn := e.mustOrg(ownerP, "Inviters")
	sc := tn.Scope()
	ctx := context.Background()
	owner := e.caller(sc, ownerP, true)
	adminM := e.join(sc, owner, adminP, tenant.RoleAdmin)
	memberM := e.join(sc, owner, memberP, tenant.RoleMember)
	invite := func(c tenant.Caller, email string, role tenant.Role) (tenant.Invitation, string) {
		t.Helper()
		var inv tenant.Invitation
		var token string
		if err := e.tx(func(tx pgx.Tx) error {
			var err error
			inv, token, err = tenant.CreateInvitation(ctx, tx, sc, c, email, role)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return inv, token
	}
	// accept commits a recorded refusal, as the HTTP route does.
	accept := func(p enginetest.Person, token string) error {
		var refused error
		err := e.tx(func(tx pgx.Tx) error {
			_, _, err := tenant.AcceptInvitation(ctx, tx, acceptor(p), token)
			var recorded *tenant.RefusalRecorded
			if errors.As(err, &recorded) {
				refused = err
				return nil
			}
			return err
		})
		if err == nil {
			err = refused
		}
		return err
	}
	status := func(id idgen.UUID) string {
		var s string
		if err := e.db.Pool.QueryRow(ctx, `SELECT status FROM invitations WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Demotion below the invited role revokes the invitation.
	admin := e.caller(sc, adminP, true)
	asAdmin, asAdminToken := invite(admin, "new-admin@example.test", tenant.RoleAdmin)
	asMember, _ := invite(admin, "new-member@example.test", tenant.RoleMember)
	memberRole := tenant.RoleMember
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.UpdateMember(ctx, tx, sc, owner, adminM.ID, tenant.MemberChange{Role: &memberRole})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status(asAdmin.ID) != "revoked" || status(asMember.ID) != "revoked" {
		t.Fatalf("after demotion: %s %s", status(asAdmin.ID), status(asMember.ID))
	}
	if err := accept(e.person("new-admin@example.test"), asAdminToken); code(err) != "invitation_not_found" {
		t.Fatalf("invitation of a demoted inviter: %v", err)
	}

	// A member allowed to invite through a team loses it when the team's
	// grants change. Acceptance refuses the invitation and records it.
	var team tenant.Team
	if err := e.tx(func(tx pgx.Tx) error {
		var err error
		if team, err = tenant.CreateTeam(ctx, tx, sc, owner, "Recruiters"); err != nil {
			return err
		}
		if _, err = tenant.SetTeamMembers(ctx, tx, sc, owner, team.ID, []idgen.UUID{memberM.ID}); err != nil {
			return err
		}
		_, err = tenant.SetTeamGrants(ctx, tx, sc, owner, team.ID, []string{"member:invite"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	recruit, recruitToken := invite(e.caller(sc, memberP, true), "recruit@example.test", tenant.RoleMember)
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.SetTeamGrants(ctx, tx, sc, owner, team.ID, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status(recruit.ID) != "pending" {
		t.Fatalf("a team change revoked the invitation: %s", status(recruit.ID))
	}
	recruitP := e.person("recruit@example.test")
	if err := accept(recruitP, recruitToken); code(err) != "invitation_not_found" {
		t.Fatalf("invitation of an inviter without member:invite: %v", err)
	}
	if status(recruit.ID) != "revoked" {
		t.Fatalf("refused invitation = %s, want revoked", status(recruit.ID))
	}
	if got := actions(t, e.db, sc); !contains(got, "tenant.invitation.refused_stale_inviter:success") {
		t.Fatalf("audit = %v", got)
	}
	if _, err := tenant.MembershipOf(ctx, e.db.Pool, sc, recruitP.Identity.ID); code(err) != "not_found" {
		t.Fatalf("refused acceptance added a membership: %v", err)
	}

	// Removal revokes every pending invitation of the removed member.
	owner2P := e.person("owner2@example.test")
	o2 := e.join(sc, owner, owner2P, tenant.RoleAdmin)
	pending, _ := invite(e.caller(sc, owner2P, true), "later@example.test", tenant.RoleMember)
	if err := e.tx(func(tx pgx.Tx) error {
		_, err := tenant.RemoveMember(ctx, tx, sc, owner, o2.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if status(pending.ID) != "revoked" {
		t.Fatalf("invitation of a removed member = %s", status(pending.ID))
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Closed is terminal. A reactivation that started before a close committed
// is refused under the tenant lock with tenant_closed, and the tenant stays
// closed. Suspend and reactivate of a closed tenant are tenant_closed too.
func TestCloseIsTerminalUnderConcurrentReactivation(t *testing.T) {
	e := newEnv(t)
	ownerP := e.person("owner@example.test")
	tn := e.mustOrg(ownerP, "Closing")
	sc := tn.Scope()
	ctx := context.Background()
	operator := events.Actor{Type: events.ActorService, ID: "zs-core-cli:test"}
	if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, sc, operator, "overdue", "test"); err != nil {
		t.Fatal(err)
	}
	holder, pid := e.holdTenant(sc)
	closeDone := make(chan error, 1)
	go func() {
		_, err := tenant.Close(ctx, e.db.Pool, e.eng, sc, operator, "customer left", "test")
		closeDone <- err
	}()
	e.waitBlocked(pid, 1)
	reactivateDone := make(chan error, 1)
	go func() {
		_, err := tenant.Reactivate(ctx, e.db.Pool, e.eng, sc, operator, "paid", "test")
		reactivateDone <- err
	}()
	e.waitBlocked(pid, 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := <-reactivateDone; code(err) != "tenant_closed" {
		t.Fatalf("reactivation racing a close: %v", err)
	}
	got, err := tenant.Get(ctx, e.db.Pool, sc)
	if err != nil || got.Status != tenant.StatusClosed {
		t.Fatalf("status = %s %v", got.Status, err)
	}
	if org, _, _ := e.fake.Organization(e.brand.Realm, tn.PublicID()); org.Enabled {
		t.Fatal("the refused reactivation left the Organization enabled")
	}
	type transition func(context.Context, tenant.DB, tenant.Engines, scope.Scope, events.Actor, string, string) (tenant.Tenant, error)
	for name, op := range map[string]transition{"suspend": tenant.Suspend, "reactivate": tenant.Reactivate, "close": tenant.Close} {
		if _, err := op(ctx, e.db.Pool, e.eng, sc, operator, "x", "test"); code(err) != "tenant_closed" {
			t.Errorf("%s a closed tenant: %v", name, err)
		}
	}
}

// A database failure while acceptance locks the tenant is a retryable 503,
// never invitation_not_found.
func TestAcceptanceLockFailureIsUnavailable(t *testing.T) {
	e := newEnv(t)
	ownerP, inviteeP := e.person("owner@example.test"), e.person("invitee@example.test")
	tn := e.mustOrg(ownerP, "Lock failure")
	sc := tn.Scope()
	ctx := context.Background()
	var token string
	if err := e.tx(func(tx pgx.Tx) error {
		var err error
		_, token, err = tenant.CreateInvitation(ctx, tx, sc, e.caller(sc, ownerP, true), inviteeP.Email, tenant.RoleMember)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.holdTenant(sc)
	err := e.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '100ms'`); err != nil {
			return err
		}
		_, _, err := tenant.AcceptInvitation(ctx, tx, acceptor(inviteeP), token)
		return err
	})
	if code(err) != "database_unavailable" {
		t.Fatalf("lock failure: %v", err)
	}
}

// Tenant names are at most 100 characters, the Kill Bill limit. A personal
// tenant takes the display name cut to 100 characters on a character
// boundary, and Kill Bill accepts the account.
func TestTenantNamesFitKillBill(t *testing.T) {
	e := newEnv(t)
	ownerP := e.person("owner@example.test")
	ctx := context.Background()
	if _, err := e.createOrg(ownerP, strings.Repeat("a", 101), "long-name-1"); code(err) != "validation_failed" {
		t.Fatalf("101 characters: %v", err)
	}
	long := strings.Repeat("é", 100)
	tn, err := e.createOrg(ownerP, long, "long-name-2")
	if err != nil {
		t.Fatalf("100 two-byte characters: %v", err)
	}
	if account, ok := e.fake.Account(e.brand.Slug, tn.PublicID()); !ok || account.Name != long {
		t.Fatalf("account = %+v %v", account, ok)
	}

	p := e.person("long@example.test")
	display := strings.Repeat("Ω", 99) + "ü" + strings.Repeat("x", 50)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE identities SET display_name = $1 WHERE id = $2`, display, p.Identity.ID); err != nil {
		t.Fatal(err)
	}
	personal, created, err := tenant.EnsurePersonal(ctx, e.db.Pool, e.eng, tenant.CreateRequest{
		Brand: e.brand.ID, Creator: p.Identity.ID, Name: display, Actor: userActor(p), CorrelationID: "test",
	})
	if err != nil || !created {
		t.Fatalf("personal tenant with a 150-character display name: %v", err)
	}
	if personal.Name != strings.Repeat("Ω", 99)+"ü" || utf8.RuneCountInString(personal.Name) != 100 {
		t.Fatalf("personal name = %q", personal.Name)
	}
	if account, ok := e.fake.Account(e.brand.Slug, personal.PublicID()); !ok || account.Name != personal.Name {
		t.Fatalf("account = %+v %v", account, ok)
	}
	if n := e.fake.RejectedAccounts(); n != 0 {
		t.Fatalf("Kill Bill refused %d accounts", n)
	}
	// The fake holds Kill Bill's limits, so a longer name would fail there.
	_, err = e.eng.KillBill.CreateAccount(ctx, killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "test"}, killbill.Account{Name: strings.Repeat("a", 101), ExternalKey: "x", Currency: "CAD"})
	if err == nil || e.fake.RejectedAccounts() != 1 {
		t.Fatalf("the fake accepted a 101-character name: %v", err)
	}
	_, err = e.eng.KillBill.CreateAccount(ctx, killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "test"}, killbill.Account{Name: "a", ExternalKey: strings.Repeat("k", 256), Currency: "CAD"})
	if err == nil || e.fake.RejectedAccounts() != 2 {
		t.Fatalf("the fake accepted a 256-character external key: %v", err)
	}
}

// A second request for the personal tenant while another holds its creation
// waits and returns the same tenant. It never answers
// idempotency_key_in_progress. When the creation stays held past the wait,
// the answer is a retryable not_ready.
func TestPersonalTenantWaitsForAConcurrentCreation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.person("solo@example.test")
	req := tenant.CreateRequest{Brand: e.brand.ID, Creator: p.Identity.ID, Name: "Solo", Actor: userActor(p), CorrelationID: "test"}

	// Many requests at once all get the one tenant.
	var wg sync.WaitGroup
	ids := make([]idgen.UUID, 6)
	errs := make([]error, 6)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tn, _, err := tenant.EnsurePersonal(ctx, e.db.Pool, e.eng, req)
			ids[i], errs[i] = tn.ID, err
		}()
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("request %d = %v %v, want %v", i, ids[i], errs[i], ids[0])
		}
	}

	// Another request holds the creation of a second person's tenant. The
	// waiting request takes over once the claim is released.
	q := e.person("held@example.test")
	held := tenant.CreateRequest{Brand: e.brand.ID, Creator: q.Identity.ID, Name: "Held", Actor: userActor(q), CorrelationID: "test"}
	hold := func() {
		t.Helper()
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_provisioning
    (tenant_id, brand_id, kind, slug, name, created_by_identity_id, fingerprint, claim_token, claimed_until)
VALUES ($1, $2, 'personal', $3, 'Held', $4, 'personal', '\x01', now() + interval '1 hour')
ON CONFLICT (brand_id, created_by_identity_id) WHERE kind = 'personal'
DO UPDATE SET claim_token = '\x01', claimed_until = now() + interval '1 hour'`,
			idgen.MustNewV7(), e.brand.ID, "held-"+strings.ToLower(idgen.MustNewV7().String()[28:]), q.Identity.ID); err != nil {
			t.Fatal(err)
		}
	}
	hold()
	go func() {
		time.Sleep(500 * time.Millisecond)
		_, _ = e.db.Pool.Exec(ctx, `UPDATE tenant_provisioning SET claim_token = NULL, claimed_until = NULL WHERE created_by_identity_id = $1`, q.Identity.ID)
	}()
	tn, created, err := tenant.EnsurePersonal(ctx, e.db.Pool, e.eng, held)
	if err != nil || !created || tn.Kind != tenant.KindPersonal {
		t.Fatalf("after the claim was released: %+v %v %v", tn, created, err)
	}

	// A creation held for longer than the wait is a retryable not_ready.
	r := e.person("stuck@example.test")
	q = r
	hold()
	start := time.Now()
	_, _, err = tenant.EnsurePersonal(ctx, e.db.Pool, e.eng, tenant.CreateRequest{Brand: e.brand.ID, Creator: r.Identity.ID, Name: "Stuck", Actor: userActor(r)})
	if code(err) != "not_ready" {
		t.Fatalf("held creation: %v", err)
	}
	if waited := time.Since(start); waited < tenant.PersonalWait || waited > tenant.PersonalWait+3*time.Second {
		t.Fatalf("waited %s", waited)
	}
}

// A row that names an identity cannot name one of another brand, and an
// accepted invitation names a membership of its own tenant.
func TestRowsNameIdentitiesAndMembershipsOfTheirOwnScope(t *testing.T) {
	e := newEnv(t)
	ownerP := e.person("owner@example.test")
	tn := e.mustOrg(ownerP, "Scoped")
	ctx := context.Background()
	other := e.fake.InsertBrand(t, e.db.Pool, "zs-other")
	stranger := enginetest.InsertPerson(t, e.db.Pool, other, "stranger@example.test")
	_, err := e.db.Pool.Exec(ctx, `INSERT INTO memberships (brand_id, tenant_id, id, identity_id, role) VALUES ($1, $2, $3, $4, 'member')`,
		tn.BrandID, tn.ID, idgen.MustNewV7(), stranger.Identity.ID)
	if !isForeignKeyViolation(err) {
		t.Fatalf("membership naming another brand's identity: %v", err)
	}
	_, err = e.db.Pool.Exec(ctx, `UPDATE tenants SET created_by_identity_id = $3 WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID, stranger.Identity.ID)
	if !isForeignKeyViolation(err) {
		t.Fatalf("tenant created by another brand's identity: %v", err)
	}
	var token string
	if err := e.tx(func(tx pgx.Tx) error {
		var err error
		_, token, err = tenant.CreateInvitation(ctx, tx, tn.Scope(), e.caller(tn.Scope(), ownerP, true), "x@example.test", tenant.RoleMember)
		return err
	}); err != nil || token == "" {
		t.Fatal(err)
	}
	_, err = e.db.Pool.Exec(ctx, `UPDATE invitations SET status = 'accepted', accepted_at = now(), accepted_membership_id = $1`, idgen.MustNewV7())
	if !isForeignKeyViolation(err) {
		t.Fatalf("invitation accepted by an unknown membership: %v", err)
	}
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
