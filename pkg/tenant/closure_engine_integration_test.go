//go:build integration

package tenant_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

type fakeDrain struct {
	done  bool
	calls int
	// onCall, when set, runs when the drain is asked.
	onCall func()
}

func (f *fakeDrain) Drained(context.Context, scope.Scope) (bool, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall()
	}
	return f.done, nil
}

func engineCreds(e *env) killbill.TenantCredentials {
	return killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug}
}

// closingWithSubscriptions is a closing tenant at cancelling with a base
// subscription and an add-on in the fake Kill Bill.
func closingWithSubscriptions(t *testing.T, e *env) (tn tenant.Tenant, base, addOn string) {
	t.Helper()
	ctx := context.Background()
	tn = settlementAt(t, e, "cancelling", false, false)
	kb := e.fake.KillBill(t)
	cc := killbill.CallContext{CreatedBy: "test"}
	var err error
	base, err = kb.CreateSubscription(ctx, engineCreds(e), cc, tn.KillBillAccountID, enginetest.PlanUsage, "base-"+idgen.MustNewV7().String())
	if err != nil {
		t.Fatal(err)
	}
	sub, err := kb.Subscription(ctx, engineCreds(e), base)
	if err != nil {
		t.Fatal(err)
	}
	addOn, err = kb.CreateAddOn(ctx, engineCreds(e), cc, tn.KillBillAccountID, sub.BundleID, enginetest.PlanUsage, "addon-"+idgen.MustNewV7().String())
	if err != nil {
		t.Fatal(err)
	}
	return tn, base, addOn
}

func engineSteps(e *env, drain *fakeDrain) tenant.EngineClosureSteps {
	return tenant.EngineClosureSteps{Engines: e.eng, Drain: drain, Today: func() string { return enginetest.CancelDay }}
}

func receipt(t *testing.T, e *env, tn tenant.Tenant) killbill.CancelReceipt {
	t.Helper()
	bundles, err := e.fake.KillBill(t).AccountBundles(context.Background(), engineCreds(e), tn.KillBillAccountID)
	if err != nil {
		t.Fatal(err)
	}
	return killbill.CancelledOn(bundles, tn.KillBillAccountID, enginetest.CancelDay)
}

// Prepare cancels the base subscription immediately, which takes its add-on
// with it, reads the account back, and reports a strict receipt. It is
// idempotent: a second call finds it done and cancels nothing.
func TestEnginePrepareCancelsEverySubscriptionAndIsIdempotent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, _, _ := closingWithSubscriptions(t, e)
	drain := &fakeDrain{done: true}
	steps := engineSteps(e, drain)
	got, err := steps.Prepare(ctx, e.db.Pool, tn.Scope())
	if err != nil || !got.Cancelled || !got.Drained || got.TargetDate.Format("2006-01-02") != enginetest.CancelDay {
		t.Fatalf("prepare = %+v %v", got, err)
	}
	if r := receipt(t, e, tn); !r.OK() || r.Subscriptions != 2 {
		t.Fatalf("receipt after prepare = %+v", r)
	}
	cancels := len(e.fake.CancelRequests(e.brand.Slug))
	if cancels != 1 {
		t.Fatalf("cancel requests = %d, want 1: the base subscription, the add-on goes with it", cancels)
	}
	if again, err := steps.Prepare(ctx, e.db.Pool, tn.Scope()); err != nil || !again.Cancelled || len(e.fake.CancelRequests(e.brand.Slug)) != cancels {
		t.Fatalf("second prepare = %+v %v, cancel requests %d, want no new cancel", again, err, len(e.fake.CancelRequests(e.brand.Slug)))
	}
}

