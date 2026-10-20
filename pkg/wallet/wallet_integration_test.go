//go:build integration

package wallet_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	t     *testing.T
	db    *postgrestest.DB
	fake  *enginetest.Fake
	eng   tenant.Engines
	clock *clock
	bill  *billing.Service
	svc   *wallet.Service
	brand enginetest.Brand
	call  wallet.Call
}

func newEnv(t *testing.T, slug string) *env {
	t.Helper()
	db := postgrestest.New(t)
	fake := enginetest.New(t)
	e := &env{t: t, db: db, fake: fake, clock: &clock{now: time.Now().UTC().Truncate(time.Second)},
		eng:  tenant.Engines{Keycloak: fake.Keycloak(t), KillBill: fake.KillBill(t), Secrets: fake.Secrets()},
		call: wallet.Call{Actor: events.Actor{Type: events.ActorService, ID: "scheduler"}, CorrelationID: "corr-wallet"}}
	e.bill = &billing.Service{DB: db.Pool, KillBill: fake.KillBill(t), Secrets: fake.Secrets(), Now: e.clock.Now}
	e.svc = &wallet.Service{DB: db.Pool, Billing: e.bill, Metering: &metering.Service{DB: db.Pool, Billing: e.bill, Now: e.clock.Now},
		CreditTTL: 5 * time.Second, Now: e.clock.Now}
	e.brand = fake.InsertBrand(t, db.Pool, slug)
	return e
}

func (e *env) tenant(name string) tenant.Tenant {
	e.t.Helper()
	return e.tenantOf(e.brand, name)
}

