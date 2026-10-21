//go:build integration

package wallet_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

// meters gives the brand the meter api-calls on the unit api-call.
func (e *env) meters() {
	e.t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, _, err := brand.ReplaceMeters(ctx, tx, e.brand.ID, []brand.Meter{{Slug: "api-calls", Unit: "request", KillBillUnit: enginetest.UnitAPICall}},
			brand.FactsFromCatalog([]killbill.Catalog{enginetest.TestCatalog()}), events.Actor{Type: events.ActorStaff, ID: "sub:op"}, "test", "test")
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// metered subscribes the tenant to the plan that prices api-call at one
// cent per call.
func (e *env) metered(tn tenant.Tenant) string {
	e.t.Helper()
	id, err := e.fake.KillBill(e.t).CreateSubscription(context.Background(), killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "test"}, tn.KillBillAccountID, enginetest.PlanMetered, "metered-"+idgen.MustNewV7().String())
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// forward records calls of usage and forwards it to Kill Bill.
func (e *env) forward(tn tenant.Tenant, calls int64) {
	e.t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, _, err := e.svc.Metering.RecordUsage(ctx, tx, tn.Scope(), metering.Call{Actor: e.call.Actor, CorrelationID: "c"},
			metering.Event{Meter: "api-calls", EventID: "use-" + idgen.MustNewV7().String(), QuantityMicro: calls * 1_000_000})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	if r, err := e.svc.Metering.Forward(ctx); err != nil || r.Failed != 0 {
		e.t.Fatalf("forward = %+v %v", r, err)
	}
}

func (e *env) reserveFor(sc scope.Scope, ref string, amount int64, lease time.Duration) (wallet.Reservation, error) {
	var r wallet.Reservation
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		r, _, err = e.svc.Reserve(context.Background(), tx, sc, e.call, wallet.ReserveRequest{ExternalRef: ref, AmountMinor: amount, Lease: lease})
		return err
	})
	return r, err
}

