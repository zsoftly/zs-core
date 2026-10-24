//go:build integration

package tenant_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// readyToComplete is a closing tenant whose settlement is at blocking with
// everything completion needs.
func readyToComplete(t *testing.T, e *env) tenant.Tenant {
	t.Helper()
	tn := settlementAt(t, e, "blocking", true, true)
	settleInvoice(t, e, tn, "no_invoice")
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE tenant_closure_settlements SET finance_completed_at = now() WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	return tn
}

func insertKey(t *testing.T, e *env, tn tenant.Tenant) {
	t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), `INSERT INTO api_keys (id, brand_id, tenant_id, created_by_membership_id, name, digest, prefix, permissions, expires_at)
SELECT gen_random_uuid(), brand_id, tenant_id, id, 'ci', decode(repeat('ab', 32), 'hex'), 'zs_test_', ARRAY['tenant:read'], now() + interval '30 days'
FROM memberships WHERE tenant_id = $1 LIMIT 1`, tn.ID); err != nil {
		t.Fatal(err)
	}
}

// Completing the settlement closes the tenant in the same transaction: the
// status, the time, the audit entry and the revoked API keys.
func TestCompletingTheSettlementClosesTheTenant(t *testing.T) {
	e := newEnv(t)
	postgrestest.EnableClosureInvariants(t, e.db)
	ctx := context.Background()
	tn := readyToComplete(t, e)
	insertKey(t, e, tn)
	got, err := tenant.AdvanceClosureSettlement(ctx, e.db.Pool, tn.Scope(), reviewActor, tenant.ClosureCompleted, "corr-complete")
	if err != nil || !got.Changed {
		t.Fatalf("completion = %+v %v", got, err)
	}
	var status string
	var closedAt *string
	if err := e.db.Pool.QueryRow(ctx, `SELECT status, closed_at::text FROM tenants WHERE id = $1`, tn.ID).Scan(&status, &closedAt); err != nil || status != "closed" || closedAt == nil {
		t.Fatalf("tenant = %q closed_at %v, %v", status, closedAt, err)
	}
	var reason *string
	if err := e.db.Pool.QueryRow(ctx, `SELECT revoked_reason FROM api_keys WHERE tenant_id = $1`, tn.ID).Scan(&reason); err != nil || reason == nil || *reason != tenant.KeyRevokedTenantClosed {
		t.Fatalf("key revoked reason = %v, %v", reason, err)
	}
	var audits int
	var actor string
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*), coalesce(max(actor_id), '') FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closed'`, tn.ID).Scan(&audits, &actor); err != nil || audits != 1 || actor != reviewActor.ID {
		t.Fatalf("tenant.closed audits = %d by %q, %v", audits, actor, err)
	}
	// A repeat changes nothing and is not an error, though the tenant is closed.
	if again, err := tenant.AdvanceClosureSettlement(ctx, e.db.Pool, tn.Scope(), reviewActor, tenant.ClosureCompleted, "corr-complete"); err != nil || again.Changed {
		t.Fatalf("repeat = %+v %v, want no change", again, err)
	}
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closed'`, tn.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("tenant.closed audits after the repeat = %d, %v", audits, err)
	}
}

// The sweeper takes a tenant through to completion and the tenant ends closed.
// A tenant that waits for review stays closing. A completed settlement always
// has a closed tenant and a closed tenant that has a settlement always has a
// completed one.
func TestSweeperClosesTheTenantWhenTheSettlementCompletes(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		m := &kbMode{noClaim: true, startCancelling: true}
		var posts, lookups atomic.Int64
		e, tn := claimedClosure(t, m, &posts, &lookups)
		postgrestest.EnableClosureInvariants(t, e.db)
		if legacy {
			addLegacyEvent(t, e, tn)
		}
		steps := &fakeClosureSteps{prepared: ready(), block: true}
		if _, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}).SweepOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := map[bool]string{false: "closed", true: "closing"}[legacy]
		var status, state string
		if err := e.db.Pool.QueryRow(context.Background(), `SELECT t.status, s.status FROM tenants t JOIN tenant_closure_settlements s ON s.brand_id = t.brand_id AND s.tenant_id = t.id WHERE t.id = $1`, tn.ID).Scan(&status, &state); err != nil || status != want {
			t.Fatalf("legacy=%v: tenant %q settlement %q, %v, want tenant %q", legacy, status, state, err, want)
		}
		var broken int
		if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM tenant_closure_settlements s JOIN tenants t ON t.brand_id = s.brand_id AND t.id = s.tenant_id
