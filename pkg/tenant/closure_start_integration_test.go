//go:build integration

package tenant_test

import (
	"context"
	"strings"
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

var closureRequest = tenant.ClosureRequest{Reason: "customer asked to leave", RequestID: "closure-request-0001"}

func startClosure(e *env, tn tenant.Tenant) (tenant.ClosureStart, error) {
	return tenant.StartClosure(context.Background(), e.db.Pool, tn.Scope(), tenant.SystemActor, closureRequest, "corr-start")
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

func TestStartClosureRefusesWhatItMust(t *testing.T) {
	e, tn := closureStartEnv(t)
	ctx := context.Background()
	for _, reason := range []string{"", "   "} {
		if _, err := tenant.StartClosure(ctx, e.db.Pool, tn.Scope(), tenant.SystemActor, tenant.ClosureRequest{Reason: reason, RequestID: closureRequest.RequestID}, "c"); code(err) != "reason_required" {
			t.Fatalf("reason %q = %v, want reason_required", reason, err)
		}
	}
	if tenantStatus(t, e, tn) != "active" || settlementRows(t, e, tn) != 0 {
		t.Fatal("a refused start changed the tenant")
	}
	unknown := scope.Scope{Brand: tn.BrandID, Tenant: idgen.MustNewV7()}
	if _, err := tenant.StartClosure(ctx, e.db.Pool, unknown, tenant.SystemActor, closureRequest, "c"); code(err) != "tenant_not_found" {
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

// The settlement keeps who asked for the closure, why, and under which request
// ID, and none of it can be changed afterwards.
func TestStartClosureRecordsTheRequestAndKeepsItFixed(t *testing.T) {
	e, tn := closureStartEnv(t)
	ctx := context.Background()
	got, err := startClosure(e, tn)
	if err != nil || got.Reason != closureRequest.Reason || got.RequestID != closureRequest.RequestID {
		t.Fatalf("start = %+v %v", got, err)
	}
	var by, reason, request string
	if err := e.db.Pool.QueryRow(ctx, `SELECT started_by, start_reason, start_request_id FROM tenant_closure_settlements WHERE tenant_id = $1`, tn.ID).Scan(&by, &reason, &request); err != nil {
		t.Fatal(err)
	}
	if by != string(tenant.SystemActor.Type)+":"+tenant.SystemActor.ID || reason != closureRequest.Reason || request != closureRequest.RequestID {
		t.Fatalf("stored = %q %q %q", by, reason, request)
	}
	for _, column := range []string{"started_by = 'someone else'", "start_reason = 'another reason'", "start_request_id = 'another-request-id'"} {
		if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET `+column+` WHERE tenant_id = $1`, tn.ID); err == nil {
			t.Errorf("%s was changed after the start", column)
		}
	}
}

// The request ID makes the start idempotent: the same ID and reason is a
// replay, the same ID with another reason conflicts, and another ID on a
// closure that has started gets the closure that exists.
func TestStartClosureReplaysByRequestID(t *testing.T) {
	e, tn := closureStartEnv(t)
	ctx := context.Background()
	first, err := startClosure(e, tn)
	if err != nil || !first.Started {
		t.Fatalf("first = %+v %v", first, err)
	}
	same, err := tenant.StartClosure(ctx, e.db.Pool, tn.Scope(), tenant.SystemActor, closureRequest, "c")
	if err != nil || same.Started || same.Reason != closureRequest.Reason {
		t.Fatalf("same request = %+v %v, want a replay", same, err)
	}
	other := tenant.ClosureRequest{Reason: "a different reason", RequestID: closureRequest.RequestID}
	if _, err := tenant.StartClosure(ctx, e.db.Pool, tn.Scope(), tenant.SystemActor, other, "c"); code(err) != "idempotency_key_conflict" {
		t.Fatalf("same ID, other reason = %v, want idempotency_key_conflict", err)
	}
	second, err := tenant.StartClosure(ctx, e.db.Pool, tn.Scope(), tenant.SystemActor, tenant.ClosureRequest{Reason: "second caller", RequestID: "closure-request-0002"}, "c")
	if err != nil || second.Started || second.Reason != closureRequest.Reason || second.RequestID != closureRequest.RequestID || !second.CutoffAt.Equal(first.CutoffAt) {
		t.Fatalf("another ID = %+v %v, want the closure that exists", second, err)
	}
}

func TestStartClosureNeedsAValidRequestID(t *testing.T) {
	e, tn := closureStartEnv(t)
	for _, id := range []string{"", "short", strings.Repeat("x", 256)} {
		if _, err := tenant.StartClosure(context.Background(), e.db.Pool, tn.Scope(), tenant.SystemActor, tenant.ClosureRequest{Reason: "r", RequestID: id}, "c"); code(err) != "validation_failed" {
			t.Errorf("request ID %q = %v, want validation_failed", id, err)
		}
	}
	if tenantStatus(t, e, tn) != "active" || settlementRows(t, e, tn) != 0 {
		t.Fatal("a refused start changed the tenant")
	}
}

// Callers at once make exactly one settlement, one start and one audit. They
// serialise on the lifecycle capability, so the others are told the tenant is
// busy or find the closure that exists.
func TestStartClosureConcurrentCallsStartItOnce(t *testing.T) {
	e, tn := closureStartEnv(t)
	var started, answered atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := startClosure(e, tn)
			switch {
			case err == nil && got.Started:
				started.Add(1)
			case err == nil, code(err) == "not_ready":
				answered.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var audits int
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_started'`, tn.ID).Scan(&audits); err != nil || started.Load() != 1 || started.Load()+answered.Load() != 6 || audits != 1 || settlementRows(t, e, tn) != 1 {
		t.Fatalf("started = %d answered = %d audits = %d settlements = %d, %v, want one start, one audit, one settlement", started.Load(), answered.Load(), audits, settlementRows(t, e, tn), err)
	}
}

// A closure cannot start while Suspend, Reactivate or Close is in the middle
// of its engine call: both hold the lifecycle capability of the tenant, so the
// start is refused as busy and the command finishes, with Kill Bill, Keycloak
// and Core in step. The tenant can start its closure once the command is done.
func TestStartClosureDuringALifecycleCommandIsRefusedAndTheCommandCompletes(t *testing.T) {
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
			var duringErr error
			e.fake.AfterWrite(func(string, string) {
				if started.CompareAndSwap(false, true) {
					_, duringErr = startClosure(e, tn)
				}
			})
			t.Cleanup(func() { e.fake.AfterWrite(nil) })
			var got tenant.Tenant
			var err error
			switch command {
			case "suspend":
				got, err = tenant.Suspend(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c")
			case "reactivate":
				got, err = tenant.Reactivate(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c")
			case "close":
				got, err = tenant.Close(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c")
			}
			e.fake.AfterWrite(nil)
			if !started.Load() {
				t.Fatal("the closure start was never tried in the middle of the command")
			}
			if code(err) != "ok" {
				t.Fatalf("%s = %v, want it to complete", command, err)
			}
			if code(duringErr) != "not_ready" {
				t.Fatalf("the closure start during %s = %v, want not_ready", command, duringErr)
			}
			want := map[string]tenant.Status{"suspend": tenant.StatusSuspended, "reactivate": tenant.StatusActive, "close": tenant.StatusClosed}[command]
			account, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
			org, _, _ := e.fake.Organization(e.brand.Realm, tn.PublicID())
			if got.Status != want || tenantStatus(t, e, tn) != string(want) || settlementRows(t, e, tn) != 0 {
				t.Fatalf("status = %q (stored %q), settlements %d, want %q and none", got.Status, tenantStatus(t, e, tn), settlementRows(t, e, tn), want)
			}
			blocked := len(account.Blocking) > 0 && account.Blocking[len(account.Blocking)-1].IsBlockBilling
			switch command {
			case "suspend":
				if !blocked || org.Enabled || account.Closed {
					t.Fatalf("suspend left Kill Bill blocked = %v, Organization enabled = %v, closed = %v", blocked, org.Enabled, account.Closed)
				}
			case "reactivate":
				if blocked || !org.Enabled || account.Closed {
					t.Fatalf("reactivate left Kill Bill blocked = %v, Organization enabled = %v, closed = %v", blocked, org.Enabled, account.Closed)
				}
			case "close":
				if !account.Closed || org.Enabled {
					t.Fatalf("close left the Kill Bill account closed = %v, Organization enabled = %v", account.Closed, org.Enabled)
				}
			}
			// The capability was released: once the command is done a tenant that
			// can still be closed starts its closure.
			if command != "close" {
				if res, err := startClosure(e, tn); err != nil || !res.Started {
					t.Fatalf("closure start after %s = %+v %v", command, res, err)
				}
			}
		})
	}
}

// While another owner holds the lifecycle capability of the tenant, Suspend,
// Reactivate, Close and StartClosure are refused as busy and touch nothing:
// not Core, not Kill Bill, not Keycloak.
func TestLifecycleCommandsAndStartClosureAreBusyWhileTheCapabilityIsHeld(t *testing.T) {
	e, tn := closureStartEnv(t)
	ctx := context.Background()
	guard, got, err := tenant.AcquireLifecycleGuard(ctx, e.db.Pool, tn.Scope())
	if err != nil || !got {
		t.Fatalf("acquire = %v %v", got, err)
	}
	before, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
	actor := tenant.SystemActor
	for name, run := range map[string]func() error{
		"suspend": func() error { _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c"); return err },
		"reactivate": func() error {
			_, err := tenant.Reactivate(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c")
			return err
		},
		"close":         func() error { _, err := tenant.Close(ctx, e.db.Pool, e.eng, tn.Scope(), actor, "r", "c"); return err },
		"start closure": func() error { _, err := startClosure(e, tn); return err },
	} {
		if err := run(); code(err) != "not_ready" {
			t.Errorf("%s while the capability is held = %v, want not_ready", name, err)
		}
	}
	after, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
	org, _, _ := e.fake.Organization(e.brand.Realm, tn.PublicID())
	if len(after.Blocking) != len(before.Blocking) || after.Closed || !org.Enabled || tenantStatus(t, e, tn) != "active" || settlementRows(t, e, tn) != 0 {
		t.Fatalf("a busy command changed something: blocking %d -> %d, closed %v, org enabled %v, status %q", len(before.Blocking), len(after.Blocking), after.Closed, org.Enabled, tenantStatus(t, e, tn))
	}
	if err := guard.Release(); err != nil {
		t.Fatal(err)
	}
	if res, err := startClosure(e, tn); err != nil || !res.Started {
		t.Fatalf("closure start after the release = %+v %v", res, err)
	}
}

// Once a closure has started, Close is refused before it calls Kill Bill: the
// account is not closed, so the closure still has its subscriptions to cancel
// and its usage to invoice.
func TestCloseOfAClosingTenantDoesNotCloseTheKillBillAccount(t *testing.T) {
	e, tn := closureStartEnv(t)
	ctx := context.Background()
	if _, err := startClosure(e, tn); err != nil {
		t.Fatal(err)
	}
	before, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
	if _, err := tenant.Close(ctx, e.db.Pool, e.eng, tn.Scope(), tenant.SystemActor, "hard close", "c"); code(err) != "not_ready" {
		t.Fatalf("close of a closing tenant = %v, want not_ready", err)
	}
	after, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
	if after.Closed || len(after.Blocking) != len(before.Blocking) {
		t.Fatalf("the Kill Bill account was touched: closed %v, blocking %d -> %d", after.Closed, len(before.Blocking), len(after.Blocking))
	}
}
