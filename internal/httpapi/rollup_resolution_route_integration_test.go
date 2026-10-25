//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Staff with the support grant confirm an unconfirmed usage rollup over HTTP,
// with the invoice of Kill Bill as the evidence; anyone else gets 404, and
// without the evidence the answer is a conflict.
func TestStaffResolveRollupRoute(t *testing.T) {
	a := newExtensionsAPI(t)
	ctx := context.Background()
	b := a.fake.InsertBrand(t, a.db.Pool, "zs-resolve-route")
	other := a.fake.InsertBrand(t, a.db.Pool, "zs-resolve-other")
	facts := brand.FactsFromCatalog([]killbill.Catalog{enginetest.TestCatalog()})
	if err := pgx.BeginFunc(ctx, a.db.Pool, func(tx pgx.Tx) error {
		_, _, err := brand.ReplaceMeters(ctx, tx, b.ID, []brand.Meter{{Slug: "api-calls", Unit: "request", KillBillUnit: enginetest.UnitAPICall}}, facts,
			events.Actor{Type: events.ActorStaff, ID: "sub:op"}, "test", "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	owner := a.person(b, "owner@example.test")
	support := a.person(b, "support@example.test")
	operator := a.person(b, "operator@example.test")
	outsider := a.person(other, "outsider@example.test")
	grantStaff(t, a.db, b, support, identity.StaffSupport)
	grantStaff(t, a.db, b, operator, identity.StaffOperator)
	grantStaff(t, a.db, other, outsider, identity.StaffSupport)
	supportTok := a.token(support, enginetest.TokenOptions{Staff: true, MFA: true})
	id := a.createTenant(owner, "Resolve Route")
	tenantID, _ := idgen.ParseAs(idgen.Tenant, id)
	sc := scope.Scope{Brand: b.ID, Tenant: tenantID}
	var account string
	if err := a.db.Pool.QueryRow(ctx, `SELECT killbill_account_id FROM tenants WHERE id = $1`, tenantID).Scan(&account); err != nil {
		t.Fatal(err)
	}
	creds := killbill.TenantCredentials{APIKey: b.Slug, APISecret: "kb-secret-" + b.Slug}
	if _, err := a.fake.KillBill(t).CreateSubscription(ctx, creds, killbill.CallContext{CreatedBy: "test"}, account, enginetest.PlanMetered, "sub-"+idgen.MustNewV7().String()); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, a.db.Pool, func(tx pgx.Tx) error {
		_, _, err := a.meter.RecordUsageBatch(ctx, tx, sc, metering.Call{Actor: events.Actor{Type: events.ActorService, ID: "scheduler"}, CorrelationID: "c"},
			[]metering.Event{{Meter: "api-calls", EventID: "r-1", QuantityMicro: 2_000_000}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a.fake.LoseAnswers("/1.0/kb/usages", 1)
	if report, err := a.meter.Forward(ctx); err != nil || report.Unconfirmed != 1 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var rollup idgen.UUID
	if err := a.db.Pool.QueryRow(ctx, `SELECT id FROM usage_rollups WHERE tenant_id = $1`, tenantID).Scan(&rollup); err != nil {
		t.Fatal(err)
	}
	path := "/v1/staff/tenants/" + id + "/usage/rollups/" + idgen.Format(idgen.Rollup, rollup) + "/resolve"
	body := map[string]any{"reason": "checked against Kill Bill", "decision": "forwarded"}

	for name, tok := range map[string]string{
		"customer":       a.token(owner, mfa()),
		"operator grant": a.token(operator, enginetest.TokenOptions{Staff: true, MFA: true}),
		"another brand":  a.token(outsider, enginetest.TokenOptions{Staff: true, MFA: true}),
	} {
		if r := a.call(http.MethodPost, path, tok, body); r.status != http.StatusNotFound && r.status != http.StatusForbidden {
			t.Errorf("%s = %d %v, want 404 or 403", name, r.status, r.body)
		}
	}
	if r := a.call(http.MethodPost, path, supportTok, body); r.status != http.StatusConflict || r.code() != "conflict" {
		t.Fatalf("no invoice carries the tracking ID = %d %v, want a conflict", r.status, r.body)
	}
	if r := a.call(http.MethodPost, path, supportTok, map[string]any{"decision": "forwarded"}); r.status != http.StatusBadRequest {
		t.Fatalf("no reason = %d %v, want 400", r.status, r.body)
	}
	if r := a.call(http.MethodPost, path, supportTok, map[string]any{"reason": "x"}); r.status != http.StatusBadRequest {
		t.Fatalf("no decision = %d %v, want 400", r.status, r.body)
	}
	if r := a.call(http.MethodPost, path, supportTok, map[string]any{"reason": "x", "decision": "forfeit"}); r.status != http.StatusBadRequest {
		t.Fatalf("unknown decision = %d %v, want 400", r.status, r.body)
	}
	invoice := a.fake.InvoiceUsage(b.Slug, account, time.Now().AddDate(0, 0, -2), time.Now().AddDate(0, 0, 2))
	first := a.call(http.MethodPost, path, supportTok, body)
	if first.status != http.StatusOK || first.str("decision") != "forwarded" || first.str("invoice_id") != invoice || first.str("rollup_id") == "" {
		t.Fatalf("resolve = %d %v", first.status, first.body)
	}
	if again := a.call(http.MethodPost, path, supportTok, body); again.status != http.StatusOK || again.str("invoice_id") != invoice || again.str("resolved_at") != first.str("resolved_at") {
		t.Fatalf("repeat = %d %v", again.status, again.body)
	}
}
