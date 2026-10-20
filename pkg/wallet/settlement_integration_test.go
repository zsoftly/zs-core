//go:build integration

package wallet_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/wallet"
)

// settleNow settles in steps, as the HTTP route does.
func (e *env) settleNow(sc scope.Scope, call wallet.Call, ref string, st wallet.Settlement) (wallet.Reservation, error) {
	return e.svc.SettleNow(context.Background(), sc, call, ref, 0, st)
}

func (e *env) reserveAs(sc scope.Scope, call wallet.Call, ref string, amount int64, lease time.Duration) (wallet.Reservation, error) {
	var r wallet.Reservation
	err := pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
		var err error
		r, _, err = e.svc.Reserve(context.Background(), tx, sc, call, wallet.ReserveRequest{ExternalRef: ref, AmountMinor: amount, Lease: lease})
		return err
	})
	return r, err
}

func calls(n int64) wallet.Settlement {
	return wallet.Settlement{Usage: &wallet.UsageSettlement{Unit: enginetest.UnitAPICall, QuantityMicro: n * 1_000_000}}
}

// Blocker 1. A settlement is checked against its hold under the tenant
// lock: usage at the catalogue price of its first tier, or credit_minor.
// Nothing above the hold reaches Kill Bill, and a unit without a price
// cannot settle a reservation.
func TestSettlementBoundedByHold(t *testing.T) {
	e := newEnv(t, "zs-bounded")
	e.meters()
	tn := e.tenant("Bounded Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	e.metered(tn)
	if _, _, err := e.reserve(sc, "tiny", 0, 1); err != nil {
		t.Fatal(err)
	}
	huge := wallet.Settlement{Usage: &wallet.UsageSettlement{Unit: enginetest.UnitAPICall, QuantityMicro: metering.MaxQuantityMicro}}
	for name, settle := range map[string]func() error{
		"library": func() error { _, err := e.settle(sc, "tiny", 0, huge); return err },
		"steps":   func() error { _, err := e.settleNow(sc, e.call, "tiny", huge); return err },
	} {
		if err := settle(); codeOf(err) != "settlement_exceeds_hold" {
			t.Fatalf("%s: usage of 1e15 on a hold of 1: %v", name, err)
		}
	}
	if recs := e.fake.UsageRecords(e.brand.Slug); len(recs) != 0 {
		t.Fatalf("Kill Bill took usage beyond the hold: %+v", recs)
	}
	if r, _ := wallet.Get(context.Background(), e.db.Pool, sc, "tiny", 0); r.Status != wallet.StatusHeld || r.SettlementPending {
		t.Fatalf("refused settlement left %+v", r)
	}
	// The metered plan prices api-call at one cent: 250 calls are 250.
	if _, _, err := e.reserve(sc, "exact", 0, 250); err != nil {
		t.Fatal(err)
	}
	if r, err := e.settle(sc, "exact", 0, calls(250)); err != nil || r.Status != wallet.StatusSettled || *r.SettledMinor != 250 {
		t.Fatalf("settle at the hold = %+v %v", r, err)
	}
	if _, _, err := e.reserve(sc, "above", 0, 250); err != nil {
		t.Fatal(err)
	}
	if _, err := e.settleNow(sc, e.call, "above", calls(251)); codeOf(err) != "settlement_exceeds_hold" {
		t.Fatalf("one unit above the hold: %v", err)
	}
	if _, err := e.settleNow(sc, e.call, "above", wallet.Settlement{CreditMinor: 251}); codeOf(err) != "settlement_exceeds_hold" {
		t.Fatalf("a charge above the hold: %v", err)
	}
	// A plan in its trial phase prices no usage: the unit is unrated.
	trial := e.tenant("Trial Co")
	e.credit(trial, 500)
	if _, err := e.fake.KillBill(t).CreateSubscription(context.Background(), killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "test"}, trial.KillBillAccountID, enginetest.PlanUsage, "trial"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.reserve(trial.Scope(), "run", 0, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := e.settleNow(trial.Scope(), e.call, "run", calls(1)); codeOf(err) != "unit_unrated" {
		t.Fatalf("unrated unit: %v", err)
	}
	if n := len(e.fake.UsageRecords(e.brand.Slug)); n != 1 {
		t.Fatalf("usage records = %d", n)
	}
}

// Blocker 2. A hold belongs to the holder that made it. Another holder of
// the tenant, even its owner, can neither settle, release, extend nor grow
// it: reservation_not_found. Staff with a reason can, and are audited.
func TestHoldsBelongToTheirHolder(t *testing.T) {
	e := newEnv(t, "zs-holder")
	ctx := context.Background()
	tn := e.tenant("Holder Co")
	sc := tn.Scope()
	e.credit(tn, 1000)
	key := wallet.Call{Actor: events.Actor{Type: events.ActorAPIKey, ID: "key_svc"}, CorrelationID: "c", Holder: "apikey:key_svc"}
	owner := wallet.Call{Actor: events.Actor{Type: events.ActorUser, ID: "idn_owner"}, CorrelationID: "c", Holder: "membership:mem_owner"}
	if _, err := e.reserveAs(sc, key, "job", 300, time.Minute); err != nil {
		t.Fatal(err)
	}
	in := func(fn func(tx pgx.Tx) error) error { return pgx.BeginFunc(ctx, e.db.Pool, fn) }
	for name, op := range map[string]func() error{
		"settle": func() error { _, err := e.settleNow(sc, owner, "job", wallet.Settlement{CreditMinor: 10}); return err },
		"settle in tx": func() error {
			return in(func(tx pgx.Tx) error {
				_, err := e.svc.Settle(ctx, tx, sc, owner, "job", 0, wallet.Settlement{CreditMinor: 10})
				return err
			})
		},
		"release": func() error {
			return in(func(tx pgx.Tx) error { _, err := e.svc.Release(ctx, tx, sc, owner, "job", 0); return err })
		},
		"extend": func() error {
			return in(func(tx pgx.Tx) error { _, err := e.svc.Extend(ctx, tx, sc, owner, "job", 0, time.Hour); return err })
		},
		"grow":   func() error { _, err := e.reserveAs(sc, owner, "job", 600, time.Minute); return err },
		"replay": func() error { _, err := e.reserveAs(sc, owner, "job", 100, time.Minute); return err },
	} {
		if err := op(); codeOf(err) != "reservation_not_found" {
			t.Fatalf("owner %s on the key's hold: %v", name, err)
		}
	}
	if e.fake.Charges(e.brand.Slug, tn.KillBillAccountID, wallet.ChargeDescriptionPrefix) != 0 {
		t.Fatal("the owner's settlement reached Kill Bill")
	}
	// The key settles its own hold.
	if r, err := e.settleNow(sc, key, "job", wallet.Settlement{CreditMinor: 100}); err != nil || r.Status != wallet.StatusSettled {
		t.Fatalf("key settles = %+v %v", r, err)
	}
	// Staff need a reason, then release another holder's hold.
	if _, err := e.reserveAs(sc, key, "job-2", 100, time.Minute); err != nil {
		t.Fatal(err)
	}
	staff := wallet.Call{Actor: events.Actor{Type: events.ActorStaff, ID: "sub:support"}, CorrelationID: "c", Staff: true}
	if err := in(func(tx pgx.Tx) error { _, err := e.svc.Release(ctx, tx, sc, staff, "job-2", 0); return err }); codeOf(err) != "reason_required" {
		t.Fatalf("staff without a reason: %v", err)
	}
	staff.Reason = "stuck job"
	if err := in(func(tx pgx.Tx) error { _, err := e.svc.Release(ctx, tx, sc, staff, "job-2", 0); return err }); err != nil {
		t.Fatal(err)
	}
	page, err := audit.Query(ctx, e.db.Pool, sc, audit.Filter{Action: wallet.ActionStaffOverride, Limit: 10})
	if err != nil || len(page.Events) != 1 || page.Events[0].Details["holder"] != "apikey:key_svc" || page.Events[0].Reason != "stuck job" {
		t.Fatalf("staff override audit = %+v %v", page.Events, err)
	}
	// Staff never reserve.
	if _, err := e.reserveAs(sc, staff, "staff-hold", 10, time.Minute); codeOf(err) != "permission_denied" {
		t.Fatalf("staff reserve: %v", err)
	}
}

// Major 1. A library caller that settles and then rolls its transaction
// back, the reservation with it, retries with the same ref and attempt.
// Kill Bill's keys come from those, so the retry meets the charge and the
// usage record of the first attempt, and Kill Bill holds one of each.
func TestLibraryRollbackChargesOnce(t *testing.T) {
	e := newEnv(t, "zs-rollback")
	e.meters()
	ctx := context.Background()
	tn := e.tenant("Rollback Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	e.metered(tn)
	attempt := func(ref string, st wallet.Settlement, commit bool) wallet.Reservation {
		t.Helper()
		tx, err := e.db.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, _, err := e.svc.Reserve(ctx, tx, sc, e.call, wallet.ReserveRequest{ExternalRef: ref, AmountMinor: 100}); err != nil {
			t.Fatal(err)
		}
		r, err := e.svc.Settle(ctx, tx, sc, e.call, ref, 0, st)
		if err != nil || r.Status != wallet.StatusSettled {
			t.Fatalf("settle %s = %+v %v", ref, r, err)
		}
		if commit {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	charges := func() int { return e.fake.Charges(e.brand.Slug, tn.KillBillAccountID, wallet.ChargeDescriptionPrefix) }
	attempt("charge", wallet.Settlement{CreditMinor: 60}, false)
	if charges() != 1 {
		t.Fatalf("charges after the rollback = %d", charges())
	}
	r := attempt("charge", wallet.Settlement{CreditMinor: 60}, true)
	if charges() != 1 || e.fake.Credit(e.brand.Slug, tn.KillBillAccountID) != 4940 {
		t.Fatalf("charges after the retry = %d, credit %d", charges(), e.fake.Credit(e.brand.Slug, tn.KillBillAccountID))
	}
	if got, _ := wallet.Get(ctx, e.db.Pool, sc, "charge", 0); got.KillBillRef != r.KillBillRef || got.Status != wallet.StatusSettled {
		t.Fatalf("recovered charge = %+v", got)
	}
	attempt("usage", calls(7), false)
	u := attempt("usage", calls(7), true)
	records := 0
	for _, rec := range e.fake.UsageRecords(e.brand.Slug) {
		if rec.TrackingID == u.KillBillRef {
			records++
		}
	}
	if records != 1 || u.KillBillRef != wallet.UsageTrackingID(sc, "usage", 0) {
		t.Fatalf("usage records under %s = %d", u.KillBillRef, records)
	}
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 0 {
		t.Fatalf("reconcile = %d %v", found, err)
	}
}

// Major 2. Kill Bill is down for longer than the leases: the settlements
// stay pending, no hold expires, and once Kill Bill is back the sweeper
// completes each once, also one whose outbox event was quarantined. A
// settlement waiting a day is audited stale and reported until it
// completes. The generic outbox reader never claims the internal event.
func TestPendingSettlementsOutliveTheirLease(t *testing.T) {
	e := newEnv(t, "zs-outlive")
	e.meters()
	ctx := context.Background()
	tn := e.tenant("Outlive Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	e.metered(tn)
	for _, ref := range []string{"charge-a", "charge-b", "usage-a"} {
		if _, err := e.reserveFor(sc, ref, 200, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	e.fake.FailWrites("/1.0/kb/invoices/charges", 1000, 503)
	e.fake.FailWrites("/1.0/kb/usages", 1000, 503)
	for ref, st := range map[string]wallet.Settlement{"charge-a": {CreditMinor: 50}, "charge-b": {CreditMinor: 70}, "usage-a": calls(30)} {
		if r, err := e.settleNow(sc, e.call, ref, st); err != nil || !r.SettlementPending {
			t.Fatalf("settle %s with Kill Bill down = %+v %v", ref, r, err)
		}
	}
	// The generic reader never takes the settlement relay's events.
	claim, err := outbox.NewPostgresReader(e.db.Pool).Claim(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range claim.Events {
		if ev.Type == wallet.EventSettlementRequested {
			t.Fatal("a generic reader claimed wallet.settlement.requested")
		}
	}
	// One settlement's event is quarantined: the sweeper drives it anyway.
	if _, err := e.db.Pool.Exec(ctx, `UPDATE outbox SET quarantined_at = now() WHERE event_type = 'wallet.settlement.requested'
AND id = (SELECT id FROM outbox WHERE event_type = 'wallet.settlement.requested' ORDER BY occurred_at LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	sweeper := &wallet.Sweeper{Service: e.svc, ReconcileEvery: 1000 * time.Hour}
	for i := 0; i < 30; i++ {
		e.clock.advance(time.Hour)
		if r, err := sweeper.SweepOnce(ctx); err != nil || r.Expired != 0 || r.Settled != 0 {
			t.Fatalf("sweep %d during the outage = %+v %v", i, r, err)
		}
	}
	if heldSum(t, e, tn) != 600 || len(e.fake.UsageRecords(e.brand.Slug)) != 0 ||
		e.fake.Charges(e.brand.Slug, tn.KillBillAccountID, wallet.ChargeDescriptionPrefix) != 0 {
		t.Fatal("the outage changed the holds or Kill Bill")
	}
	if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = $2`, tn.ID, wallet.ActionSettlementStale); n != 3 {
		t.Fatalf("stale audits = %d", n)
	}
	e.fake.FailWrites("/1.0/kb/invoices/charges", 0, 503)
	e.fake.FailWrites("/1.0/kb/usages", 0, 503)
	if _, err := e.svc.Reconcile(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM wallet_reconciliation_findings WHERE tenant_id = $1 AND kind = 'pending_settlement_stale'`, tn.ID); n != 3 {
		t.Fatalf("stale findings = %d", n)
	}
	// The backoff of the last attempt ends within the hour.
	e.clock.advance(time.Hour)
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Settled != 3 || r.Expired != 0 {
		t.Fatalf("sweep after the outage = %+v %v", r, err)
	}
	if r, err := sweeper.SweepOnce(ctx); err != nil || r.Settled != 0 {
		t.Fatalf("second sweep = %+v %v", r, err)
	}
	if got := e.fake.Charges(e.brand.Slug, tn.KillBillAccountID, wallet.ChargeDescriptionPrefix); got != 2 {
		t.Fatalf("charges = %d", got)
	}
	if recs := e.fake.UsageRecords(e.brand.Slug); len(recs) != 1 || recs[0].AmountMicro != 30_000_000 {
		t.Fatalf("usage records = %+v", recs)
	}
	if n := count(t, e.db, `SELECT count(*) FROM reservations WHERE tenant_id = $1 AND status = 'settled'`, tn.ID); n != 3 {
		t.Fatalf("settled = %d", n)
	}
	if n := count(t, e.db, `SELECT count(*) FROM reservation_events WHERE tenant_id = $1 AND kind = 'expire'`, tn.ID); n != 0 {
		t.Fatalf("expired holds = %d", n)
	}
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 0 {
		t.Fatalf("reconcile after completion = %d %v", found, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM wallet_reconciliation_findings WHERE tenant_id = $1`, tn.ID); n != 0 {
		t.Fatalf("findings left = %d", n)
	}
}

// Major 3. A usage settlement records its usage event only once Kill Bill
// took it, in the transaction that settles the reservation. A settlement
// waiting for Kill Bill, or one Kill Bill refused, leaves no event, so the
// summaries and the backlog never see unbilled usage. Settling again after
// a refusal, with the default event ID, records one event. The
// subscription is chosen and stored before Kill Bill is called.
func TestSettlementUsageEventsOnlyOnceBilled(t *testing.T) {
	e := newEnv(t, "zs-orphan")
	e.meters()
	ctx := context.Background()
	tn := e.tenant("Orphan Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	sub := e.metered(tn)
	if _, err := e.reserveFor(sc, "run", 500, time.Hour); err != nil {
		t.Fatal(err)
	}
	events := func() int {
		return count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1`, tn.ID)
	}
	summed := func() int64 {
		sum, err := metering.GetUsageSummary(ctx, e.db.Pool, sc, "", e.clock.Now().AddDate(0, 0, -1), e.clock.Now().AddDate(0, 0, 2))
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, tot := range sum.Totals {
			total += tot.QuantityMicro
		}
		return total
	}
	e.fake.FailWrites("/1.0/kb/usages", 1, 503)
	r, err := e.settleNow(sc, e.call, "run", calls(40))
	if err != nil || !r.SettlementPending || r.KillBillSubscriptionID != sub {
		t.Fatalf("pending settlement = %+v %v", r, err)
	}
	if events() != 0 || summed() != 0 {
		t.Fatalf("pending settlement left %d events, %d in summaries", events(), summed())
	}
	// Kill Bill refuses the next attempt: the settlement fails, the hold
	// stays held, and still no event.
	e.fake.FailWrites("/1.0/kb/usages", 1, 400)
	e.clock.advance(time.Minute)
	if rep, err := (&wallet.Sweeper{Service: e.svc, ReconcileEvery: 1000 * time.Hour}).SweepOnce(ctx); err != nil || rep.Failed != 1 {
		t.Fatalf("sweep = %+v %v", rep, err)
	}
	if got, _ := wallet.Get(ctx, e.db.Pool, sc, "run", 0); got.Status != wallet.StatusHeld || got.SettlementPending || events() != 0 {
		t.Fatalf("after a refusal = %+v, %d events", got, events())
	}
	// Settling again with the default event ID records exactly one event.
	r, err = e.settleNow(sc, e.call, "run", calls(40))
	if err != nil || r.Status != wallet.StatusSettled {
		t.Fatalf("settle again = %+v %v", r, err)
	}
	if events() != 1 || summed() != 40_000_000 {
		t.Fatalf("after the settlement: %d events, %d in summaries", events(), summed())
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND forwarded_at IS NULL`, tn.ID); n != 0 {
		t.Fatalf("unforwarded settlement events = %d", n)
	}
	if recs := e.fake.UsageRecords(e.brand.Slug); len(recs) != 1 || recs[0].SubscriptionID != sub {
		t.Fatalf("usage records = %+v", recs)
	}
}

// Major 4. Two Core instances share the database. One settles: the Kill
// Bill credit drops and its hold is released. The other, within its cache
// lifetime, reads the new wallet version under the lock and cannot
// reserve against the credit it cached.
func TestCachedCreditNeverOutlivesAnotherInstancesSettlement(t *testing.T) {
	e := newEnv(t, "zs-instances")
	tn := e.tenant("Instances Co")
	sc := tn.Scope()
	e.credit(tn, 1000)
	b := &wallet.Service{DB: e.db.Pool, Billing: e.bill, Metering: e.svc.Metering, CreditTTL: time.Hour, Now: e.clock.Now}
	a := &wallet.Service{DB: e.db.Pool, Billing: e.bill, Metering: e.svc.Metering, CreditTTL: time.Hour, Now: e.clock.Now}
	reserveOn := func(w *wallet.Service, ref string, amount int64) error {
		return pgx.BeginFunc(context.Background(), e.db.Pool, func(tx pgx.Tx) error {
			_, _, err := w.Reserve(context.Background(), tx, sc, e.call, wallet.ReserveRequest{ExternalRef: ref, AmountMinor: amount})
			return err
		})
	}
	// B caches the credit of 1000.
	if err := reserveOn(b, "b-1", 100); err != nil {
		t.Fatal(err)
	}
	if err := reserveOn(a, "a-1", 500); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SettleNow(context.Background(), sc, e.call, "a-1", 0, wallet.Settlement{CreditMinor: 500}); err != nil {
		t.Fatal(err)
	}
	// 1000 - 500 charged - 100 held leaves 400.
	if err := reserveOn(b, "b-2", 450); codeOf(err) != "insufficient_credit" {
		t.Fatalf("B reserves against the credit it cached: %v", err)
	}
	if err := reserveOn(b, "b-3", 400); err != nil {
		t.Fatalf("B at the edge: %v", err)
	}
	// A credit Core writes moves the version on too: B sees it at once.
	e.credit(tn, 300)
	if err := reserveOn(b, "b-4", 300); err != nil {
		t.Fatalf("B after a credit: %v", err)
	}
	// A hold reason is read live, never cached.
	e.fake.AccountBlock(e.brand.Slug, tn.KillBillAccountID, killbill.BlockingState{StateName: "STOP", Service: "ops", IsBlockChange: true, IsBlockEntitlement: true})
	if err := reserveOn(b, "b-5", 1); codeOf(err) != "wallet_on_hold" {
		t.Fatalf("B on a blocked account: %v", err)
	}
}

// A duplicate answer from Kill Bill is accepted only for the amount sent
// before under the same key: a charge Kill Bill holds under the key with
// another amount, or a second send of another quantity after an earlier
// one may have reached Kill Bill, is settlement_conflict.
func TestDuplicateOnlyForTheSameAmount(t *testing.T) {
	e := newEnv(t, "zs-dup")
	e.meters()
	ctx := context.Background()
	tn := e.tenant("Dup Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	sub := e.metered(tn)
	if _, err := e.reserveFor(sc, "charge", 100, time.Hour); err != nil {
		t.Fatal(err)
	}
	e.fake.AddExternalCharge(e.brand.Slug, tn.KillBillAccountID, 80, wallet.ChargeDescription(sc, "charge", 0))
	if _, err := e.settle(sc, "charge", 0, wallet.Settlement{CreditMinor: 70}); codeOf(err) != "settlement_conflict" {
		t.Fatalf("charge of another amount under the key: %v", err)
	}
	if r, err := e.settle(sc, "charge", 0, wallet.Settlement{CreditMinor: 80}); err != nil || *r.SettledMinor != 80 {
		t.Fatalf("charge of the same amount = %+v %v", r, err)
	}
	// A usage send whose answer was lost, then a refusal: a settlement of
	// another quantity under the same key is refused before Kill Bill.
	if _, err := e.reserveFor(sc, "usage", 100, time.Hour); err != nil {
		t.Fatal(err)
	}
	e.fake.LoseAnswers("/1.0/kb/usages", 1)
	if r, err := e.settleNow(sc, e.call, "usage", calls(3)); err != nil || !r.SettlementPending {
		t.Fatalf("lost answer = %+v %v", r, err)
	}
	e.fake.FailWrites("/1.0/kb/usages", 1, 400)
	e.clock.advance(time.Minute)
	if rep, err := (&wallet.Sweeper{Service: e.svc, ReconcileEvery: 1000 * time.Hour}).SweepOnce(ctx); err != nil || rep.Failed != 1 {
		t.Fatalf("sweep = %+v %v", rep, err)
	}
	if _, err := e.settleNow(sc, e.call, "usage", calls(5)); codeOf(err) != "settlement_conflict" {
		t.Fatalf("another quantity after a send: %v", err)
	}
	if r, err := e.settleNow(sc, e.call, "usage", calls(3)); err != nil || r.Status != wallet.StatusSettled || r.KillBillSubscriptionID != sub {
		t.Fatalf("the same quantity = %+v %v", r, err)
	}
	if recs := e.fake.UsageRecords(e.brand.Slug); len(recs) != 1 || recs[0].AmountMicro != 3_000_000 {
		t.Fatalf("usage records = %+v", recs)
	}
}

// Usage is reconciled per tracking ID where an invoice lists them: a
// tracking ID Core sent that the invoice of its period lacks is
// missing_usage, and one Core never sent is unknown_usage, reported apart
// even when their amounts cancel out in the month. A cancelled
// subscription is still reconciled.
func TestUsageReconciledPerTrackingID(t *testing.T) {
	e := newEnv(t, "zs-tracking")
	e.meters()
	ctx := context.Background()
	tn := e.tenant("Tracking Co")
	sc := tn.Scope()
	e.credit(tn, 5000)
	sub := e.metered(tn)
	e.forward(tn, 4)
	from := e.clock.Now().Truncate(24*time.Hour).AddDate(0, 0, -1)
	to := from.AddDate(0, 0, 3)
	if found, err := e.svc.Reconcile(ctx, sc); err != nil || found != 0 {
		t.Fatalf("clean = %d %v", found, err)
	}
	// Core believes it sent "ghost" and Kill Bill holds "outside" instead,
	// of the same quantity on the same day.
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollups (id, brand_id, tenant_id, killbill_unit, subscription_id, day, period_start,
    record_at, quantity_micro, events, tracking_id, status, forwarded_at)
VALUES (gen_random_uuid(), $1, $2, 'api-call', $3::uuid, $4::date, $5, $5, 2000000, 1, 'zs-usg-ghost', 'forwarded', $5)`,
		e.brand.ID, tn.ID, sub, e.clock.Now().Format(time.DateOnly), e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.fake.KillBill(t).RecordUsage(ctx, killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "outside"}, killbill.SubscriptionUsageRecord{SubscriptionID: sub, TrackingID: "outside",
			UnitUsageRecords: []killbill.UnitUsageRecords{{UnitType: enginetest.UnitAPICall, UsageRecords: []killbill.UsageRecord{{RecordDate: e.clock.Now().Format(time.DateOnly), Amount: "2"}}}}}); err != nil {
		t.Fatal(err)
	}
	// The month's totals match, so only the tracking IDs tell.
	if e.fake.InvoiceUsage(e.brand.Slug, tn.KillBillAccountID, from, to) == "" {
		t.Fatal("nothing invoiced")
	}
	// Cancelling the subscription keeps it in the reconciliation.
	if err := e.fake.KillBill(t).CancelSubscription(ctx, killbill.TenantCredentials{APIKey: e.brand.Slug, APISecret: "kb-secret-" + e.brand.Slug},
		killbill.CallContext{CreatedBy: "test"}, sub, killbill.PolicyImmediate); err != nil {
		t.Fatal(err)
	}
	found, err := e.svc.Reconcile(ctx, sc)
	if err != nil || found != 2 {
		t.Fatalf("reconcile = %d %v", found, err)
	}
	for kind, ref := range map[string]string{wallet.MismatchMissingUsage: "tracking:zs-usg-ghost", wallet.MismatchUnknownUsage: "tracking:outside"} {
		if n := count(t, e.db, `SELECT count(*) FROM wallet_reconciliation_findings WHERE tenant_id = $1 AND kind = $2 AND killbill_ref = $3`, tn.ID, kind, ref); n != 1 {
			t.Fatalf("%s %s = %d", kind, ref, n)
		}
	}
}