// Nothing is cancelled until the usage drain says the usage is all with Kill
// Bill, and once it does the subscriptions are cancelled.
func TestEnginePrepareCancelsOnlyOnceTheUsageIsDrained(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, _, _ := closingWithSubscriptions(t, e)
	drain := &fakeDrain{done: false}
	got, err := engineSteps(e, drain).Prepare(ctx, e.db.Pool, tn.Scope())
	if err != nil || got.Cancelled || got.Drained || drain.calls != 1 || len(e.fake.CancelRequests(e.brand.Slug)) != 0 {
		t.Fatalf("prepare before the drain = %+v %v, drain calls %d, cancels %d, want nothing cancelled", got, err, drain.calls, len(e.fake.CancelRequests(e.brand.Slug)))
	}
	noDrain := tenant.EngineClosureSteps{Engines: e.eng, Today: func() string { return enginetest.CancelDay }}
	if got, err := noDrain.Prepare(ctx, e.db.Pool, tn.Scope()); err != nil || got.Cancelled || len(e.fake.CancelRequests(e.brand.Slug)) != 0 {
		t.Fatalf("prepare without a drain = %+v %v, want nothing cancelled", got, err)
	}
	drain.done = true
	if got, err := engineSteps(e, drain).Prepare(ctx, e.db.Pool, tn.Scope()); err != nil || !got.Cancelled || !got.Drained {
		t.Fatalf("prepare after the drain = %+v %v, want cancelled and drained", got, err)
	}
	// An account with nothing to cancel is cancelled once drained.
	empty := settlementAt(t, e, "cancelling", false, false)
	if got, err := engineSteps(e, drain).Prepare(ctx, e.db.Pool, empty.Scope()); err != nil || !got.Cancelled || !got.Drained {
		t.Fatalf("prepare with nothing to cancel = %+v %v", got, err)
	}
}

// A cancel whose answer is lost is read back and found done, not repeated. A
// cancel that never took effect is an error, and is not repeated within the
// call: the next call reads the account first and then cancels.
func TestEnginePrepareReadsBackAfterAnUnknownCancel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, _, _ := closingWithSubscriptions(t, e)
	e.fake.LoseAnswers("/1.0/kb/subscriptions", 1)
	got, err := engineSteps(e, &fakeDrain{done: true}).Prepare(ctx, e.db.Pool, tn.Scope())
	if err != nil || !got.Cancelled || len(e.fake.CancelRequests(e.brand.Slug)) != 1 {
		t.Fatalf("lost answer = %+v %v, cancels %d, want read back and done after one cancel", got, err, len(e.fake.CancelRequests(e.brand.Slug)))
	}

	other, _, _ := closingWithSubscriptions(t, e)
	before := len(e.fake.CancelRequests(e.brand.Slug))
	e.fake.FailWrites("/1.0/kb/subscriptions", 1, 503)
	got, err = engineSteps(e, &fakeDrain{done: true}).Prepare(ctx, e.db.Pool, other.Scope())
	if err == nil || got.Cancelled || len(e.fake.CancelRequests(e.brand.Slug)) != before+1 {
		t.Fatalf("a cancel that failed = %+v %v, cancels %d, want an error and one attempt", got, err, len(e.fake.CancelRequests(e.brand.Slug))-before)
	}
	if got, err := engineSteps(e, &fakeDrain{done: true}).Prepare(ctx, e.db.Pool, other.Scope()); err != nil || !got.Cancelled {
		t.Fatalf("the next call = %+v %v, want it done", got, err)
	}
}

func TestEnginePrepareRefusesATenantThatIsNotClosing(t *testing.T) {
	e := newEnv(t)
	tn := e.mustOrg(e.person("not-closing@example.test"), "Not closing")
	if _, err := engineSteps(e, &fakeDrain{done: true}).Prepare(context.Background(), e.db.Pool, tn.Scope()); code(err) != "not_ready" {
		t.Fatalf("prepare of an active tenant = %v, want not_ready", err)
	}
}

func accountOf(e *env, tn tenant.Tenant) enginetest.Account {
	a, _ := e.fake.Account(e.brand.Slug, tn.PublicID())
	return a
}

