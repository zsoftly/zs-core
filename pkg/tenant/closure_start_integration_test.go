//go:build integration

package tenant_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func closureStartEnv(t *testing.T) (*env, tenant.Tenant) {
	t.Helper()
	e := newEnv(t)
	postgrestest.EnableSettlementStartRule(t, e.db)
	return e, e.mustOrg(e.person("closer@example.test"), "Closing tenant")
}

func startClosure(e *env, tn tenant.Tenant) (tenant.ClosureStart, error) {
	return tenant.StartClosure(context.Background(), e.db.Pool, tn.Scope(), tenant.SystemActor, "customer asked to leave", "corr-start")
}

func tenantStatus(t *testing.T, e *env, tn tenant.Tenant) string {
	t.Helper()
	var status string
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT status FROM tenants WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func settlementRows(t *testing.T, e *env, tn tenant.Tenant) int {
	t.Helper()
	var n int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM tenant_closure_settlements WHERE tenant_id = $1`, tn.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Starting a closure moves the tenant to closing and creates its settlement at
// cancelling in one transaction, audited with the reason.
func TestStartClosureMovesTheTenantToClosingWithItsSettlement(t *testing.T) {
	for _, from := range []string{"active", "suspended"} {
		e, tn := closureStartEnv(t)
		ctx := context.Background()
		if from == "suspended" {
			if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, tn.ID); err != nil {
				t.Fatal(err)
			}
		}
		got, err := startClosure(e, tn)
		if err != nil || !got.Started || got.Tenant.Status != tenant.StatusClosing || got.CutoffAt.IsZero() {
			t.Fatalf("from %s: start = %+v %v", from, got, err)
		}
		var status, state string
		var attempts int
		var drift float64
		if err := e.db.Pool.QueryRow(ctx, `SELECT t.status, s.status, s.attempts, abs(extract(epoch FROM s.cutoff_at - now()))
FROM tenants t JOIN tenant_closure_settlements s ON s.brand_id = t.brand_id AND s.tenant_id = t.id WHERE t.id = $1`, tn.ID).Scan(&status, &state, &attempts, &drift); err != nil {
			t.Fatal(err)
		}
		if status != "closing" || state != "cancelling" || attempts != 0 || drift > 30 {
			t.Fatalf("from %s: tenant %q settlement %q attempts %d cutoff drift %.0fs", from, status, state, attempts, drift)
		}
		var audits int
		var reason string
		if err := e.db.Pool.QueryRow(ctx, `SELECT count(*), coalesce(max(reason), '') FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_started'`, tn.ID).Scan(&audits, &reason); err != nil || audits != 1 || reason != "customer asked to leave" {
			t.Fatalf("from %s: audits = %d reason = %q, %v", from, audits, reason, err)
		}
	}
}

// A second start is harmless: same cutoff, no second settlement, no second audit.
func TestStartClosureIsIdempotent(t *testing.T) {
	e, tn := closureStartEnv(t)
	first, err := startClosure(e, tn)
	if err != nil || !first.Started {
		t.Fatalf("first = %+v %v", first, err)
	}
	second, err := startClosure(e, tn)
	if err != nil || second.Started || !second.CutoffAt.Equal(first.CutoffAt) || second.Tenant.Status != tenant.StatusClosing {
		t.Fatalf("second = %+v %v, want not started with the same cutoff %v", second, err, first.CutoffAt)
	}
	var audits int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_started'`, tn.ID).Scan(&audits); err != nil || audits != 1 || settlementRows(t, e, tn) != 1 {
		t.Fatalf("audits = %d settlements = %d, %v, want one each", audits, settlementRows(t, e, tn), err)
	}
}

// Two starts at once make exactly one settlement, one start and one audit.
func TestStartClosureConcurrentCallsStartItOnce(t *testing.T) {
	e, tn := closureStartEnv(t)
	var started atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := startClosure(e, tn)
			if err != nil {
				t.Error(err)
				return
			}
			if got.Started {
				started.Add(1)
			}
		}()
	}
	wg.Wait()
	var audits int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_started'`, tn.ID).Scan(&audits); err != nil || started.Load() != 1 || audits != 1 || settlementRows(t, e, tn) != 1 {
		t.Fatalf("started = %d audits = %d settlements = %d, %v, want one each", started.Load(), audits, settlementRows(t, e, tn), err)
	}
}

