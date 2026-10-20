//go:build integration

package brand

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
)

// tenantStub answers the tenant lookup of a plan: the tenant exists.
func tenantStub(t *testing.T) *killbill.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1.0/kb/tenants" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(killbill.Tenant{TenantID: "00000000-0000-0000-0000-0000000000aa", APIKey: r.URL.Query().Get("apiKey")})
	}))
	t.Cleanup(srv.Close)
	kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return kb
}

func TestApplyConvergesMetersAndQuotas(t *testing.T) {
	ctx := context.Background()
	db := postgrestest.New(t)
	deps := offlineDeps(t, db)
	deps.KillBill = tenantStub(t)
	base := brandManifest(t, "zs-meters", "env://BRAND_ZS_METERS__KEYCLOAK_STAFF_CLIENT_SECRET")
	base.Billing.Meters, base.Entitlements = nil, nil
	id := insertBrand(t, db, base)
	rec, err := Get(ctx, db.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	xml, err := os.ReadFile(filepath.Join("..", "..", "brands", "zs-reference", "catalogue.xml"))
	if err != nil {
		t.Fatal(err)
	}
	fs := files{"zs-meters/catalogue.xml": xml}
	p := &plan{record: &rec, brandID: id}
	st := &runState{deps: deps, brandID: id}
	run := func(m *Manifest) []Change {
		t.Helper()
		changes, err := planExtensions(ctx, deps, p, m, resolved{}, fs)
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range changes {
			if err := ch.run(ctx, st); err != nil {
				t.Fatal(err)
			}
		}
		return changes
	}

	m := reference(t)
	m.Slug, m.Billing.Catalogue = "zs-meters", "zs-meters/catalogue.xml"
	if changes := run(m); len(changes) != 2 || changes[0].Target != TargetMeters || changes[1].Target != TargetQuotas {
		t.Fatalf("first plan = %+v", changes)
	}
	if changes := run(m); len(changes) != 0 {
		t.Fatalf("second plan = %+v", changes)
	}
	meters, _ := ListMeters(ctx, db.Pool, id)
	quotas, _ := ListQuotas(ctx, db.Pool, id, "")
	if len(meters) != 1 || meters[0].Slug != "api-calls" || len(quotas) != 2 {
		t.Fatalf("stored = %+v %+v", meters, quotas)
	}

	// A meter on a unit the catalogue lacks stops the plan.
	bad := reference(t)
	bad.Slug, bad.Billing.Catalogue = "zs-meters", "zs-meters/catalogue.xml"
	(*bad.Billing.Meters)[0].KillBillUnit = "gpu-hour"
	if _, err := planExtensions(ctx, deps, p, bad, resolved{}, fs); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("unknown unit: %v", err)
	}
	// Without meters in the manifest, quotas name the stored meters.
	noMeters := reference(t)
	noMeters.Slug, noMeters.Billing.Catalogue, noMeters.Billing.Meters = "zs-meters", "zs-meters/catalogue.xml", nil
	if changes := run(noMeters); len(changes) != 0 {
		t.Fatalf("quotas on stored meters = %+v", changes)
	}
	(*noMeters.Entitlements)[0].Meter = "nope"
	if _, err := planExtensions(ctx, deps, p, noMeters, resolved{}, fs); codeOf(err) != "brand_manifest_invalid" {
		t.Fatalf("quota on an unknown meter: %v", err)
	}
	// An empty list retires every meter and removes every quota.
	empty := reference(t)
	empty.Slug, empty.Billing.Catalogue = "zs-meters", "zs-meters/catalogue.xml"
	empty.Billing.Meters, empty.Entitlements = &[]Meter{}, &[]Entitlement{}
	if changes := run(empty); len(changes) != 2 {
		t.Fatalf("emptying = %+v", changes)
	}
	meters, _ = ListMeters(ctx, db.Pool, id)
	quotas, _ = ListQuotas(ctx, db.Pool, id, "")
	if len(meters) != 1 || meters[0].Status != MeterRetired || len(quotas) != 0 {
		t.Fatalf("after emptying = %+v %+v", meters, quotas)
	}
	if _, err := MeterBySlug(ctx, db.Pool, id, "api-calls"); codeOf(err) != "meter_not_found" {
		t.Fatalf("retired meter: %v", err)
	}
}