WHERE (s.status = 'completed') <> (t.status = 'closed')`).Scan(&broken); err != nil || broken != 0 {
			t.Fatalf("legacy=%v: %d tenants whose settlement and status disagree, %v", legacy, broken, err)
		}
	}
}

// The database ties a completed settlement to a closed tenant at commit, in
// both directions, so no code path can leave one without the other.
func TestClosureInvariantsAreEnforcedByTheDatabase(t *testing.T) {
	e := newEnv(t)
	postgrestest.EnableClosureInvariants(t, e.db)
	ctx := context.Background()
	run := func(statements ...string) error {
		return pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
			for _, s := range statements {
				if _, err := tx.Exec(ctx, s); err != nil {
					return err
				}
			}
			return nil
		})
	}
	complete := `UPDATE tenant_closure_settlements SET status = 'completed', completed_at = now() WHERE tenant_id = '%s'`
	_ = complete
	// A settlement completed while the tenant is still closing.
	tn := readyToComplete(t, e)
	if err := run(`UPDATE tenant_closure_settlements SET status = 'completed', completed_at = now() WHERE tenant_id = '` + tn.ID.String() + `'`); !checkViolation(err) {
		t.Fatalf("a completed settlement of a closing tenant = %v, want a check violation", err)
	}
	// A closing tenant closed without a completed settlement.
	if err := run(`UPDATE tenants SET status = 'closed', closed_at = now() WHERE id = '` + tn.ID.String() + `'`); !checkViolation(err) {
		t.Fatalf("closing -> closed without a completed settlement = %v, want a check violation", err)
	}
	// A closing tenant that goes back to another status.
	for _, to := range []string{"active", "suspended"} {
		if err := run(`UPDATE tenants SET status = '` + to + `' WHERE id = '` + tn.ID.String() + `'`); !checkViolation(err) {
			t.Fatalf("closing -> %s = %v, want a check violation", to, err)
		}
	}
	// Both in one transaction satisfy the rules at commit.
	if err := run(
		`UPDATE tenant_closure_settlements SET status = 'completed', completed_at = now() WHERE tenant_id = '`+tn.ID.String()+`'`,
		`UPDATE tenants SET status = 'closed', closed_at = now() WHERE id = '`+tn.ID.String()+`'`,
	); err != nil {
		t.Fatalf("both in one transaction = %v", err)
	}
	// An active tenant that has a settlement that is not completed is not closed
	// by a plain update.
	other := e.mustOrg(e.person("other-owner@example.test"), "Other")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at) VALUES ($1, $2, now())`, other.BrandID, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := run(`UPDATE tenants SET status = 'closed', closed_at = now() WHERE id = '` + other.ID.String() + `'`); !checkViolation(err) {
		t.Fatalf("closing a tenant with an unfinished settlement = %v, want a check violation", err)
	}
	// A tenant with no settlement at all can be closed: that is the hard close.
	plain := e.mustOrg(e.person("plain-owner@example.test"), "Plain")
	if err := run(`UPDATE tenants SET status = 'closed', closed_at = now() WHERE id = '` + plain.ID.String() + `'`); err != nil {
		t.Fatalf("a tenant without a settlement closes = %v", err)
	}
}

// The hard close is for a tenant without a closure: with a settlement it is
// refused, before it calls Kill Bill, and without one it works.
func TestHardCloseIsRefusedForATenantWithASettlement(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := e.mustOrg(e.person("hard-owner@example.test"), "Hard close")
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at) VALUES ($1, $2, now())`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Close(ctx, e.db.Pool, e.eng, tn.Scope(), tenant.SystemActor, "hard close", "c"); code(err) != "tenant_closing" {
		t.Fatalf("close of a tenant with a settlement = %v, want tenant_closing", err)
	}
	if account, _ := e.fake.Account(e.brand.Slug, tn.PublicID()); account.Closed {
		t.Fatal("the Kill Bill account was closed")
	}
	plain := e.mustOrg(e.person("plain2-owner@example.test"), "No closure")
	if got, err := tenant.Close(ctx, e.db.Pool, e.eng, plain.Scope(), tenant.SystemActor, "hard close", "c"); err != nil || got.Status != tenant.StatusClosed {
		t.Fatalf("close of a tenant without a settlement = %+v %v", got, err)
	}
}