func TestStartClosureRefusesWhatItMust(t *testing.T) {
	e, tn := closureStartEnv(t)
	ctx := context.Background()
	for _, reason := range []string{"", "   "} {
		if _, err := tenant.StartClosure(ctx, e.db.Pool, tn.Scope(), tenant.SystemActor, reason, "c"); code(err) != "reason_required" {
			t.Fatalf("reason %q = %v, want reason_required", reason, err)
		}
	}
	if tenantStatus(t, e, tn) != "active" || settlementRows(t, e, tn) != 0 {
		t.Fatal("a refused start changed the tenant")
	}
	unknown := scope.Scope{Brand: tn.BrandID, Tenant: idgen.MustNewV7()}
	if _, err := tenant.StartClosure(ctx, e.db.Pool, unknown, tenant.SystemActor, "reason", "c"); code(err) != "tenant_not_found" {
		t.Fatalf("unknown tenant = %v, want not found", err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closed' WHERE id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := startClosure(e, tn); code(err) != "tenant_closed" || settlementRows(t, e, tn) != 0 {
		t.Fatalf("closed tenant = %v settlements %d, want tenant_closed and none", err, settlementRows(t, e, tn))
	}
}

// A settlement that already exists for a tenant that is not closing is not
// overwritten, and the tenant keeps its status: the start is all or nothing.
func TestStartClosureIsAllOrNothing(t *testing.T) {
	e, tn := closureStartEnv(t)
	ctx := context.Background()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at) VALUES ($1, $2, now() - interval '1 day')`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := startClosure(e, tn); code(err) != "conflict" {
		t.Fatalf("a start over an existing settlement = %v, want conflict", err)
	}
	var audits int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_started'`, tn.ID).Scan(&audits); err != nil || audits != 0 || tenantStatus(t, e, tn) != "active" {
		t.Fatalf("status = %q audits = %d, %v, want active and none", tenantStatus(t, e, tn), audits, err)
	}
}

// A tenant that is closing cannot be suspended, reactivated or closed the old
// way: the settlement is what ends it.
func TestClosingTenantRefusesSuspendReactivateAndClose(t *testing.T) {
	e, tn := closureStartEnv(t)
	ctx := context.Background()
	if _, err := startClosure(e, tn); err != nil {
		t.Fatal(err)
	}
	actor := tenant.SystemActor
	for name, run := range map[string]func() error{
		"suspend": func() error { _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c"); return err },
		"reactivate": func() error {
			_, err := tenant.Reactivate(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c")
			return err
		},
		"close": func() error { _, err := tenant.Close(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c"); return err },
	} {
		if err := run(); code(err) != "not_ready" {
			t.Errorf("%s of a closing tenant = %v, want not_ready", name, err)
		}
	}
	if got := tenantStatus(t, e, tn); got != "closing" {
		t.Fatalf("status after the refusals = %q, want closing", got)
	}
}

// The sweeper picks up a tenant whose closure has started, and with no steps
// plugged in it waits at cancelling.
func TestStartedClosureIsPickedUpBySweeper(t *testing.T) {
	e, tn := closureStartEnv(t)
	if _, err := startClosure(e, tn); err != nil {
		t.Fatal(err)
	}
	report, err := (&tenant.Sweeper{DB: e.db.Pool, Engines: e.eng}).SweepOnce(context.Background())
	if err != nil || report.ClosuresChecked != 1 || report.ClosuresWaiting != 1 {
		t.Fatalf("sweep = %+v %v, want the started closure checked and waiting", report, err)
	}
	if got := settlementStatus(t, e, tn); got != "cancelling" {
		t.Fatalf("settlement = %q, want cancelling", got)
	}
	if org, _, _ := e.fake.Organization(e.brand.Realm, tn.PublicID()); org.Enabled {
		t.Fatal("the Organization of a closing tenant stayed enabled after the sweep")
	}
}

// A closure that starts while Suspend, Reactivate or Close is calling the
// engines wins. The command is refused, the tenant stays closing with its
// settlement, nothing is audited as the command's own change, and the
// Organization the command may have enabled is disabled again.
func TestStartClosureDuringALifecycleCommandWins(t *testing.T) {
	for _, command := range []string{"suspend", "reactivate", "close"} {
		t.Run(command, func(t *testing.T) {
			e, tn := closureStartEnv(t)
			ctx := context.Background()
			actor := tenant.SystemActor
			if command == "reactivate" {
				if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "overdue", "c"); err != nil {
					t.Fatal(err)
				}
			}
			var started atomic.Bool
			e.fake.AfterWrite(func(string, string) {
				if started.CompareAndSwap(false, true) {
					if _, err := startClosure(e, tn); err != nil {
						t.Error(err)
					}
				}
			})
			t.Cleanup(func() { e.fake.AfterWrite(nil) })
			var err error
			switch command {
			case "suspend":
				_, err = tenant.Suspend(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c")
			case "reactivate":
				_, err = tenant.Reactivate(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c")
			case "close":
				_, err = tenant.Close(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c")
			}
			e.fake.AfterWrite(nil)
			if !started.Load() {
				t.Fatal("the closure was never started in the middle of the command")
			}
			if code(err) != "not_ready" {
				t.Fatalf("%s during a closure start = %v, want not_ready", command, err)
			}
			if got := tenantStatus(t, e, tn); got != "closing" || settlementRows(t, e, tn) != 1 {
				t.Fatalf("status = %q settlements = %d, want closing and one", got, settlementRows(t, e, tn))
			}
			var own int
			if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = $2`, tn.ID,
				map[string]string{"suspend": "tenant.suspended", "reactivate": "tenant.reactivated", "close": "tenant.closed"}[command]).Scan(&own); err != nil {
				t.Fatal(err)
			}
			if own != 0 {
				t.Fatalf("%d audit entries of the refused %s command, want none", own, command)
			}
			if org, _, _ := e.fake.Organization(e.brand.Realm, tn.PublicID()); org.Enabled {
				t.Fatalf("the Organization is enabled after a refused %s", command)
			}
		})
	}
}