// Block sets a blocking state that stops new entitlements and changes and
// leaves billing, so the final invoice and any balance stay collectible. It
// reads the state back, and a second call writes nothing.
func TestEngineBlockSetsTheClosedBlockingStateAndLeavesBillingCollectible(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := settlementAt(t, e, "blocking", true, false)
	steps := engineSteps(e, &fakeDrain{done: true})
	ok, err := steps.Block(ctx, e.db.Pool, tn.Scope())
	if err != nil || !ok {
		t.Fatalf("block = %v %v", ok, err)
	}
	account := accountOf(e, tn)
	last := account.Blocking[len(account.Blocking)-1]
	if last.StateName != tenant.BlockingClosed || last.Service != tenant.BlockingService || !last.IsBlockChange || !last.IsBlockEntitlement || last.IsBlockBilling {
		t.Fatalf("blocking state = %+v, want %s on %s blocking change and entitlement but not billing", last, tenant.BlockingClosed, tenant.BlockingService)
	}
	if account.Closed {
		t.Fatal("the Kill Bill account was closed: closing it writes off unpaid invoices")
	}
	n := len(account.Blocking)
	if ok, err := steps.Block(ctx, e.db.Pool, tn.Scope()); err != nil || !ok || len(accountOf(e, tn).Blocking) != n {
		t.Fatalf("second block = %v %v, states %d -> %d, want no new state", ok, err, n, len(accountOf(e, tn).Blocking))
	}
}

// A block whose answer is lost is read back and found done; one that did not
// take effect is an error, and the next call writes it.
func TestEngineBlockReadsBackAfterALostAnswerAndAFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := settlementAt(t, e, "blocking", true, false)
	steps := engineSteps(e, &fakeDrain{done: true})
	e.fake.LoseAnswers("/1.0/kb/accounts", 1)
	if ok, err := steps.Block(ctx, e.db.Pool, tn.Scope()); err != nil || !ok {
		t.Fatalf("block with a lost answer = %v %v, want read back and done", ok, err)
	}
	other := settlementAt(t, e, "blocking", true, false)
	e.fake.FailWrites("/1.0/kb/accounts", 1, 503)
	if ok, err := steps.Block(ctx, e.db.Pool, other.Scope()); err == nil || ok {
		t.Fatalf("block that failed = %v %v, want an error", ok, err)
	}
	if ok, err := steps.Block(ctx, e.db.Pool, other.Scope()); err != nil || !ok {
		t.Fatalf("the next block = %v %v, want done", ok, err)
	}
}

// A tenant that was suspended is blocked as closed: the closed state is the
// latest of the service, and it is the one in force.
func TestEngineBlockAfterASuspension(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := e.mustOrg(e.person("suspended-then-closing@example.test"), "Suspended then closing")
	if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, tn.Scope(), tenant.SystemActor, "overdue", "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status, cancellation_completed_at, drain_completed_at, finance_target_date, finance_completed_at) VALUES ($1, $2, now(), 'blocking', now(), now(), current_date, now())`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := engineSteps(e, &fakeDrain{done: true}).Block(ctx, e.db.Pool, tn.Scope()); err != nil || !ok {
		t.Fatalf("block = %v %v", ok, err)
	}
	account := accountOf(e, tn)
	if last := account.Blocking[len(account.Blocking)-1]; last.StateName != tenant.BlockingClosed || last.IsBlockBilling {
		t.Fatalf("latest state = %+v", last)
	}
}