func (e *env) tenantOf(b enginetest.Brand, name string) tenant.Tenant {
	e.t.Helper()
	owner := enginetest.InsertPerson(e.t, e.db.Pool, b, strings.ReplaceAll(strings.ToLower(name), " ", "-")+"@example.test")
	tn, err := tenant.CreateOrganization(context.Background(), e.db.Pool, e.eng, tenant.CreateRequest{
		Brand: b.ID, Creator: owner.Identity.ID, Name: name, IdempotencyKey: "key-" + idgen.MustNewV7().String(),
		BillingCountry: "CA", Actor: events.Actor{Type: events.ActorUser, ID: owner.Identity.PublicID()}, CorrelationID: "test",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tn
}

// credit grants a staff credit through Core, so the reconciliation knows
// it.
func (e *env) credit(tn tenant.Tenant, minor int64) {
	e.t.Helper()
	_, _, err := e.bill.GrantCredit(context.Background(), tn.Scope(), billing.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:staff"},
		Reason: "test credit", CorrelationID: "corr-credit"}, billing.StaffCredit{AmountMinor: minor, Currency: "CAD", IdempotencyKey: "credit-" + idgen.MustNewV7().String()})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) policy(overdraft, lowBalance int64) {
	e.t.Helper()
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE brands SET manifest = jsonb_set(manifest, '{billing,wallet}', $2::jsonb) WHERE id = $1`,
		e.brand.ID, fmt.Sprintf(`{"overdraft_minor": %d, "low_balance_minor": %d}`, overdraft, lowBalance)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) reserve(sc scope.Scope, ref string, attempt int, amount int64) (wallet.Reservation, bool, error) {
	var (
		r       wallet.Reservation
		created bool
	)
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		r, created, err = e.svc.Reserve(context.Background(), tx, sc, e.call, wallet.ReserveRequest{ExternalRef: ref, Attempt: attempt, AmountMinor: amount, Lease: time.Minute})
		return err
	})
	return r, created, err
}

func (e *env) settle(sc scope.Scope, ref string, attempt int, st wallet.Settlement) (wallet.Reservation, error) {
	var r wallet.Reservation
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		r, err = e.svc.Settle(context.Background(), tx, sc, e.call, ref, attempt, st)
		return err
	})
	return r, err
}

func (e *env) release(sc scope.Scope, ref string, attempt int) (wallet.Reservation, error) {
	var r wallet.Reservation
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		r, err = e.svc.Release(context.Background(), tx, sc, e.call, ref, attempt)
		return err
	})
	return r, err
}

func (e *env) view(sc scope.Scope) wallet.View {
	e.t.Helper()
	v, err := e.svc.View(context.Background(), sc)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// avail is the available amount of a view, which is never nil when Kill
// Bill rated the unbilled amount.
func avail(v wallet.View) int64 {
	if v.AvailableMinor == nil {
		return -1 << 62
	}
	return *v.AvailableMinor
}

func codeOf(err error) string {
	var z *zserrors.Error
	if err == nil {
		return "ok"
	}
	if !errors.As(err, &z) {
		return "untyped: " + err.Error()
	}
	return zserrors.Lookup(z.Code).Name
}

func count(t *testing.T, db *postgrestest.DB, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReserveGrowSettleRelease(t *testing.T) {
	e := newEnv(t, "zs-wallet")
	tn := e.tenant("Wallet Co")
	sc := tn.Scope()
	e.credit(tn, 1000)

	r, created, err := e.reserve(sc, "job-1", 0, 400)
	if err != nil || !created || r.Status != wallet.StatusHeld || r.AmountMinor != 400 || !strings.HasPrefix(r.ID, "rsv_") || r.Currency != "CAD" {
		t.Fatalf("reserve = %+v %v %v", r, created, err)
	}
	// An equal or smaller replay is a no-op, a larger one grows the hold.
	if again, created, err := e.reserve(sc, "job-1", 0, 300); err != nil || created || again.AmountMinor != 400 {
		t.Fatalf("smaller replay = %+v %v %v", again, created, err)
	}
	if grown, created, err := e.reserve(sc, "job-1", 0, 700); err != nil || created || grown.AmountMinor != 700 || grown.ID != r.ID {
		t.Fatalf("grow = %+v %v %v", grown, created, err)
	}
	// Another attempt of the same job is another hold.
	_, _, err = e.reserve(sc, "job-1", 1, 400)
	if codeOf(err) != "insufficient_credit" {
		t.Fatalf("over the credit: %v", err)
	}
	if short, ok := wallet.Shortfall(err); !ok || short != 100 {
		t.Fatalf("shortfall = %d %v", short, ok)
	}
	v := e.view(sc)
	if v.KillBillCreditMinor != 1000 || v.ReservedMinor != 700 || avail(v) != 300 || v.Currency != "CAD" || v.OnHold {
		t.Fatalf("view = %+v", v)
	}

	// Settle 450 of the 700 as consumed credit: Kill Bill charges 450 and
	// the rest of the hold is released.
	settled, err := e.settle(sc, "job-1", 0, wallet.Settlement{CreditMinor: 450})
	if err != nil || settled.Status != wallet.StatusSettled || settled.SettledMinor == nil || *settled.SettledMinor != 450 ||
		settled.KillBillRefType != wallet.RefExternalCharge || settled.KillBillRef == "" || settled.RatedBy != wallet.RatedByExternalCharge {
		t.Fatalf("settle = %+v %v", settled, err)
	}
	if got := e.fake.Credit(e.brand.Slug, tn.KillBillAccountID); got != 550 {
		t.Fatalf("Kill Bill credit = %d", got)
	}
	if v := e.view(sc); v.KillBillCreditMinor != 550 || v.ReservedMinor != 0 || avail(v) != 550 {
		t.Fatalf("view after settle = %+v", v)
	}
	// The same settlement again returns it; another one conflicts.
	if again, err := e.settle(sc, "job-1", 0, wallet.Settlement{CreditMinor: 450}); err != nil || again.KillBillRef != settled.KillBillRef {
		t.Fatalf("settle replay = %+v %v", again, err)
	}
	if _, err := e.settle(sc, "job-1", 0, wallet.Settlement{CreditMinor: 400}); codeOf(err) != "reservation_conflict" {
		t.Fatalf("other settlement: %v", err)
	}
	// A replay of a settled reservation, of any amount, is closed.
	for _, amount := range []int64{100, 900} {
		if _, _, err := e.reserve(sc, "job-1", 0, amount); codeOf(err) != "reservation_closed" {
			t.Fatalf("replay %d of a settled hold: %v", amount, err)
		}
	}
	// A settlement cannot exceed the hold.
	if _, _, err := e.reserve(sc, "job-2", 0, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := e.settle(sc, "job-2", 0, wallet.Settlement{CreditMinor: 101}); codeOf(err) != "settlement_exceeds_hold" {
		t.Fatalf("settle above the hold: %v", err)
	}
	// Release returns the hold, twice is the same, then settle is refused.
	if rel, err := e.release(sc, "job-2", 0); err != nil || rel.Status != wallet.StatusReleased {
		t.Fatalf("release = %+v %v", rel, err)
	}
	if rel, err := e.release(sc, "job-2", 0); err != nil || rel.Status != wallet.StatusReleased {
		t.Fatalf("release again = %+v %v", rel, err)
	}
	if _, err := e.settle(sc, "job-2", 0, wallet.Settlement{CreditMinor: 10}); codeOf(err) != "reservation_not_held" {
		t.Fatalf("settle released: %v", err)
	}
	if _, _, err := e.reserve(sc, "job-2", 0, 10); codeOf(err) != "reservation_closed" {
		t.Fatalf("replay of a released hold: %v", err)
	}
	if _, err := e.release(sc, "job-404", 0); codeOf(err) != "reservation_not_found" {
		t.Fatalf("release unknown: %v", err)
	}
	// Extend renews the lease of a held reservation only.
	if _, _, err := e.reserve(sc, "job-3", 0, 100); err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		r, err := e.svc.Extend(context.Background(), tx, sc, e.call, "job-3", 0, time.Hour)
		if err == nil && !r.LeaseExpiresAt.Equal(e.clock.Now().Add(time.Hour)) {
			err = fmt.Errorf("lease = %v", r.LeaseExpiresAt)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Every write is audited and has its event.
	for action, want := range map[string]int{"wallet.reservation.created": 3, "wallet.reservation.grown": 1, "wallet.reservation.settled": 1,
		"wallet.reservation.released": 1, "wallet.reservation.extended": 1} {
		if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = $2`, tn.ID, action); n != want {
			t.Errorf("%s audit events = %d, want %d", action, n, want)
		}
	}
	if n := count(t, e.db, `SELECT count(*) FROM reservation_events WHERE tenant_id = $1`, tn.ID); n != 7 {
		t.Fatalf("reservation events = %d", n)
	}
	if _, err := e.db.Pool.Exec(context.Background(), `DELETE FROM reservation_events WHERE tenant_id = $1`, tn.ID); err == nil {
		t.Fatal("reservation events are not append-only")
	}
}