func heldSum(t *testing.T, e *env, tn tenant.Tenant) int64 {
	t.Helper()
	var v int64
	if err := e.db.Pool.QueryRow(context.Background(), `SELECT coalesce(sum(amount_minor), 0) FROM reservations WHERE tenant_id = $1 AND status = 'held'`, tn.ID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func relayOnce(t *testing.T, e *env, ctx context.Context) (int, error) {
	t.Helper()
	// A claim of an earlier, cancelled relay ends, as it would after its
	// five minutes.
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE outbox SET claimed_until = NULL, claim_token = NULL, next_attempt_at = now()
WHERE event_type = 'wallet.settlement.requested' AND published_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	relay := outbox.NewRelay("test", outbox.NewPostgresReader(e.db.Pool).WithEventTypes(wallet.EventSettlementRequested), e.svc.RelayHandler(), 0, 0, nil)
	return relay.RunOnce(ctx)
}

// Gate 1. Concurrent callers never hold more than the Kill Bill credit,
// less the unbilled usage, plus the overdraft, while the credit cache
// refreshes and credit arrives mid-run. A monitor checks the sum of the
// holds at every step.
func TestOverspendNeverExceedsAvailableCredit(t *testing.T) {
	e := newEnv(t, "zs-gate-overspend")
	e.policy(100, 0)
	e.meters()
	tn := e.tenant("Overspend Co")
	sc := tn.Scope()
	e.credit(tn, 2000)
	e.metered(tn)
	// Forwarded usage first: 300 calls at one cent are 300 unbilled.
	e.forward(tn, 300)
	v := e.view(sc)
	// The view and Reserve share one number: 2000 - 300 + 100.
	if v.UnbilledMinor == nil || *v.UnbilledMinor != 300 || avail(v) != 1800 {
		t.Fatalf("view after usage = %+v", v)
	}
	// Exactly credit - unbilled + overdraft is available: 1800.
	if _, err := e.reserveFor(sc, "edge", 1801, time.Minute); codeOf(err) != "insufficient_credit" {
		t.Fatalf("one past the edge: %v", err)
	}
	if _, err := e.reserveFor(sc, "edge", 1800, time.Minute); err != nil {
		t.Fatalf("at the edge: %v", err)
	}
	if _, err := e.release(sc, "edge", 0); err != nil {
		t.Fatal(err)
	}

	var (
		stop      = make(chan struct{})
		wg        sync.WaitGroup
		violation atomic.Value
	)
	const unbilled, overdraft = 300, 100
	// The monitor: the holds, read from the rows, never pass the credit
	// Kill Bill holds, which only grows during the run.
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			held := heldSum(t, e, tn)
			if kb := e.fake.Credit(e.brand.Slug, tn.KillBillAccountID); held > kb-unbilled+overdraft {
				violation.Store(fmt.Sprintf("held %d > credit %d - unbilled %d + overdraft %d", held, kb, unbilled, overdraft))
			}
		}
	}()
	// The cache: the clock moves past the TTL again and again, so callers
	// refresh the Kill Bill credit mid-run, and credit arrives.
	cacheDone := make(chan struct{})
	go func() {
		defer close(cacheDone)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			e.clock.advance(3 * time.Second)
			if i%10 == 0 {
				e.credit(tn, 50)
			}
		}
	}()
	rng := rand.New(rand.NewPCG(4, 2))
	var amounts [40]int64
	for i := range amounts {
		amounts[i] = int64(50 + rng.IntN(300))
	}
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := fmt.Sprintf("job-%d", i)
			if _, err := e.reserveFor(sc, ref, amounts[i], time.Hour); err != nil {
				if codeOf(err) != "insufficient_credit" {
					t.Errorf("reserve %s: %v", ref, err)
				}
				return
			}
			if _, err := e.reserveFor(sc, ref, amounts[i]*2, time.Hour); err != nil && codeOf(err) != "insufficient_credit" {
				t.Errorf("grow %s: %v", ref, err)
			}
			if i%4 == 0 {
				if _, err := e.release(sc, ref, 0); err != nil {
					t.Errorf("release %s: %v", ref, err)
				}
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	<-monitorDone
	<-cacheDone
	if msg := violation.Load(); msg != nil {
		t.Fatal(msg)
	}
	held := heldSum(t, e, tn)
	kb := e.fake.Credit(e.brand.Slug, tn.KillBillAccountID)
	if held > kb-unbilled+overdraft || held == 0 {
		t.Fatalf("held %d against credit %d", held, kb)
	}
	t.Logf("held %d of credit %d less unbilled %d plus overdraft %d", held, kb, unbilled, overdraft)
}

// Gate 1, failing closed: when Kill Bill cannot rate the unbilled usage,
// the view says so and no hold is granted.
func TestUnbilledUnknownFailsClosed(t *testing.T) {
	e := newEnv(t, "zs-gate-unbilled")
	tn := e.tenant("Unknown Co")
	sc := tn.Scope()
	e.credit(tn, 1000)
	e.fake.SetDryRunDown(true)
	if _, err := e.reserveFor(sc, "job", 10, time.Minute); codeOf(err) != "wallet_unavailable" {
		t.Fatalf("reserve without the unbilled amount: %v", err)
	}
	if v := e.view(sc); v.UnbilledMinor != nil || v.AvailableMinor != nil || v.KillBillCreditMinor != 1000 || v.LowBalance {
		t.Fatalf("view without the unbilled amount = %+v", v)
	}
	e.fake.SetDryRunDown(false)
	if _, err := e.reserveFor(sc, "job", 10, time.Minute); err != nil {
		t.Fatal(err)
	}
}