// The whole closure through the sweeper with the real steps against the fake
// Kill Bill: cancel, drain, finance with nothing to invoice, block, complete,
// and the tenant ends closed with its account blocked and not closed.
func TestSweeperCompletesAClosureWithTheEngineSteps(t *testing.T) {
	e := newEnv(t)
	postgrestest.EnableClosureInvariants(t, e.db)
	ctx := context.Background()
	tn, _, _ := closingWithSubscriptions(t, e)
	steps := engineSteps(e, &fakeDrain{done: true})
	sweeper := &tenant.Sweeper{DB: e.db.Pool, Engines: e.eng, Closure: steps}
	for pass := 1; pass <= 3; pass++ {
		if _, err := sweeper.SweepOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if tenantStatus(t, e, tn) == "closed" {
			break
		}
		if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET next_attempt_at = now() - interval '1 second' WHERE tenant_id = $1`, tn.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := tenantStatus(t, e, tn); got != "closed" || settlementStatus(t, e, tn) != "completed" {
		t.Fatalf("tenant %q settlement %q, want closed and completed", got, settlementStatus(t, e, tn))
	}
	if r := receipt(t, e, tn); !r.OK() {
		t.Fatalf("receipt = %+v", r)
	}
	account := accountOf(e, tn)
	if account.Closed || account.Blocking[len(account.Blocking)-1].StateName != tenant.BlockingClosed {
		t.Fatalf("account closed = %v, blocking = %+v, want blocked and not closed", account.Closed, account.Blocking)
	}
}

// A tenant that was suspended has Kill Bill billing blocked. Prepare opens it
// before it does anything else, with the closed state, so the final invoice is
// not computed under a billing block. A tenant that was not suspended gets no
// write.
func TestEnginePrepareOpensBillingOfASuspendedTenantFirst(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := e.mustOrg(e.person("suspended-closing@example.test"), "Suspended closing")
	kb := e.fake.KillBill(t)
	if _, err := kb.CreateSubscription(ctx, engineCreds(e), killbill.CallContext{CreatedBy: "test"}, tn.KillBillAccountID, enginetest.PlanUsage, "susp-"+idgen.MustNewV7().String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.Suspend(ctx, e.db.Pool, e.eng, tn.Scope(), tenant.SystemActor, "overdue", "c"); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE id = $1`, tn.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at) VALUES ($1, $2, now())`, tn.BrandID, tn.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if last := accountOf(e, tn).Blocking[len(accountOf(e, tn).Blocking)-1]; !last.IsBlockBilling {
		t.Fatalf("the suspended tenant's billing is not blocked: %+v", last)
	}
	var atDrain killbill.BlockingState
	drain := &fakeDrain{done: true, onCall: func() {
		b := accountOf(e, tn).Blocking
		atDrain = b[len(b)-1]
	}}
	if got, err := engineSteps(e, drain).Prepare(ctx, e.db.Pool, tn.Scope()); err != nil || !got.Cancelled {
		t.Fatalf("prepare = %+v %v", got, err)
	}
	if atDrain.StateName != tenant.BlockingClosed || atDrain.IsBlockBilling {
		t.Fatalf("blocking state when the drain was asked = %+v, want %s with billing open", atDrain, tenant.BlockingClosed)
	}
	n := len(accountOf(e, tn).Blocking)
	if _, err := engineSteps(e, drain).Prepare(ctx, e.db.Pool, tn.Scope()); err != nil || len(accountOf(e, tn).Blocking) != n {
		t.Fatalf("a second prepare wrote a blocking state: %d -> %d, %v", n, len(accountOf(e, tn).Blocking), err)
	}

	// A tenant that was not suspended has no state to open.
	other, _, _ := closingWithSubscriptions(t, e)
	if _, err := engineSteps(e, &fakeDrain{done: true}).Prepare(ctx, e.db.Pool, other.Scope()); err != nil || len(accountOf(e, other).Blocking) != 0 {
		t.Fatalf("prepare of an active tenant wrote %d blocking states, %v", len(accountOf(e, other).Blocking), err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Kill Bill refuses the cancel with 500 and code 1031, as it does for a
// subscription in a state it cannot be cancelled from, and the subscription is
// still not cancelled when it is read: that is a case for a person, not a wait.
func TestEnginePrepareSendsARefusedCancelThatChangedNothingToAPerson(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, base, _ := closingWithSubscriptions(t, e)
	refused := 0
	engines := e.eng
	engines.KillBill = e.fake.KillBillThrough(t, func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/subscriptions/") {
				refused++
				body := `{"className":"org.killbill.billing.entitlement.api.EntitlementApiException","code":1031,"message":"Subscription is in state ACTIVE: Failed to cancel"}`
				return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			}
			return next.RoundTrip(r)
		})
	})
	steps := tenant.EngineClosureSteps{Engines: engines, Drain: &fakeDrain{done: true}, Today: func() string { return enginetest.CancelDay }}
	got, err := steps.Prepare(ctx, e.db.Pool, tn.Scope())
	if code(err) != "conflict" || got.Cancelled || refused == 0 {
		t.Fatalf("prepare = %+v %v, refused cancels %d, want the manual-review conflict", got, err, refused)
	}
	if sub, err := e.fake.KillBill(t).Subscription(ctx, engineCreds(e), base); err != nil || sub.State == "CANCELLED" {
		t.Fatalf("the subscription is %+v %v, want it left as it was", sub, err)
	}
	// It is the same answer on the next call: the closure does not wait quietly.
	if _, err := steps.Prepare(ctx, e.db.Pool, tn.Scope()); code(err) != "conflict" {
		t.Fatalf("second prepare = %v, want conflict again", err)
	}
}
