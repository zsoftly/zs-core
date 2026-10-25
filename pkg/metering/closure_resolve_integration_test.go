//go:build integration

package metering_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/scope"
)

var staffCall = metering.StaffCall{
	Call:   metering.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:op"}, CorrelationID: "corr-resolve"},
	Reason: "checked against Kill Bill",
}

// unconfirmedRollup leaves one rollup whose send Kill Bill took and whose answer
// was lost, and returns its public ID, its tenant and the account.
func unconfirmedRollup(t *testing.T, e *env, b enginetest.Brand, name string) (rollupID string, sc scope.Scope, account string) {
	t.Helper()
	ctx := context.Background()
	tn := e.tenant(b, name)
	e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "u-1", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	e.fake.LoseAnswers("/1.0/kb/usages", 1)
	if report, err := e.svc.Forward(ctx); err != nil || report.Unconfirmed != 1 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var id idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return idgen.Format(idgen.Rollup, id), tn.Scope(), tn.KillBillAccountID
}

// Staff confirm an unconfirmed rollup only against Kill Bill: the tracking ID
// of the rollup has to be on an invoice of the tenant's account. The
// confirmation is recorded with the invoice, audited, idempotent, and it
// releases the drain and the operator warning.
func TestStaffResolveAnUnconfirmedRollupWithInvoiceEvidence(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve")
	rollup, sc, account := unconfirmedRollup(t, e, b, "Resolve Co")
	slug := b.Slug
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE id = $1`, sc.Tenant); err != nil {
		t.Fatal(err)
	}
	if done, err := e.svc.Drained(ctx, sc); err != nil || done {
		t.Fatalf("drained with an unconfirmed rollup = %v %v, want false", done, err)
	}
	// No invoice carries the tracking ID yet: nothing to confirm against.
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup); codeOf(err) != "conflict" {
		t.Fatalf("resolve without evidence = %v, want conflict", err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollup_resolutions WHERE tenant_id = $1`, sc.Tenant); n != 0 {
		t.Fatalf("resolutions after a refused resolve = %d", n)
	}
	invoice := e.fake.InvoiceUsage(slug, account, e.clock.Now().AddDate(0, 0, -2), e.clock.Now().AddDate(0, 0, 2))
	got, replayed, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup)
	if err != nil || replayed || got.InvoiceID != invoice || got.Decision != "forwarded" {
		t.Fatalf("resolve = %+v replayed %v %v, want forwarded with invoice %s", got, replayed, err, invoice)
	}
	if again, replayed, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, staffCall, rollup); err != nil || !replayed || again.InvoiceID != invoice {
		t.Fatalf("repeat = %+v replayed %v %v", again, replayed, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'metering.rollup_resolved' AND reason = $2`, sc.Tenant, staffCall.Reason); n != 1 {
		t.Fatalf("resolution audits = %d, want 1", n)
	}
	if done, err := e.svc.Drained(ctx, sc); err != nil || !done {
		t.Fatalf("drained after the resolution = %v %v, want true", done, err)
	}
	fs, err := doctor.ClosureFindings(ctx, e.db.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Check == "usage_unconfirmed" {
			t.Fatalf("a resolved rollup is still reported: %+v", f)
		}
	}
}

// Only an unconfirmed rollup of the tenant can be resolved this way.
func TestStaffResolveRefusesWhatIsNotUnconfirmed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve-no")
	rollup, sc, _ := unconfirmedRollup(t, e, b, "Resolve No")
	// A rollup that is simply pending.
	other := e.tenant(b, "Pending Co")
	e.subscribe(b, other, enginetest.PlanMetered)
	e.fake.SetDown(false, true)
	if _, _, err := e.record(other.Scope(), metering.Event{Meter: "api-calls", EventID: "p-1", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Forward(ctx); err != nil {
		t.Fatal(err)
	}
	var pending idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM usage_rollups WHERE tenant_id = $1`, other.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, other.Scope(), staffCall, idgen.Format(idgen.Rollup, pending)); codeOf(err) != "conflict" {
		t.Fatalf("resolve of a pending rollup = %v, want conflict", err)
	}
	// Another tenant's scope does not reach the rollup.
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, other.Scope(), staffCall, rollup); codeOf(err) != "not_found" {
		t.Fatalf("resolve through another tenant = %v, want not_found", err)
	}
	if _, _, err := e.svc.ResolveUnconfirmedRollup(ctx, sc, metering.StaffCall{Call: staffCall.Call}, rollup); codeOf(err) != "reason_required" {
		t.Fatalf("resolve without a reason = %v, want reason_required", err)
	}
}

// The database refuses a resolution of a rollup that is not unconfirmed, and
// one that has been recorded cannot be changed or removed.
func TestRollupResolutionsAreGuardedByTheDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-resolve-db")
	rollup, sc, account := unconfirmedRollup(t, e, b, "Resolve DB")
	id, _ := idgen.ParseAs(idgen.Rollup, rollup)
	invoice := e.fake.InvoiceUsage(b.Slug, account, e.clock.Now().AddDate(0, 0, -2), e.clock.Now().AddDate(0, 0, 2))
	insert := `INSERT INTO usage_rollup_resolutions (brand_id, tenant_id, rollup_id, decision, invoice_id, resolved_by, note) VALUES ($1, $2, $3, 'forwarded', $4::uuid, 'sub:op', 'n')`
	if _, err := e.db.Pool.Exec(ctx, insert, sc.Brand, sc.Tenant, id, invoice); err != nil {
		t.Fatalf("a resolution of an unconfirmed rollup = %v", err)
	}
	for _, stmt := range []string{`UPDATE usage_rollup_resolutions SET note = 'x'`, `DELETE FROM usage_rollup_resolutions`} {
		_, err := e.db.Pool.Exec(ctx, stmt)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%s = %v, want a check violation", stmt, err)
		}
	}
	// A rollup that is not unconfirmed.
	var plain idgen.UUID
	other := e.tenant(b, "Plain DB")
	e.subscribe(b, other, enginetest.PlanMetered)
	e.fake.SetDown(false, true)
	if _, _, err := e.record(other.Scope(), metering.Event{Meter: "api-calls", EventID: "q-1", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Forward(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM usage_rollups WHERE tenant_id = $1`, other.ID).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	_, err := e.db.Pool.Exec(ctx, insert, other.BrandID, other.ID, plain, invoice)
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("a resolution of a rollup that is not unconfirmed = %v, want a check violation", err)
	}
}
