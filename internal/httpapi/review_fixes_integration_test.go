//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// With mfa required_for_staff, every staff route needs evidence of a second
// factor in the staff token, however recent the sign-in. Without a grant the
// answer is still 404.
func TestStaffRoutesNeedASecondFactorWhenTheBrandRequiresIt(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-strict")
	// Before any token of the brand is verified, so the registry loads it.
	if _, err := a.db.Pool.Exec(context.Background(),
		`UPDATE brands SET manifest = jsonb_set(manifest, '{identity,mfa}', '"required_for_staff"') WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	owner := a.person(b, "owner@example.test")
	tenantID := a.createTenant(owner, "Strict")
	staff := a.person(b, "staff@example.test")
	plain := a.token(staff, enginetest.TokenOptions{Staff: true})
	second := a.token(staff, enginetest.TokenOptions{Staff: true, MFA: true})
	old := a.token(staff, enginetest.TokenOptions{Staff: true, MFA: true, AuthAge: 10 * time.Hour})
	if r := a.call(http.MethodGet, "/v1/audit/events", plain, nil); r.status != http.StatusNotFound {
		t.Fatalf("without a grant = %d %v", r.status, r.body)
	}
	grantStaff(t, a.db, b, staff, identity.StaffOperator)
	grantStaff(t, a.db, b, staff, identity.StaffActivation)
	decision := map[string]any{"state": "approved", "reason": "KYC passed", "request_id": "activation-0001"}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/audit/events"},
		{http.MethodGet, "/v1/brands/" + idgen.Format(idgen.Brand, b.ID)},
		{http.MethodPost, "/v1/staff/tenants/" + tenantID + "/activation"},
	} {
		var body any
		if route.method == http.MethodPost {
			body = decision
		}
		if r := a.call(route.method, route.path, plain, body); r.status != http.StatusForbidden || r.code() != "mfa_required" {
			t.Errorf("%s %s without a second factor = %d %v", route.method, route.path, r.status, r.body)
		}
		if r := a.call(route.method, route.path, old, body); r.status >= 400 {
			t.Errorf("%s %s with an old second factor = %d %v", route.method, route.path, r.status, r.body)
		}
		if r := a.call(route.method, route.path, second, body); r.status >= 400 {
			t.Errorf("%s %s with a second factor = %d %v", route.method, route.path, r.status, r.body)
		}
	}
	// Customer routes keep their own rules.
	if r := a.call(http.MethodGet, "/v1/tenants/"+tenantID, a.token(owner, enginetest.TokenOptions{}), nil); r.status != http.StatusOK {
		t.Fatalf("customer read = %d %v", r.status, r.body)
	}
}

// Every membership or tenant write queues one Keycloak Organization update
// in the outbox, in its own transaction. No goroutine is started per write.
// The mirror relay applies the updates.
func TestWritesQueueTheOrganizationMirror(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-mirror")
	owner := a.person(b, "owner@example.test")
	id := a.createTenant(owner, "Mirror")
	tok := a.token(owner, mfa())
	ctx := context.Background()
	count := func() int {
		var n int
		if err := a.db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE event_type = $1`, tenant.MirrorEventType).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// One write first, so the HTTP connection and its goroutines exist.
	if r := a.call(http.MethodPatch, "/v1/tenants/"+id, tok, map[string]any{"name": "Warm"}); r.status != http.StatusOK {
		t.Fatalf("warm-up = %d %v", r.status, r.body)
	}
	before, baseline := count(), runtime.NumGoroutine()
	for i := range 100 {
		if r := a.call(http.MethodPatch, "/v1/tenants/"+id, tok, map[string]any{"name": fmt.Sprintf("Mirror %d", i)}); r.status != http.StatusOK {
			t.Fatalf("write %d = %d %v", i, r.status, r.body)
		}
	}
	if got := count() - before; got != 100 {
		t.Fatalf("100 writes queued %d events", got)
	}
	if grown := runtime.NumGoroutine() - baseline; grown > 5 {
		t.Fatalf("goroutines grew by %d over 100 writes", grown)
	}

	reader := outbox.NewPostgresReader(a.db.Pool).WithEventTypes(tenant.MirrorEventType)
	relay := outbox.NewRelay("test", reader, tenant.MirrorHandler(a.db.Pool, a.eng), 50, 0, nil)
	published := 0
	for range 10 {
		n, err := relay.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		published += n
	}
	if published != before+100 {
		t.Fatalf("published %d of %d", published, before+100)
	}
	org, _, ok := a.fake.Organization(b.Realm, id)
	if !ok || org.Name != "Mirror 99" {
		t.Fatalf("organization = %+v %v", org, ok)
	}
}

// An acceptance refused because the inviter lost member:invite is recorded:
// the HTTP route commits the revocation and its audit event, and answers 404.
func TestStaleInviterRefusalIsRecordedOverHTTP(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-stale")
	owner, recruiter, recruit := a.person(b, "owner@example.test"), a.person(b, "recruiter@example.test"), a.person(b, "recruit@example.test")
	id := a.createTenant(owner, "Stale")
	base := "/v1/tenants/" + id
	tok := a.token(owner, mfa())
	recruiterMem := a.join(id, owner, recruiter, "member")
	team := a.call(http.MethodPost, base+"/teams", tok, map[string]any{"name": "Recruiters"})
	if team.status != http.StatusCreated {
		t.Fatalf("team = %d %v", team.status, team.body)
	}
	teamPath := base + "/teams/" + team.str("id")
	if r := a.call(http.MethodPut, teamPath+"/members", tok, map[string]any{"membership_ids": []string{recruiterMem}}); r.status != http.StatusOK {
		t.Fatalf("team members = %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodPut, teamPath+"/grants", tok, map[string]any{"permissions": []string{"member:invite"}}); r.status != http.StatusOK {
		t.Fatalf("team grants = %d %v", r.status, r.body)
	}
	inv := a.call(http.MethodPost, base+"/invitations", a.token(recruiter, mfa()), map[string]any{"email": recruit.Email, "role": "member"})
	if inv.status != http.StatusCreated {
		t.Fatalf("invite = %d %v", inv.status, inv.body)
	}
	if r := a.call(http.MethodPut, teamPath+"/grants", tok, map[string]any{"permissions": []string{}}); r.status != http.StatusOK {
		t.Fatalf("team grants = %d %v", r.status, r.body)
	}
	r := a.call(http.MethodPost, "/v1/invitations/accept", a.token(recruit, enginetest.TokenOptions{}), map[string]any{"token": inv.str("token")})
	if r.status != http.StatusNotFound || r.code() != "invitation_not_found" {
		t.Fatalf("accept = %d %v", r.status, r.body)
	}
	invID, _ := idgen.ParseAs(idgen.Invitation, inv.body["invitation"].(map[string]any)["id"].(string))
	var status string
	var refusals int
	ctx := context.Background()
	if err := a.db.Pool.QueryRow(ctx, `SELECT status FROM invitations WHERE id = $1`, invID).Scan(&status); err != nil || status != "revoked" {
		t.Fatalf("invitation = %q %v", status, err)
	}
	if err := a.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'tenant.invitation.refused_stale_inviter'`).Scan(&refusals); err != nil || refusals != 1 {
		t.Fatalf("refusals audited = %d %v", refusals, err)
	}
}
