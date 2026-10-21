//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func TestBillingRecipientStatusAndCommittedAccessReduction(t *testing.T) {
	a := newTenantAPI(t)
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-recipient")
	other := a.fake.InsertBrand(t, a.db.Pool, "zs-recipient-other")
	owner, next := a.person(b, "sensitive-owner@example.test"), a.person(b, "sensitive-next@example.test")
	id := a.createTenant(owner, "Recipient API")
	base := "/v1/tenants/" + id
	member := a.join(id, owner, next, "admin")
	u, err := idgen.ParseAs(idgen.Tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	sc := scope.Scope{Brand: b.ID, Tenant: u}
	if _, err := tenant.ReconcileBillingRecipient(context.Background(), a.db.Pool, a.eng, sc, "api"); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"ready", "pending"} {
		if expected == "pending" {
			a.fake.SetDown(true, true)
			r := a.call(http.MethodPost, base+"/ownership/transfer", a.token(owner, mfa()), map[string]any{"membership_id": member})
			if r.status != http.StatusOK {
				t.Fatalf("committed transfer appeared failed: %d %v", r.status, r.body)
			}
		}
		r := a.call(http.MethodGet, base, a.token(owner, mfa()), nil)
		if r.status != http.StatusOK || r.str("billing_recipient_status") != expected {
			t.Fatalf("status = %d %v", r.status, r.body)
		}
		encoded, _ := json.Marshal(r.body)
		if strings.Contains(string(encoded), "@") || strings.Contains(string(encoded), "kb-secret") {
			t.Fatal("tenant response disclosed recipient")
		}
	}
	a.fake.SetDown(false, false)
	if _, err := a.db.Pool.Exec(context.Background(), `UPDATE identities SET email_verified=false WHERE brand_id=$1 AND id=$2`, b.ID, next.Identity.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.ReconcileBillingRecipient(context.Background(), a.db.Pool, a.eng, sc, "api-clear"); err != nil {
		t.Fatal(err)
	}
	r := a.call(http.MethodGet, base, a.token(owner, mfa()), nil)
	if r.status != http.StatusOK || r.str("billing_recipient_status") != "no_eligible_owner" {
		t.Fatalf("clear status = %d %v", r.status, r.body)
	}
	encoded, _ := json.Marshal(r.body)
	if strings.Contains(string(encoded), "@") || strings.Contains(string(encoded), "kb-secret") {
		t.Fatal("clear status disclosed recipient")
	}
	intruder := a.person(other, "intruder@example.test")
	intruderToken := a.token(intruder, mfa())
	if r := a.call(http.MethodGet, "/v1/me", intruderToken, nil); r.status != http.StatusOK {
		t.Fatalf("other-brand authentication: %d %v", r.status, r.body)
	}
	if r := a.call(http.MethodGet, base, intruderToken, nil); r.status != http.StatusNotFound || r.code() != "tenant_not_found" {
		t.Fatalf("cross-brand status = %d %v", r.status, r.body)
	}
}