// Gate 2. A settlement retried, after a lost answer or a crash between
// the Kill Bill write and the Core commit, makes exactly one Kill Bill
// object, and the Core row names it.
func TestSettlementRetriesAreIdempotent(t *testing.T) {
	e := newEnv(t, "zs-gate-retry")
	e.meters()
	ctx := context.Background()
	tn := e.tenant("Retry Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	sub := e.metered(tn)
	charges := func() int { return e.fake.Charges(e.brand.Slug, tn.KillBillAccountID, wallet.ChargeDescriptionPrefix) }
	usage := func(id string) int {
		n := 0
		for _, u := range e.fake.UsageRecords(e.brand.Slug) {
			if u.TrackingID == id {
				n++
			}
		}
		return n
	}
	chargeItem := func(r wallet.Reservation) {
		t.Helper()
		in, err := e.fake.KillBill(t).AccountInvoices(ctx, killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug}, tn.KillBillAccountID, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range in {
			for _, it := range i.Items {
				if it.Description == wallet.ChargeDescription(sc, r.ExternalRef, r.Attempt) && it.InvoiceItemID == r.KillBillRef {
					return
				}
			}
		}
		t.Fatalf("reservation %s names %s, not its Kill Bill charge", r.ID, r.KillBillRef)
	}

	// The same settlement twice.
	for _, ref := range []string{"twice-charge", "twice-usage"} {
		if _, _, err := e.reserve(sc, ref, 0, 100); err != nil {
			t.Fatal(err)
		}
	}
	a, err := e.settle(sc, "twice-charge", 0, wallet.Settlement{CreditMinor: 40})
	if err != nil {
		t.Fatalf("initial charge settlement: %v", err)
	}
	b, err := e.settle(sc, "twice-charge", 0, wallet.Settlement{CreditMinor: 40})
	if err != nil {
		t.Fatalf("replayed charge settlement: %v", err)
	}
	if charges() != 1 || a.KillBillRef != b.KillBillRef {
		t.Fatalf("charge settled twice: %d charges", charges())
	}
	chargeItem(a)
	use := wallet.Settlement{Usage: &wallet.UsageSettlement{Unit: enginetest.UnitAPICall, QuantityMicro: 3_000_000, Subscription: sub}}
	u1, err := e.settle(sc, "twice-usage", 0, use)
	if err != nil {
		t.Fatalf("initial usage settlement: %v", err)
	}
	u2, err := e.settle(sc, "twice-usage", 0, use)
	if err != nil {
		t.Fatalf("replayed usage settlement: %v", err)
	}
	if usage(u1.KillBillRef) != 1 || u1.KillBillRef != u2.KillBillRef {
		t.Fatalf("usage settled twice: %d records", usage(u1.KillBillRef))
	}

	// The answer of the Kill Bill write is lost: the settlement is queued,
	// and the relay finds the charge Kill Bill made instead of writing it
	// again.
	for _, ref := range []string{"lost-charge", "lost-usage"} {
		if _, _, err := e.reserve(sc, ref, 0, 100); err != nil {
			t.Fatal(err)
		}
	}
	e.fake.LoseAnswers("/1.0/kb/invoices/charges", 1)
	if p, err := e.settle(sc, "lost-charge", 0, wallet.Settlement{CreditMinor: 30}); err != nil || !p.SettlementPending {
		t.Fatalf("lost charge answer = %+v %v", p, err)
	}
	e.fake.LoseAnswers("/1.0/kb/usages", 1)
	if p, err := e.settle(sc, "lost-usage", 0, use); err != nil || !p.SettlementPending {
		t.Fatalf("lost usage answer = %+v %v", p, err)
	}
	writesBefore := countWrites(e, "/1.0/kb/invoices/charges")
	if n, err := relayOnce(t, e, ctx); err != nil || n != 2 {
		t.Fatalf("relay = %d %v", n, err)
	}
	if countWrites(e, "/1.0/kb/invoices/charges") != writesBefore {
		t.Fatal("the relay wrote the lost charge again")
	}
	lc, _ := wallet.Get(ctx, e.db.Pool, sc, "lost-charge", 0)
	lu, _ := wallet.Get(ctx, e.db.Pool, sc, "lost-usage", 0)
	if lc.Status != wallet.StatusSettled || lu.Status != wallet.StatusSettled || charges() != 2 || usage(lu.KillBillRef) != 1 {
		t.Fatalf("after the relay: %+v %+v, %d charges, %d usage records", lc, lu, charges(), usage(lu.KillBillRef))
	}
	chargeItem(lc)

	// A crash between the Kill Bill write and the Core commit: the
	// transaction rolls back after Kill Bill took the write. The retry
	// finds Kill Bill's object.
	for _, ref := range []string{"crash-charge", "crash-usage"} {
		if _, _, err := e.reserve(sc, ref, 0, 100); err != nil {
			t.Fatal(err)
		}
	}
	for ref, st := range map[string]wallet.Settlement{"crash-charge": {CreditMinor: 20}, "crash-usage": use} {
		tx, err := e.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Settle(ctx, tx, sc, e.call, ref, 0, st); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback(ctx)
		if r, _ := wallet.Get(ctx, e.db.Pool, sc, ref, 0); r.Status != wallet.StatusHeld || r.SettlementPending {
			t.Fatalf("%s after the crash = %+v", ref, r)
		}
	}
	if charges() != 3 {
		t.Fatalf("charges after the crash = %d", charges())
	}
	cc, err := e.settle(sc, "crash-charge", 0, wallet.Settlement{CreditMinor: 20})
	if err != nil || charges() != 3 {
		t.Fatalf("retry after the crash = %+v %v, %d charges", cc, err, charges())
	}
	chargeItem(cc)
	cu, err := e.settle(sc, "crash-usage", 0, use)
	if err != nil || usage(cu.KillBillRef) != 1 {
		t.Fatalf("usage retry after the crash = %+v %v", cu, err)
	}
}

func countWrites(e *env, prefix string) int {
	n := 0
	for _, w := range e.fake.Writes() {
		if strings.HasPrefix(w.Path, prefix) {
			n++
		}
	}
	return n
}

// Gate 3. An outage makes no charge, keeps the holds held and pending,
// and the queued settlements complete once each. A pending hold never
// expires, even past its lease, so no settlement is ever abandoned. The
// database failing after the Kill Bill write is recovered from Kill Bill's
// object.
func TestOutagesNeverChargeWrongly(t *testing.T) {
	e := newEnv(t, "zs-gate-outage")
	ctx := context.Background()
	tn := e.tenant("Outage Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	charges := func() int { return e.fake.Charges(e.brand.Slug, tn.KillBillAccountID, wallet.ChargeDescriptionPrefix) }
	if _, err := e.reserveFor(sc, "a", 100, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reserveFor(sc, "b", 100, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reserveFor(sc, "short", 100, time.Minute); err != nil {
		t.Fatal(err)
	}
	e.fake.SetDown(false, true)
	for _, ref := range []string{"a", "b", "short"} {
		if r, err := e.settle(sc, ref, 0, wallet.Settlement{CreditMinor: 60}); err != nil || !r.SettlementPending || r.Status != wallet.StatusHeld {
			t.Fatalf("settle %s with Kill Bill down = %+v %v", ref, r, err)
		}
	}
	if charges() != 0 || heldSum(t, e, tn) != 300 {
		t.Fatalf("during the outage: %d charges, held %d", charges(), heldSum(t, e, tn))
	}
	// The short lease ends while its settlement waits: it stays held and
	// pending, and the sweeper's attempts while Kill Bill is down charge
	// nothing.
	e.clock.advance(2 * time.Minute)
	if r, err := (&wallet.Sweeper{Service: e.svc, ReconcileEvery: time.Hour}).SweepOnce(ctx); err != nil || r.Expired != 0 || r.Pending != 3 {
		t.Fatalf("sweep = %+v %v", r, err)
	}
	if charges() != 0 || heldSum(t, e, tn) != 300 {
		t.Fatalf("after the lease ended: %d charges, held %d", charges(), heldSum(t, e, tn))
	}
	e.fake.SetDown(false, false)
	if n, err := relayOnce(t, e, ctx); err != nil || n != 3 {
		t.Fatalf("relay = %d %v", n, err)
	}
	if n, err := relayOnce(t, e, ctx); err != nil || n != 0 {
		t.Fatalf("second relay = %d %v", n, err)
	}
	if charges() != 3 {
		t.Fatalf("charges after the outage = %d", charges())
	}
	for _, ref := range []string{"a", "b", "short"} {
		if r, _ := wallet.Get(ctx, e.db.Pool, sc, ref, 0); r.Status != wallet.StatusSettled || r.SettlementPending {
			t.Fatalf("%s = %+v, want settled", ref, r)
		}
	}
	var expired int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM reservation_events WHERE tenant_id = $1 AND kind = 'expire'`, tn.ID).Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired holds = %d %v", expired, err)
	}

	// The database fails right after Kill Bill took the charge: the
	// connection of the settlement dies.
	if _, err := e.reserveFor(sc, "db", 100, time.Hour); err != nil {
		t.Fatal(err)
	}
	tx, err := e.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	e.fake.AfterWrite(func(method, path string) {
		if strings.HasPrefix(path, "/1.0/kb/invoices/charges") {
			_, _ = e.db.Pool.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid)
		}
	})
	if _, err := e.svc.Settle(ctx, tx, sc, e.call, "db", 0, wallet.Settlement{CreditMinor: 70}); err == nil {
		if err := tx.Commit(ctx); err == nil {
			t.Fatal("the settlement committed on a dead connection")
		}
	}
	_ = tx.Rollback(ctx)
	e.fake.AfterWrite(nil)
	if charges() != 4 {
		t.Fatalf("charges after the database failure = %d", charges())
	}
	writes := countWrites(e, "/1.0/kb/invoices/charges")
	r, err := e.settle(sc, "db", 0, wallet.Settlement{CreditMinor: 70})
	if err != nil || r.Status != wallet.StatusSettled || charges() != 4 || countWrites(e, "/1.0/kb/invoices/charges") != writes {
		t.Fatalf("recovery = %+v %v, %d charges", r, err, charges())
	}
}

// Gate 4. Every kind of drift is detected, audited with its amounts and
// object IDs, and Kill Bill is never corrected.
func TestReconciliationDetectsEveryDiscrepancy(t *testing.T) {
	e := newEnv(t, "zs-gate-recon")
	e.meters()
	ctx := context.Background()
	tn := e.tenant("Drift Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	sub := e.metered(tn)
	e.forward(tn, 10)
	if _, _, err := e.reserve(sc, "held", 0, 200); err != nil {
		t.Fatal(err)
	}
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 0 {
		t.Fatalf("clean reconcile = %d %v", found, err)
	}
	// A credit added in Kill Bill behind Core's back.
	e.fake.AddKillBillCredit(e.brand.Slug, tn.KillBillAccountID, 777)
	// Usage in Kill Bill that no Core event explains.
	if err := e.fake.KillBill(t).RecordUsage(ctx, killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "outside"}, killbill.SubscriptionUsageRecord{SubscriptionID: sub, TrackingID: "outside-core",
			UnitUsageRecords: []killbill.UnitUsageRecords{{UnitType: enginetest.UnitAPICall, UsageRecords: []killbill.UsageRecord{{RecordDate: e.clock.Now().Format(time.DateOnly), Amount: "5"}}}}}); err != nil {
		t.Fatal(err)
	}
	// A Core settlement whose Kill Bill charge does not exist.
	ghost := "00000000-0000-0000-0000-00000000beef"
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO reservations (id, brand_id, tenant_id, currency, external_ref, attempt, amount_minor, status,
    lease_expires_at, settled_minor, rated_by, killbill_ref_type, killbill_ref, actor_type, correlation_id, settled_at, created_by_actor)
VALUES ($1, $2, $3, 'CAD', 'ghost', 0, 90, 'settled', now(), 90, 'external_charge', 'external_charge', $4, 'system', 'test', now(), 'service:ghost')`,
		idgen.MustNewV7(), e.brand.ID, tn.ID, ghost); err != nil {
		t.Fatal(err)
	}
	// A held total that does not match the reservation rows' history.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE reservations SET amount_minor = amount_minor + 50 WHERE tenant_id = $1 AND external_ref = 'held'`, tn.ID); err != nil {
		t.Fatal(err)
	}
	// A dry-run invoice is a POST that changes nothing, so it is not
	// counted.
	changes := func() int { return len(e.fake.Writes()) - countWrites(e, "/1.0/kb/invoices/dryRun") }
	writes := changes()
	credit := e.fake.Credit(e.brand.Slug, tn.KillBillAccountID)
	found, err := e.svc.Reconcile(ctx, sc)
	if err != nil || found != 4 {
		t.Fatalf("reconcile = %d %v", found, err)
	}
	if changes() != writes || e.fake.Credit(e.brand.Slug, tn.KillBillAccountID) != credit {
		t.Fatal("the reconciliation changed Kill Bill")
	}
	page, err := audit.Query(ctx, e.db.Pool, sc, audit.Filter{Action: wallet.EventReconciliationMismatch, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]map[string]any{}
	for _, ev := range page.Events {
		kinds[ev.Details["kind"].(string)] = ev.Details
	}
	checks := map[string][]string{
		wallet.MismatchUnknownCredit: {"killbill_ref", "killbill_amount_minor"},
		wallet.MismatchUnknownUsage:  {"killbill_subscription_id", "killbill_quantity_micro", "core_quantity_micro"},
		wallet.MismatchMissingCharge: {"killbill_ref", "core_amount_minor"},
		wallet.MismatchHeldMismatch:  {"reservation_id", "reserved_minor", "history_minor"},
	}
	for kind, keys := range checks {
		d, ok := kinds[kind]
		if !ok {
			t.Fatalf("no %s audit event in %v", kind, kinds)
		}
		for _, k := range keys {
			if _, ok := d[k]; !ok {
				t.Fatalf("%s audit details lack %s: %v", kind, k, d)
			}
		}
	}
	if d := kinds[wallet.MismatchUnknownUsage]; d["killbill_quantity_micro"] != float64(15_000_000) || d["core_quantity_micro"] != float64(10_000_000) {
		t.Fatalf("usage amounts = %v", d)
	}
	if d := kinds[wallet.MismatchMissingCharge]; d["killbill_ref"] != ghost {
		t.Fatalf("missing charge = %v", d)
	}
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 0 {
		t.Fatalf("second reconcile = %d %v", found, err)
	}
}

// Gate 5. The forwarder, the lease sweeper and the settlement relay are
// cancelled in the middle of a batch and started again: no event reaches
// Kill Bill twice, no hold is settled or expired twice.
func TestCancelledWorkersResumeWithoutDuplicates(t *testing.T) {
	e := newEnv(t, "zs-gate-cancel")
	e.meters()
	ctx := context.Background()
	var tenants []tenant.Tenant
	for i := 0; i < 3; i++ {
		tn := e.tenant(fmt.Sprintf("Cancel %d", i))
		e.credit(tn, 5000)
		e.metered(tn)
		tenants = append(tenants, tn)
		for j := 0; j < 3; j++ {
			if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
				_, _, err := e.svc.Metering.RecordUsage(ctx, tx, tn.Scope(), metering.Call{Actor: e.call.Actor, CorrelationID: "c"},
					metering.Event{Meter: "api-calls", EventID: fmt.Sprintf("e-%d", j), QuantityMicro: 1_000_000})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The forwarder is cancelled right after its first usage record.
	cctx, cancel := context.WithCancel(ctx)
	e.fake.AfterWrite(func(method, path string) {
		if strings.HasPrefix(path, "/1.0/kb/usages") {
			cancel()
		}
	})
	_, _ = e.svc.Metering.Forward(cctx)
	e.fake.AfterWrite(nil)
	if n := len(e.fake.UsageRecords(e.brand.Slug)); n != 1 {
		t.Fatalf("records before the restart = %d", n)
	}
	// Restarted after its claims end.
	e.clock.advance(5 * time.Minute)
	if r, err := e.svc.Metering.Forward(ctx); err != nil || r.Failed != 0 {
		t.Fatalf("forward after the restart = %+v %v", r, err)
	}
	recs := e.fake.UsageRecords(e.brand.Slug)
	seen := map[string]bool{}
	var total int64
	for _, r := range recs {
		if seen[r.SubscriptionID+r.TrackingID] {
			t.Fatalf("tracking ID %s recorded twice", r.TrackingID)
		}
		seen[r.SubscriptionID+r.TrackingID] = true
		total += r.AmountMicro
	}
	if len(recs) != 3 || total != 9_000_000 {
		t.Fatalf("records after the restart = %d, %d micro", len(recs), total)
	}

	// The lease sweeper is cancelled at random points and started again.
	sc := tenants[0].Scope()
	for i := 0; i < 12; i++ {
		if _, err := e.reserveFor(sc, fmt.Sprintf("lease-%d", i), 10, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	e.clock.advance(time.Minute)
	sweeper := &wallet.Sweeper{Service: e.svc, Batch: 2, ReconcileEvery: time.Hour}
	for i := 0; i < 6; i++ {
		short, stop := context.WithTimeout(ctx, time.Duration(i)*time.Millisecond)
		_, _ = sweeper.SweepOnce(short)
		stop()
	}
	if _, err := sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var expired, events int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE tenant_id = $1 AND external_ref LIKE 'lease-%' AND status = 'expired'`, tenants[0].ID).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM reservation_events ev JOIN reservations r ON r.id = ev.reservation_id
WHERE r.tenant_id = $1 AND r.external_ref LIKE 'lease-%' AND ev.kind = 'expire'`, tenants[0].ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if expired != 12 || events != 12 {
		t.Fatalf("expired %d, expire events %d", expired, events)
	}

	// The settlement relay is cancelled right after its first Kill Bill
	// charge, then started again.
	sc = tenants[1].Scope()
	for i := 0; i < 3; i++ {
		if _, err := e.reserveFor(sc, fmt.Sprintf("job-%d", i), 100, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	e.fake.SetDown(false, true)
	for i := 0; i < 3; i++ {
		if r, err := e.settle(sc, fmt.Sprintf("job-%d", i), 0, wallet.Settlement{CreditMinor: 50}); err != nil || !r.SettlementPending {
			t.Fatalf("queue = %+v %v", r, err)
		}
	}
	e.fake.SetDown(false, false)
	rctx, rcancel := context.WithCancel(ctx)
	e.fake.AfterWrite(func(method, path string) {
		if strings.HasPrefix(path, "/1.0/kb/invoices/charges") {
			rcancel()
		}
	})
	_, _ = relayOnce(t, e, rctx)
	e.fake.AfterWrite(nil)
	// The cancelled relay's claim on its settlement ends, as it would after
	// two minutes.
	e.clock.advance(3 * time.Minute)
	if n, err := relayOnce(t, e, ctx); err != nil || n == 0 {
		t.Fatalf("relay after the restart = %d %v", n, err)
	}
	if got := e.fake.Charges(e.brand.Slug, tenants[1].KillBillAccountID, wallet.ChargeDescriptionPrefix); got != 3 {
		t.Fatalf("charges = %d", got)
	}
	for i := 0; i < 3; i++ {
		if r, _ := wallet.Get(ctx, e.db.Pool, sc, fmt.Sprintf("job-%d", i), 0); r.Status != wallet.StatusSettled {
			t.Fatalf("job-%d = %+v", i, r)
		}
	}
	var settles int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM reservation_events WHERE tenant_id = $1 AND kind = 'settle'`, tenants[1].ID).Scan(&settles); err != nil || settles != 3 {
		t.Fatalf("settle events = %d %v", settles, err)
	}
}