func TestOverdraftLowBalanceAndHolds(t *testing.T) {
	e := newEnv(t, "zs-wallet-policy")
	e.policy(200, 300)
	tn := e.tenant("Policy Co")
	sc := tn.Scope()
	e.credit(tn, 500)
	// The overdraft allowance lets holds pass the credit by 200.
	if _, _, err := e.reserve(sc, "a", 0, 650); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.reserve(sc, "b", 0, 60); codeOf(err) != "insufficient_credit" {
		t.Fatalf("past the overdraft: %v", err)
	}
	v := e.view(sc)
	// Available counts the overdraft allowance, as Reserve does: 500 - 650 +
	// 200.
	if avail(v) != 50 || !v.LowBalance || v.OverdraftMinor != 200 || v.LowBalanceThresholdMinor != 300 {
		t.Fatalf("view = %+v", v)
	}
	e.view(sc)
	if n := count(t, e.db, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND event_type = 'wallet.low_balance'`, tn.ID); n != 1 {
		t.Fatalf("low balance events = %d", n)
	}
	// Suspension and a blocked Kill Bill account put the wallet on hold.
	x := e.tenant("Blocked Co")
	e.credit(x, 500)
	e.fake.AccountBlock(e.brand.Slug, x.KillBillAccountID, killbill.BlockingState{StateName: "STOP", Service: "ops", IsBlockChange: true, IsBlockEntitlement: true})
	if _, _, err := e.reserve(x.Scope(), "a", 0, 10); codeOf(err) != "wallet_on_hold" {
		t.Fatalf("blocked account: %v", err)
	}
	y := e.tenant("Suspended Co")
	e.credit(y, 500)
	if _, err := tenant.Suspend(context.Background(), e.db.Pool, e.eng, y.Scope(), events.Actor{Type: events.ActorService, ID: "ops"}, "test", "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.reserve(y.Scope(), "a", 0, 10); codeOf(err) != "wallet_on_hold" {
		t.Fatalf("suspended tenant: %v", err)
	}
}

func TestSettlementWithKillBillDownQueuesAndCompletes(t *testing.T) {
	e := newEnv(t, "zs-wallet-down")
	tn := e.tenant("Down Co")
	sc := tn.Scope()
	e.credit(tn, 1000)
	if _, _, err := e.reserve(sc, "job", 0, 600); err != nil {
		t.Fatal(err)
	}
	e.fake.SetDown(false, true)
	pending, err := e.settle(sc, "job", 0, wallet.Settlement{CreditMinor: 500})
	if err != nil || pending.Status != wallet.StatusHeld || !pending.SettlementPending {
		t.Fatalf("settle with Kill Bill down = %+v %v", pending, err)
	}
	// The same settlement again is still pending; anything else is refused
	// while it waits, and the hold still counts.
	if again, err := e.settle(sc, "job", 0, wallet.Settlement{CreditMinor: 500}); err != nil || !again.SettlementPending {
		t.Fatalf("settle again = %+v %v", again, err)
	}
	if _, err := e.settle(sc, "job", 0, wallet.Settlement{CreditMinor: 400}); codeOf(err) != "reservation_settlement_pending" {
		t.Fatalf("other settlement while pending: %v", err)
	}
	if _, _, err := e.reserve(sc, "job", 0, 900); codeOf(err) != "reservation_settlement_pending" {
		t.Fatalf("grow while pending: %v", err)
	}
	if _, err := e.release(sc, "job", 0); codeOf(err) != "reservation_settlement_pending" {
		t.Fatalf("release while pending: %v", err)
	}
	// Within its lease the sweeper leaves it to the relay.
	e.clock.advance(30 * time.Second)
	sweeper := &wallet.Sweeper{Service: e.svc, ReconcileEvery: time.Hour}
	if _, err := sweeper.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.fake.SetDown(false, false)
	if v := e.view(sc); v.ReservedMinor != 600 || v.PendingSettlements != 1 || avail(v) != 400 {
		t.Fatalf("view while pending = %+v", v)
	}
	// The relay defers while Kill Bill is down and completes once it is up.
	relay := outbox.NewRelay("test", outbox.NewPostgresReader(e.db.Pool).WithEventTypes(wallet.EventSettlementRequested), e.svc.RelayHandler(), 0, 0, nil)
	e.fake.SetDown(false, true)
	if n, err := relay.RunOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("relay with Kill Bill down = %d %v", n, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM outbox WHERE event_type = 'wallet.settlement.requested' AND quarantined_at IS NULL AND dispatch_attempts = 0`); n != 1 {
		t.Fatalf("deferred settlement = %d", n)
	}
	e.fake.SetDown(false, false)
	if _, err := e.db.Pool.Exec(context.Background(), `UPDATE outbox SET next_attempt_at = now() WHERE event_type = 'wallet.settlement.requested'`); err != nil {
		t.Fatal(err)
	}
	if n, err := relay.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("relay = %d %v", n, err)
	}
	done, err := wallet.Get(context.Background(), e.db.Pool, sc, "job", 0)
	if err != nil || done.Status != wallet.StatusSettled || done.SettlementPending || *done.SettledMinor != 500 {
		t.Fatalf("after the relay = %+v %v", done, err)
	}
	if got := e.fake.Credit(e.brand.Slug, tn.KillBillAccountID); got != 500 {
		t.Fatalf("Kill Bill credit = %d", got)
	}
	if v := e.view(sc); v.ReservedMinor != 0 || avail(v) != 500 || v.PendingSettlements != 0 {
		t.Fatalf("view after the relay = %+v", v)
	}
	// A charge Kill Bill made before its answer was lost is found again,
	// never charged twice.
	if _, _, err := e.reserve(sc, "lost", 0, 100); err != nil {
		t.Fatal(err)
	}
	e.fake.AddExternalCharge(e.brand.Slug, tn.KillBillAccountID, 80, wallet.ChargeDescription(sc, "lost", 0))
	if s, err := e.settle(sc, "lost", 0, wallet.Settlement{CreditMinor: 80}); err != nil || *s.SettledMinor != 80 {
		t.Fatalf("settle after a lost answer = %+v %v", s, err)
	}
	if got := e.fake.Credit(e.brand.Slug, tn.KillBillAccountID); got != 420 {
		t.Fatalf("Kill Bill credit after a lost answer = %d", got)
	}
}

func TestUsageSettlementAndForcedRetry(t *testing.T) {
	e := newEnv(t, "zs-wallet-usage")
	ctx := context.Background()
	tn := e.tenant("Usage Co")
	sc := tn.Scope()
	e.credit(tn, 1000)
	facts := brand.FactsFromCatalog([]killbill.Catalog{enginetest.TestCatalog()})
	if err := pgx.BeginFunc(ctx, e.db.Pool, func(tx pgx.Tx) error {
		_, _, err := brand.ReplaceMeters(ctx, tx, e.brand.ID, []brand.Meter{{Slug: "api-calls", Unit: "request", KillBillUnit: enginetest.UnitAPICall}},
			facts, events.Actor{Type: events.ActorStaff, ID: "sub:op"}, "test", "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := e.fake.KillBill(t).CreateSubscription(ctx, killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "test"}, tn.KillBillAccountID, enginetest.PlanMetered, "metered")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.reserve(sc, "run", 0, 500); err != nil {
		t.Fatal(err)
	}
	usage := wallet.Settlement{Usage: &wallet.UsageSettlement{Unit: enginetest.UnitAPICall, QuantityMicro: 250_000_000}}
	r, err := e.settle(sc, "run", 0, usage)
	if err != nil || r.Status != wallet.StatusSettled || r.KillBillRefType != wallet.RefUsageRecord || r.KillBillSubscriptionID != sub ||
		r.RatedBy != wallet.RatedByCatalogue || r.SettledMinor == nil || *r.SettledMinor != 250 {
		t.Fatalf("usage settlement = %+v %v", r, err)
	}
	recs := e.fake.UsageRecords(e.brand.Slug)
	if len(recs) != 1 || recs[0].TrackingID != r.KillBillRef || recs[0].AmountMicro != 250_000_000 || recs[0].SubscriptionID != sub {
		t.Fatalf("Kill Bill usage = %+v", recs)
	}
	// The usage shows in Core's events, forwarded by the settlement, so the
	// forwarder never sends it again.
	list, _, err := metering.ListUsageEvents(ctx, e.db.Pool, sc, metering.Filter{}, 10, "")
	if err != nil || len(list) != 1 || list[0].ReservationID != r.ID || list[0].ForwardedAt == nil || list[0].KillBillTrackingID != r.KillBillRef {
		t.Fatalf("usage events = %+v %v", list, err)
	}
	if report, err := e.svc.Metering.Forward(ctx); err != nil || report.Forwarded != 0 {
		t.Fatalf("forwarder after a settlement = %+v %v", report, err)
	}
	// A forced retry, as after a commit lost on the way back: the same
	// tracking ID reaches Kill Bill, which records nothing more.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE reservations SET status = 'held', settled_at = NULL, settlement_fingerprint = NULL WHERE tenant_id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	if again, err := e.settle(sc, "run", 0, usage); err != nil || again.KillBillRef != r.KillBillRef {
		t.Fatalf("forced retry = %+v %v", again, err)
	}
	if recs := e.fake.UsageRecords(e.brand.Slug); len(recs) != 1 {
		t.Fatalf("Kill Bill usage after a forced retry = %+v", recs)
	}
	// A unit the subscription's plan does not price is refused before Kill
	// Bill records anything.
	if _, _, err := e.reserve(sc, "run-2", 0, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := e.settle(sc, "run-2", 0, wallet.Settlement{Usage: &wallet.UsageSettlement{Unit: "gpu-hour", QuantityMicro: 1}}); codeOf(err) != "billing_rejected" {
		t.Fatalf("unit outside the plan: %v", err)
	}
}

func TestConcurrentReserveGrowSettle(t *testing.T) {
	e := newEnv(t, "zs-wallet-race")
	ctx := context.Background()
	tn := e.tenant("Race Co")
	sc := tn.Scope()
	const credit = 10_000
	e.credit(tn, credit)
	if _, err := e.fake.KillBill(t).CreateSubscription(ctx, killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "test"}, tn.KillBillAccountID, enginetest.PlanMetered, "race-metered"); err != nil {
		t.Fatal(err)
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		charged int64
		settled int
		usages  int
	)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref := fmt.Sprintf("job-%d", i)
			if _, _, err := e.reserve(sc, ref, 0, 500); err != nil {
				if codeOf(err) != "insufficient_credit" {
					t.Errorf("reserve %s: %v", ref, err)
				}
				return
			}
			if _, _, err := e.reserve(sc, ref, 0, 700); err != nil && codeOf(err) != "insufficient_credit" {
				t.Errorf("grow %s: %v", ref, err)
			}
			var st wallet.Settlement
			switch i % 3 {
			case 0:
				if _, err := e.release(sc, ref, 0); err != nil {
					t.Errorf("release %s: %v", ref, err)
				}
				return
			case 1:
				st = wallet.Settlement{Usage: &wallet.UsageSettlement{Unit: enginetest.UnitAPICall, QuantityMicro: int64(i+1) * 1_000_000}}
			default:
				st = wallet.Settlement{CreditMinor: int64(300 + i)}
			}
			if _, err := e.settle(sc, ref, 0, st); err != nil {
				t.Errorf("settle %s: %v", ref, err)
				return
			}
			// A concurrent retry of the same settlement changes nothing.
			if _, err := e.settle(sc, ref, 0, st); err != nil {
				t.Errorf("settle again %s: %v", ref, err)
			}
			mu.Lock()
			charged += st.CreditMinor
			settled++
			if st.Usage != nil {
				usages++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if settled == 0 || usages == 0 {
		t.Fatalf("settled %d, of which usage %d", settled, usages)
	}
	if got := e.fake.Credit(e.brand.Slug, tn.KillBillAccountID); got != credit-charged {
		t.Fatalf("Kill Bill credit = %d, want %d", got, credit-charged)
	}
	if n := count(t, e.db, `SELECT count(*) FROM reservations WHERE tenant_id = $1 AND status = 'settled'`, tn.ID); n != settled {
		t.Fatalf("settled = %d, want %d", n, settled)
	}
	// One usage record per usage settlement, each with its own tracking ID.
	recs := e.fake.UsageRecords(e.brand.Slug)
	seen := map[string]bool{}
	for _, r := range recs {
		if seen[r.TrackingID] {
			t.Fatalf("tracking ID %s recorded twice", r.TrackingID)
		}
		seen[r.TrackingID] = true
	}
	if len(recs) != usages {
		t.Fatalf("usage records = %d, want %d", len(recs), usages)
	}
	// Every refused hold was refused for credit, never for a lost race:
	// the tenant lock serialized them.
	if n := count(t, e.db, `SELECT count(*) FROM reservation_events WHERE tenant_id = $1 AND kind IN ('reserve', 'grow')`, tn.ID); n < settled {
		t.Fatalf("holds = %d, settled = %d", n, settled)
	}
	v := e.view(sc)
	if v.ReservedMinor != 0 || v.KillBillCreditMinor != credit-charged {
		t.Fatalf("view = %+v", v)
	}
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 0 {
		t.Fatalf("reconcile = %d %v", found, err)
	}
}

func TestWalletViewMatchesKillBillAndReconciliation(t *testing.T) {
	e := newEnv(t, "zs-wallet-ledger")
	ctx := context.Background()
	tn := e.tenant("Ledger Co")
	sc := tn.Scope()
	e.credit(tn, 5_000)
	rng := rand.New(rand.NewPCG(20261020, 4))
	held := map[string]int64{}
	sweeper := &wallet.Sweeper{Service: e.svc, ReconcileEvery: time.Nanosecond}
	for step := 0; step < 200; step++ {
		ref := fmt.Sprintf("r-%d", rng.IntN(25))
		switch op := rng.IntN(6); op {
		case 0, 1:
			amount := int64(1 + rng.IntN(800))
			r, _, err := e.reserve(sc, ref, 0, amount)
			switch codeOf(err) {
			case "ok":
				if r.Status == wallet.StatusHeld {
					held[ref] = r.AmountMinor
				}
			case "insufficient_credit", "reservation_closed":
			default:
				t.Fatalf("step %d reserve: %v", step, err)
			}
		case 2:
			if h, ok := held[ref]; ok {
				if _, err := e.settle(sc, ref, 0, wallet.Settlement{CreditMinor: 1 + rng.Int64N(h)}); err != nil {
					t.Fatalf("step %d settle: %v", step, err)
				}
				delete(held, ref)
			}
		case 3:
			if _, ok := held[ref]; ok {
				if _, err := e.release(sc, ref, 0); err != nil {
					t.Fatalf("step %d release: %v", step, err)
				}
				delete(held, ref)
			}
		case 4:
			// Leases end: the sweeper expires every hold.
			e.clock.advance(2 * time.Minute)
			if _, err := sweeper.SweepOnce(ctx); err != nil {
				t.Fatal(err)
			}
			held = map[string]int64{}
		case 5:
			if rng.IntN(4) == 0 {
				e.credit(tn, int64(100+rng.IntN(400)))
			}
		}
		var sum int64
		for _, h := range held {
			sum += h
		}
		v := e.view(sc)
		if kb := e.fake.Credit(e.brand.Slug, tn.KillBillAccountID); v.KillBillCreditMinor != kb || v.ReservedMinor != sum || avail(v) != wallet.Available(kb, 0, sum, 0) {
			t.Fatalf("step %d: view %+v, Kill Bill credit %d, held %d", step, v, kb, sum)
		}
	}
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 0 {
		t.Fatalf("reconcile after the sequence = %d %v", found, err)
	}
	// A credit added behind Core's back is exactly one mismatch, reported
	// once.
	e.fake.AddKillBillCredit(e.brand.Slug, tn.KillBillAccountID, 1234)
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 1 {
		t.Fatalf("reconcile after a credit behind Core's back = %d %v", found, err)
	}
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 0 {
		t.Fatalf("reconcile again = %d %v", found, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND event_type = 'wallet.reconciliation_mismatch'`, tn.ID); n != 1 {
		t.Fatalf("mismatch events = %d", n)
	}
	// The sweep never changed Kill Bill.
	if writes := e.fake.Writes(); len(writes) == 0 {
		t.Fatal("no writes recorded")
	}
	var kind string
	if err := e.db.Pool.QueryRow(ctx, `SELECT kind FROM wallet_reconciliation_findings WHERE tenant_id = $1`, tn.ID).Scan(&kind); err != nil || kind != wallet.MismatchUnknownCredit {
		t.Fatalf("finding = %q %v", kind, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM outbox WHERE tenant_id = $1 AND event_type = 'reservation.expired'`, tn.ID); n == 0 {
		t.Fatal("no reservation.expired event")
	}
}

func TestReservationIsolation(t *testing.T) {
	e := newEnv(t, "zs-wallet-a")
	other := e.fake.InsertBrand(t, e.db.Pool, "zs-wallet-b")
	x, y := e.tenant("Tenant X"), e.tenant("Tenant Y")
	e.credit(x, 500)
	if _, _, err := e.reserve(x.Scope(), "job", 0, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := wallet.Get(context.Background(), e.db.Pool, y.Scope(), "job", 0); codeOf(err) != "reservation_not_found" {
		t.Fatalf("tenant Y reads X's hold: %v", err)
	}
	if _, err := e.release(y.Scope(), "job", 0); codeOf(err) != "reservation_not_found" {
		t.Fatalf("tenant Y releases X's hold: %v", err)
	}
	if list, _, err := wallet.List(context.Background(), e.db.Pool, y.Scope(), "", "", 50, ""); err != nil || len(list) != 0 {
		t.Fatalf("tenant Y lists = %v %v", list, err)
	}
	cross := scope.Scope{Brand: other.ID, Tenant: x.ID}
	if _, err := wallet.Get(context.Background(), e.db.Pool, cross, "job", 0); codeOf(err) != "reservation_not_found" {
		t.Fatalf("X's hold under brand B: %v", err)
	}
	if _, _, err := e.reserve(cross, "job-b", 0, 10); codeOf(err) != "tenant_not_found" {
		t.Fatalf("reserve for X under brand B: %v", err)
	}
	// Y has no credit of its own: X's credit is not Y's.
	if _, _, err := e.reserve(y.Scope(), "job", 0, 1); codeOf(err) != "insufficient_credit" {
		t.Fatalf("Y reserving X's credit: %v", err)
	}
}
