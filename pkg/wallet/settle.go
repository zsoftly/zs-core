package wallet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/billing"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Settlement is how a reservation ends in Kill Bill: as usage on a
// subscription, or as CreditMinor of the account credit consumed through
// an external charge. Exactly one is set.
type Settlement struct {
	Usage       *UsageSettlement `json:"usage,omitempty"`
	CreditMinor int64            `json:"credit_minor,omitempty"`
}

// UsageSettlement is usage of a Kill Bill unit on a subscription.
// Subscription is optional: without it, the tenant's active subscription
// whose plan prices the unit takes the usage. EventID names the usage
// event Core records when a meter of the brand maps to the unit. It
// defaults to one derived from the reservation's key, rsv:<ref>:<attempt>.
type UsageSettlement struct {
	Subscription  string `json:"subscription,omitempty"`
	Unit          string `json:"unit"`
	QuantityMicro int64  `json:"quantity_micro"`
	EventID       string `json:"event_id,omitempty"`
}

// Settlement timing.
const (
	// settleClaim is how long one worker holds a pending settlement while
	// it calls Kill Bill.
	settleClaim = 2 * time.Minute
	// settleCallTimeout bounds the Kill Bill calls of one attempt, in real
	// time, well inside the claim.
	settleCallTimeout = settleClaim - 30*time.Second
	// Backoff of a settlement Kill Bill could not take: 10 seconds after
	// the first failure, doubling up to an hour.
	settleBackoffFirst = 10 * time.Second
	settleBackoffMax   = time.Hour
	// StaleAfter is how long a settlement may wait for Kill Bill before it
	// is audited as wallet.settlement_stale and reported as the finding
	// pending_settlement_stale.
	StaleAfter = 24 * time.Hour
)

// stored is the settlement as the reservation row keeps it: the request,
// when it was made, so a settlement completed later is dated when it was
// asked for, and the subscription a usage settlement goes to, chosen
// before Kill Bill is called and never changed.
type stored struct {
	Settlement   Settlement `json:"settlement"`
	RequestedAt  time.Time  `json:"requested_at"`
	Subscription string     `json:"subscription,omitempty"`
}

func (st Settlement) check(r Reservation) (Settlement, error) {
	switch {
	case st.Usage == nil && st.CreditMinor == 0:
		return Settlement{}, validation("A settlement needs usage or credit_minor.")
	case st.Usage != nil && st.CreditMinor != 0:
		return Settlement{}, validation("A settlement has usage or credit_minor, not both.")
	case st.CreditMinor < 0:
		return Settlement{}, validation("The credit_minor must be positive.")
	case st.CreditMinor > MaxAmountMinor:
		return Settlement{}, validation("The credit_minor must be at most 10000000000.")
	}
	if u := st.Usage; u != nil {
		if !unitPattern.MatchString(u.Unit) {
			return Settlement{}, validation("The unit must be a usage unit of the catalogue.")
		}
		if u.QuantityMicro <= 0 || u.QuantityMicro > metering.MaxQuantityMicro {
			return Settlement{}, validation("The quantity_micro must be positive and at most 1000000000000000.")
		}
		if u.Subscription != "" {
			if _, err := idgen.ParseUUID(u.Subscription); err != nil || len(u.Subscription) != 36 {
				return Settlement{}, validation("The subscription must be a Kill Bill subscription ID.")
			}
		}
		if u.EventID == "" {
			c := *u
			c.EventID = "rsv:" + r.ExternalRef + ":" + strconv.Itoa(r.Attempt)
			st.Usage = &c
		}
	}
	return st, nil
}

// sameAmount reports whether two settlements ask Kill Bill for the same
// thing: the same unit and quantity, or the same charge.
func sameAmount(a, b Settlement) bool {
	if (a.Usage == nil) != (b.Usage == nil) {
		return false
	}
	if a.Usage == nil {
		return a.CreditMinor == b.CreditMinor
	}
	return a.Usage.Unit == b.Usage.Unit && a.Usage.QuantityMicro == b.Usage.QuantityMicro
}

var unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

var (
	errExceedsHold         = zserrors.New(zserrors.CodeSettlementExceedsHold, "")
	errUnrated             = zserrors.New(zserrors.CodeUnitUnrated, "The catalogue prices no tier of this unit on the subscription's current phase, so the settlement cannot be checked against the hold.")
	errSettlementConflict  = zserrors.New(zserrors.CodeSettlementConflict, "")
	errSettlementRacedPlan = zserrors.New(zserrors.CodeReservationSettlementPending, "The reservation changed while the settlement was prepared. Retry it.")
)

// fingerprintOf is the digest of a settlement request. A settle replay with
// another request is reservation_conflict.
func fingerprintOf(st Settlement) string {
	b, _ := json.Marshal(st)
	return digest("zs-core.settlement.v1", string(b))
}

// settlementKey is the digest every Kill Bill key of a settlement derives
// from: the brand, the tenant, the caller's external_ref and attempt, and
// the kind of object. It never uses the reservation's ID, so a caller that
// rolled its transaction back and retries with the same ref and attempt
// reaches the object its first attempt made.
func settlementKey(sc scope.Scope, ref string, attempt int, kind string) string {
	return digest("zs-core.settlement-key.v1", sc.Brand.String(), sc.Tenant.String(), ref, strconv.Itoa(attempt), kind)
}

// UsageTrackingID is the Kill Bill tracking ID of a usage settlement of the
// reservation keyed by ref and attempt. Every attempt of the settlement
// sends the same one, and Kill Bill keeps one record per tracking ID and
// subscription.
func UsageTrackingID(sc scope.Scope, ref string, attempt int) string {
	return "zs-rsv-" + settlementKey(sc, ref, attempt, "usage")[:40]
}

// ChargeDescriptionPrefix starts the description of every external charge
// a settlement writes.
const ChargeDescriptionPrefix = "zs-core settlement "

// ChargeDescription is the description of the external charge a credit
// settlement of the reservation keyed by ref and attempt writes. A retry
// finds the charge by it.
func ChargeDescription(sc scope.Scope, ref string, attempt int) string {
	return ChargeDescriptionPrefix + settlementKey(sc, ref, attempt, "charge")[:40]
}

// state is the settlement bookkeeping of a reservation row.
type state struct {
	fingerprint string
	current     *stored
	sent        *stored
	attempts    int
	token       []byte
	estimate    *int64
}

func loadState(ctx context.Context, q Querier, sc scope.Scope, id idgen.UUID) (state, error) {
	args, err := sc.Bind(id)
	if err != nil {
		return state{}, err
	}
	var (
		st          state
		current     []byte
		sent        []byte
		fingerprint *string
	)
	if err := q.QueryRow(ctx, `SELECT settlement_fingerprint, settlement, sent_settlement, settle_attempts, settle_claim_token, estimate_minor
FROM reservations WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, args...).Scan(&fingerprint, &current, &sent, &st.attempts, &st.token, &st.estimate); err != nil {
		return state{}, unavailable(err)
	}
	if fingerprint != nil {
		st.fingerprint = *fingerprint
	}
	for _, f := range []struct {
		raw []byte
		dst **stored
	}{{current, &st.current}, {sent, &st.sent}} {
		if len(f.raw) == 0 {
			continue
		}
		var v stored
		if err := json.Unmarshal(f.raw, &v); err != nil {
			return state{}, zserrors.Wrap(zserrors.CodeInternal, "", err)
		}
		*f.dst = &v
	}
	return st, nil
}

// plan is what a settlement sends to Kill Bill, worked out from Kill Bill
// before the tenant lock is taken.
type plan struct {
	st       stored
	fp       string
	estimate int64
	ratedBy  string
	// sentSub is the subscription an earlier send of this reservation
	// used, when the plan was made.
	sentSub string
}

// resolve works out a settlement. A usage settlement goes to the
// subscription an earlier send of the reservation used, otherwise the
// requested one, otherwise the tenant's first active subscription whose
// plan prices the unit, and must have started by now. It is priced with
// the first tier of the subscription's current phase. A unit the phase
// does not price is unit_unrated. A credit settlement is worth its
// credit_minor.
func (s *Service) resolve(ctx context.Context, acct *billing.TenantAccount, r Reservation, sent *stored, req Settlement, now time.Time) (plan, error) {
	req, err := req.check(r)
	if err != nil {
		return plan{}, err
	}
	p := plan{fp: fingerprintOf(req), st: stored{Settlement: req, RequestedAt: now}}
	u := req.Usage
	if u == nil {
		p.estimate, p.ratedBy = req.CreditMinor, RatedByExternalCharge
		return p, nil
	}
	requested := u.Subscription
	if sent != nil && sent.Subscription != "" {
		requested, p.sentSub = sent.Subscription, sent.Subscription
	}
	target, err := acct.UsageSubscriptionAt(ctx, requested, u.Unit, now)
	if err != nil {
		return plan{}, err
	}
	v, ok := acct.EstimateUsage(target.Plan, target.Subscription.PhaseType, u.Unit, u.QuantityMicro)
	if !ok {
		return plan{}, errUnrated
	}
	p.st.Subscription = target.Subscription.SubscriptionID
	p.estimate, p.ratedBy = v, RatedByCatalogue
	return p, nil
}

// prepared is the outcome of prepare: a reservation and, when answered is
// false, the settlement to write and send.
type prepared struct {
	r        Reservation
	answered bool
}

// prepare takes the tenant lock and the reservation in tx and decides the
// settlement. A settlement the reservation already settled, or one that
// waits for Kill Bill with the same request, is answered at once. Otherwise
// it checks the plan under the lock: its value may not exceed the hold,
// settlement_exceeds_hold, an earlier send under the same key must have
// asked for the same amount, settlement_conflict, and a usage event the
// settlement records must be free to record.
func (s *Service) prepare(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, ref string, attempt int, req Settlement, p *plan) (prepared, error) {
	if err := lockTenant(ctx, tx, sc); err != nil {
		return prepared{}, err
	}
	r, err := get(ctx, tx, sc, ref, attempt, true)
	if err != nil {
		return prepared{}, err
	}
	if !call.owns(r) {
		return prepared{}, errNotFound
	}
	st, err := loadState(ctx, tx, sc, r.id)
	if err != nil {
		return prepared{}, err
	}
	normalized, checkErr := req.check(r)
	if checkErr != nil && r.Status == StatusHeld && !r.SettlementPending {
		return prepared{}, checkErr
	}
	fp := fingerprintOf(normalized)
	switch {
	case r.Status == StatusSettled && st.fingerprint == fp:
		return prepared{r: r, answered: true}, nil
	case r.Status == StatusSettled:
		return prepared{}, errConflict
	case r.Status != StatusHeld:
		return prepared{}, errNotHeld
	case r.SettlementPending && st.fingerprint == fp:
		return prepared{r: r, answered: true}, nil
	case r.SettlementPending:
		return prepared{}, errPending
	}
	if p == nil || p.fp != fp {
		return prepared{}, errSettlementRacedPlan
	}
	sentSub := ""
	if st.sent != nil {
		sentSub = st.sent.Subscription
	}
	if sentSub != p.sentSub {
		return prepared{}, errSettlementRacedPlan
	}
	if p.estimate > r.AmountMinor {
		return prepared{}, zserrors.New(zserrors.CodeSettlementExceedsHold,
			"The settlement is worth "+strconv.FormatInt(p.estimate, 10)+" minor units and the reservation holds "+strconv.FormatInt(r.AmountMinor, 10)+".")
	}
	if st.sent != nil && !sameAmount(st.sent.Settlement, p.st.Settlement) {
		return prepared{}, errSettlementConflict
	}
	if u := p.st.Settlement.Usage; u != nil {
		if err := s.metering().CheckSettledUsage(ctx, tx, sc, s.mcall(call), u.Unit, settledEvent(p.st)); err != nil {
			return prepared{}, err
		}
	}
	return prepared{r: r}, nil
}

func (s *Service) mcall(call Call) metering.Call {
	return metering.Call{Actor: call.Actor, CorrelationID: call.CorrelationID}
}

// settledEvent is the usage event a usage settlement records once it
// completes.
func settledEvent(st stored) metering.Event {
	u := st.Settlement.Usage
	return metering.Event{EventID: u.EventID, QuantityMicro: u.QuantityMicro, OccurredAt: st.RequestedAt, Subscription: st.Subscription}
}

// writeIntent records the settlement on the reservation, in tx, before Kill
// Bill is called: settlement_pending, the request, its fingerprint, the
// chosen subscription, the estimate, and the settlement as sent. token
// names the worker that calls Kill Bill. It is nil for a library call,
// whose transaction holds the reservation until it ends.
func writeIntent(ctx context.Context, tx pgx.Tx, sc scope.Scope, r Reservation, p plan, token []byte, now time.Time) (Reservation, error) {
	body, err := json.Marshal(p.st)
	if err != nil {
		return Reservation{}, err
	}
	var sub, until any
	if p.st.Subscription != "" {
		sub = p.st.Subscription
	}
	if token != nil {
		until = now.Add(settleClaim)
	}
	args, _ := sc.Bind(r.id, body, p.fp, sub, p.estimate, p.st.RequestedAt, token, until, now)
	out, err := scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET settlement_pending = true, settlement = $4::jsonb,
    settlement_fingerprint = $5, killbill_subscription_id = $6::uuid, estimate_minor = $7, settle_requested_at = $8,
    settle_claim_token = $9, settle_claimed_until = $10, settle_next_at = $11, sent_settlement = $4::jsonb, updated_at = $11
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+reservationColumns, args...))
	if err != nil {
		return Reservation{}, unavailable(err)
	}
	return out, nil
}

// sent is what Kill Bill made for a settlement.
type sent struct {
	refType, ref, ratedBy string
	settled               *int64
	details               map[string]any
	trackingID            string
}

// send writes a settlement to Kill Bill: a usage record under the tracking
// ID derived from the reservation's key, or an external charge with the
// description derived from it, found again before a new one is written.
// A charge Kill Bill already holds under the key with another amount is
// settlement_conflict. It returns Kill Bill's failure as it is, so the
// caller can tell an unreachable Kill Bill from a refusal.
func (s *Service) send(ctx context.Context, acct *billing.TenantAccount, sc scope.Scope, call Call, r Reservation, st stored, estimate *int64) (sent, error) {
	ctx, cancel := context.WithTimeout(ctx, settleCallTimeout)
	defer cancel()
	out := sent{details: map[string]any{}}
	if u := st.Settlement.Usage; u != nil {
		tracking := UsageTrackingID(sc, r.ExternalRef, r.Attempt)
		duplicate, err := acct.RecordUsage(ctx, call.billing(), st.Subscription, u.Unit, tracking,
			[]billing.UsageAmount{{At: st.RequestedAt, AmountMicro: u.QuantityMicro}})
		if err != nil {
			return sent{}, err
		}
		out.refType, out.ref, out.trackingID, out.ratedBy = RefUsageRecord, tracking, tracking, RatedByNone
		if estimate != nil {
			v := *estimate
			out.settled, out.ratedBy = &v, RatedByCatalogue
		}
		out.details["killbill_subscription_id"] = st.Subscription
		out.details["killbill_tracking_id"] = tracking
		out.details["killbill_unit"] = u.Unit
		out.details["quantity_micro"] = u.QuantityMicro
		out.details["duplicate"] = duplicate
		return out, nil
	}
	item, err := acct.Charge(ctx, call.billing(), st.Settlement.CreditMinor, ChargeDescription(sc, r.ExternalRef, r.Attempt))
	if err != nil {
		return sent{}, err
	}
	charged, err := billing.ToMinor(item.Amount, acct.Currency())
	if err != nil {
		return sent{}, billing.KBError(err)
	}
	if charged != st.Settlement.CreditMinor {
		return sent{}, errSettlementConflict
	}
	out.settled, out.refType, out.ref, out.ratedBy = &charged, RefExternalCharge, item.InvoiceItemID, RatedByExternalCharge
	out.details["killbill_charge_id"] = item.InvoiceItemID
	out.details["killbill_invoice_id"] = item.InvoiceID
	return out, nil
}

// complete records what Kill Bill made, in tx: the usage event a usage
// settlement records, written now and only now, the reservation settled
// with the Kill Bill reference, and the wallet version moved on, since the
// Kill Bill credit or unbilled amount changed and the hold is released.
func (s *Service) complete(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, r Reservation, st stored, out sent) (Reservation, error) {
	details := out.details
	if u := st.Settlement.Usage; u != nil {
		recorded, err := s.metering().RecordSettledUsage(ctx, tx, sc, s.mcall(call), r.id, u.Unit, out.trackingID, settledEvent(st))
		if err != nil {
			return Reservation{}, err
		}
		if recorded != nil {
			details["usage_id"] = recorded.ID
		}
	}
	if out.settled != nil {
		details["settled_minor"] = *out.settled
	}
	var subArg any
	if st.Subscription != "" {
		subArg = st.Subscription
	}
	holder := r.holder
	now := s.now()
	args, _ := sc.Bind(r.id, out.settled, out.ratedBy, out.refType, out.ref, subArg, now)
	r, err := scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET status = 'settled', settlement_pending = false,
    settled_minor = $4, rated_by = $5, killbill_ref_type = $6, killbill_ref = $7, killbill_subscription_id = $8::uuid,
    settled_at = $9, updated_at = $9, settle_claim_token = NULL, settle_claimed_until = NULL, settle_next_at = NULL
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+reservationColumns, args...))
	if err != nil {
		return Reservation{}, unavailable(err)
	}
	if err := billing.BumpWalletVersion(ctx, tx, sc); err != nil {
		return Reservation{}, err
	}
	if err := event(ctx, tx, sc, call, r, "settle", details); err != nil {
		return Reservation{}, err
	}
	if err := staffOverride(ctx, tx, sc, call, r, holder, "settle"); err != nil {
		return Reservation{}, err
	}
	if err := record(ctx, tx, sc, call, "wallet.reservation.settled", EventReservationSettled, r, details); err != nil {
		return Reservation{}, err
	}
	s.forget(sc)
	return r, nil
}

// queue keeps a settlement Kill Bill could not take, in tx: the hold stays
// held and pending, the next attempt waits the backoff, and the first time
// the settlement is queued the relay is woken.
func (s *Service) queue(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, r Reservation, attempts int) (Reservation, error) {
	now := s.now()
	args, _ := sc.Bind(r.id, now.Add(settleBackoff(attempts)), now)
	r, err := scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET settle_attempts = settle_attempts + 1, settle_next_at = $4,
    settle_claim_token = NULL, settle_claimed_until = NULL, updated_at = $5
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+reservationColumns, args...))
	if err != nil {
		return Reservation{}, unavailable(err)
	}
	if attempts > 0 {
		return r, nil
	}
	if err := event(ctx, tx, sc, call, r, "settlement_queued", nil); err != nil {
		return Reservation{}, err
	}
	if err := record(ctx, tx, sc, call, "wallet.reservation.settlement_queued", "", r, nil); err != nil {
		return Reservation{}, err
	}
	return r, writeEvent(ctx, tx, sc, call, EventSettlementRequested, events.Aggregate{Type: "reservation", ID: r.ID},
		map[string]any{"reservation_id": r.ID, "external_ref": r.ExternalRef, "attempt": r.Attempt})
}

// settleBackoff is the wait after the given number of attempts Kill Bill
// could not take.
func settleBackoff(attempts int) time.Duration {
	wait := settleBackoffFirst
	for i := 0; i < attempts && wait < settleBackoffMax; i++ {
		wait *= 2
	}
	return min(wait, settleBackoffMax)
}

// failed records a settlement Kill Bill refused, in tx. The hold stays held
// until it is released or expires, and reservation.settlement_failed is
// emitted. The settlement as sent is forgotten only when no earlier
// attempt could have reached Kill Bill.
func (s *Service) failed(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, r Reservation, cause error) error {
	s.log().WarnContext(ctx, "settlement refused", "reservation_id", r.ID, "error", cause.Error())
	args, _ := sc.Bind(r.id, s.now())
	r, err := scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET settlement_pending = false, settlement = NULL,
    settlement_fingerprint = NULL, estimate_minor = NULL, settle_requested_at = NULL, settle_claim_token = NULL,
    settle_claimed_until = NULL, settle_next_at = NULL, stale_reported_at = NULL,
    sent_settlement = CASE WHEN settle_attempts = 0 THEN NULL ELSE sent_settlement END,
    killbill_subscription_id = CASE WHEN settle_attempts = 0 THEN NULL ELSE killbill_subscription_id END,
    settle_attempts = 0, updated_at = $4
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+reservationColumns, args...))
	if err != nil {
		return unavailable(err)
	}
	details := map[string]any{"error": errorName(cause)}
	if err := event(ctx, tx, sc, call, r, "settlement_failed", details); err != nil {
		return err
	}
	return record(ctx, tx, sc, call, "wallet.reservation.settlement_failed", EventSettlementFailed, r, details)
}

// planFor reads the reservation without a lock and resolves the settlement
// against Kill Bill. A reservation that is not plainly held gets no plan:
// prepare answers it without Kill Bill.
func (s *Service) planFor(ctx context.Context, q billing.Querier, sc scope.Scope, call Call, ref string, attempt int, req Settlement) (*plan, error) {
	r, err := get(ctx, q, sc, ref, attempt, false)
	if err != nil {
		return nil, err
	}
	if !call.owns(r) {
		return nil, errNotFound
	}
	if r.Status != StatusHeld || r.SettlementPending {
		return nil, nil
	}
	st, err := loadState(ctx, q, sc, r.id)
	if err != nil {
		return nil, err
	}
	acct, err := s.Billing.TenantAccount(ctx, q, sc)
	if err != nil {
		return nil, err
	}
	p, err := s.resolve(ctx, acct, r, st.sent, req, s.now())
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Service) checkSettle(call Call, ref string, attempt int) error {
	if !s.Ready() {
		return errNotReady
	}
	if err := call.validate(); err != nil {
		return err
	}
	if err := checkKey(ref, attempt); err != nil {
		return errNotFound
	}
	return nil
}

// Settle ends a held reservation in Kill Bill, in tx, the caller's
// transaction. It writes the settlement's intent to the reservation, then
// writes to Kill Bill: a usage record on the subscription with a tracking
// ID derived from the caller's ref and attempt, or an external charge that
// Kill Bill pays from the account credit, with a description derived from
// them. Then it records the Kill Bill reference, marks the reservation
// settled and so releases the rest of the hold. Settle holds the tenant
// lock in tx for the whole call, Kill Bill's included. SettleNow does not.
//
// The settlement is checked under the lock first: its catalogue value, or
// its credit_minor, may not exceed the hold, settlement_exceeds_hold. A
// usage settlement of a unit the subscription's current phase does not
// price is unit_unrated.
//
// A retry is safe. Kill Bill's keys come from the caller's keys only, so a
// caller that rolled tx back after Settle and retries with the same ref and
// attempt meets the usage record or the charge its first attempt made, and
// Core records it instead of making another. The same settlement again
// returns the reservation. Another settlement is reservation_conflict. A
// released or expired reservation is reservation_not_held. Only the
// reservation's holder, or a staff override, may settle it.
//
// When Kill Bill cannot be reached, the settlement waits in the
// reservation, pending, and the sweeper completes it once Kill Bill
// answers. The hold keeps counting against the credit, never expires, and
// cannot grow, settle differently or be released meanwhile. Settle returns
// it with no error. A settlement Kill Bill refuses leaves nothing in tx:
// the error is returned and the hold stays held.
func (s *Service) Settle(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, ref string, attempt int, req Settlement) (Reservation, error) {
	if err := s.checkSettle(call, ref, attempt); err != nil {
		return Reservation{}, err
	}
	p, err := s.planFor(ctx, tx, sc, call, ref, attempt, req)
	if err != nil {
		return Reservation{}, err
	}
	pr, err := s.prepare(ctx, tx, sc, call, ref, attempt, req, p)
	if err != nil || pr.answered {
		return pr.r, err
	}
	acct, err := s.Billing.TenantAccount(ctx, tx, sc)
	if err != nil {
		return Reservation{}, err
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Reservation{}, unavailable(err)
	}
	defer func() { _ = sp.Rollback(context.WithoutCancel(ctx)) }()
	r, err := writeIntent(ctx, sp, sc, pr.r, *p, nil, s.now())
	if err != nil {
		return Reservation{}, err
	}
	r.holder = pr.r.holder
	estimate := p.estimate
	out, sendErr := s.send(ctx, acct, sc, call, r, p.st, &estimate)
	switch {
	case sendErr == nil:
		r, err = s.complete(ctx, sp, sc, call, r, p.st, out)
	case billing.Unreachable(sendErr):
		s.log().WarnContext(ctx, "settlement queued: Kill Bill is unreachable", "reservation_id", r.ID, "error", sendErr.Error())
		r, err = s.queue(ctx, sp, sc, call, r, 0)
	default:
		return Reservation{}, sendErr
	}
	if err != nil {
		return Reservation{}, err
	}
	if err := sp.Commit(ctx); err != nil {
		return Reservation{}, unavailable(err)
	}
	return r, nil
}

// SettleNow settles like Settle, in its own transactions on DB, and never
// holds the tenant lock while it calls Kill Bill: it writes the intent and
// commits, calls Kill Bill, then takes the lock again to complete. Reserve
// is never blocked by a Kill Bill call. The HTTP route uses it. A
// settlement Kill Bill could not take is returned pending. A settlement
// Kill Bill refused is recorded as failed and its error returned.
func (s *Service) SettleNow(ctx context.Context, sc scope.Scope, call Call, ref string, attempt int, req Settlement) (Reservation, error) {
	if err := s.checkSettle(call, ref, attempt); err != nil {
		return Reservation{}, err
	}
	if s.DB == nil {
		return Reservation{}, errNotReady
	}
	p, err := s.planFor(ctx, s.DB, sc, call, ref, attempt, req)
	if err != nil {
		return Reservation{}, err
	}
	token := newToken()
	var (
		pr prepared
		r  Reservation
	)
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		pr, err = s.prepare(ctx, tx, sc, call, ref, attempt, req, p)
		if err != nil || pr.answered {
			return err
		}
		r, err = writeIntent(ctx, tx, sc, pr.r, *p, token, s.now())
		return err
	})
	if err != nil || pr.answered {
		return pr.r, err
	}
	r.holder = pr.r.holder
	estimate := p.estimate
	r, _, err = s.drive(ctx, sc, call, r, p.st, &estimate, 0, token)
	return r, err
}

func newToken() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// outcome is how one attempt of a pending settlement ended.
type outcome int

const (
	outcomeSettled outcome = iota
	outcomeQueued
	outcomeFailed
	outcomeLost
)

// drive makes one attempt of a pending settlement whose claim token holds:
// it calls Kill Bill, then completes, queues or fails the settlement in a
// new transaction under the tenant lock, provided the claim still holds.
// attempts is how many attempts Kill Bill could not take before this one.
func (s *Service) drive(ctx context.Context, sc scope.Scope, call Call, r Reservation, st stored, estimate *int64, attempts int, token []byte) (Reservation, outcome, error) {
	acct, err := s.Billing.TenantAccount(ctx, s.DB, sc)
	if zserrors.IsCode(err, zserrors.CodeDatabaseUnavailable) {
		// The claim runs out and the sweeper tries again.
		return Reservation{}, outcomeQueued, err
	}
	var out sent
	sendErr := err
	if err == nil {
		out, sendErr = s.send(ctx, acct, sc, call, r, st, estimate)
	}
	if sendErr != nil && ctx.Err() != nil {
		// This worker was stopped, so it cannot tell what Kill Bill did.
		// Its claim runs out and another attempt finds out.
		return Reservation{}, outcomeQueued, ctx.Err()
	}
	result := outcomeSettled
	var final Reservation
	err = pgx.BeginFunc(context.WithoutCancel(ctx), s.DB, func(tx pgx.Tx) error {
		ctx := context.WithoutCancel(ctx)
		if err := lockTenant(ctx, tx, sc); err != nil {
			return err
		}
		cur, err := get(ctx, tx, sc, r.ExternalRef, r.Attempt, true)
		if err != nil {
			return err
		}
		held, err := loadState(ctx, tx, sc, cur.id)
		if err != nil {
			return err
		}
		if !cur.SettlementPending || !bytes.Equal(held.token, token) {
			// Another worker took the settlement over after this claim ran
			// out. It sends the same keys, so Kill Bill made one object, and
			// it records the settlement itself.
			final, result = cur, outcomeLost
			return nil
		}
		switch {
		case sendErr == nil:
			final, err = s.complete(ctx, tx, sc, call, cur, st, out)
		case billing.Unreachable(sendErr):
			result = outcomeQueued
			s.log().WarnContext(ctx, "settlement waits: Kill Bill is unreachable", "reservation_id", cur.ID, "error", sendErr.Error())
			final, err = s.queue(ctx, tx, sc, call, cur, attempts)
		default:
			result = outcomeFailed
			if err := s.failed(ctx, tx, sc, call, cur, sendErr); err != nil {
				return err
			}
			final, err = get(ctx, tx, sc, r.ExternalRef, r.Attempt, false)
		}
		return err
	})
	if err != nil {
		return Reservation{}, result, err
	}
	if result == outcomeFailed {
		return Reservation{}, result, sendErr
	}
	return final, result, nil
}

// claimed is a pending settlement one worker holds.
type claimed struct {
	sc       scope.Scope
	r        Reservation
	st       stored
	estimate *int64
	attempts int
	token    []byte
}

// claimSQL claims pending settlements no other worker holds. due limits it
// to those whose backoff ended. key limits it to one reservation.
var claimSQL = `UPDATE reservations r SET settle_claim_token = $1, settle_claimed_until = $2
FROM (SELECT id FROM reservations
      WHERE settlement_pending AND (settle_claimed_until IS NULL OR settle_claimed_until <= $3)
        AND (NOT $4 OR settle_next_at <= $3)
        AND ($5::uuid IS NULL OR (brand_id = $5 AND tenant_id = $6 AND external_ref = $7 AND attempt = $8))
      ORDER BY settle_next_at, id LIMIT $9 FOR UPDATE SKIP LOCKED) due
WHERE r.id = due.id
RETURNING r.brand_id, r.tenant_id, r.settlement, r.estimate_minor, r.settle_attempts, ` + prefixed

// prefixed is reservationColumns in a statement where id alone is
// ambiguous.
var prefixed = "r." + reservationColumns

// claim takes pending settlements for this worker.
func (s *Service) claim(ctx context.Context, due bool, key *struct {
	sc      scope.Scope
	ref     string
	attempt int
}, limit int) ([]claimed, error) {
	now := s.now()
	token := newToken()
	var brandID, tenantID, ref, attempt any
	if key != nil {
		brandID, tenantID, ref, attempt = key.sc.Brand, key.sc.Tenant, key.ref, key.attempt
	}
	rows, err := s.DB.Query(ctx, claimSQL, token, now.Add(settleClaim), now, due, brandID, tenantID, ref, attempt, limit)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var out []claimed
	for rows.Next() {
		c := claimed{token: token}
		var body []byte
		r, err := scanReservation(rows, &c.sc.Brand, &c.sc.Tenant, &body, &c.estimate, &c.attempts)
		if err != nil {
			return nil, unavailable(err)
		}
		if err := json.Unmarshal(body, &c.st); err != nil {
			return nil, zserrors.Wrap(zserrors.CodeInternal, "", err)
		}
		c.r = r
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// relayCall is the actor of a settlement the relay or the sweeper drives.
func relayCall(correlationID string) Call {
	return Call{Actor: events.Actor{Type: events.ActorSystem}, Reason: "pending settlement", CorrelationID: correlationID}
}

// RelayHandler completes the settlement a wallet.settlement.requested event
// names, as soon as it is written. While Kill Bill stays unreachable the
// event is deferred, without spending an attempt, so a settlement is never
// quarantined for it. The sweeper drives every pending settlement as well,
// so a settlement whose event was quarantined anyway still completes.
func (s *Service) RelayHandler() outbox.Handler {
	return outbox.HandlerFunc(func(ctx context.Context, env events.Envelope) error {
		if env.Type != EventSettlementRequested || s.DB == nil {
			return outbox.ErrDeliveryTerminal
		}
		sc := scope.Scope{Brand: env.Brand(), Tenant: env.Tenant()}
		var payload struct {
			ExternalRef string `json:"external_ref"`
			Attempt     int    `json:"attempt"`
		}
		if err := json.Unmarshal(env.Payload, &payload); err != nil || sc.Validate() != nil || checkKey(payload.ExternalRef, payload.Attempt) != nil {
			return outbox.ErrDeliveryTerminal
		}
		list, err := s.claim(ctx, false, &struct {
			sc      scope.Scope
			ref     string
			attempt int
		}{sc, payload.ExternalRef, payload.Attempt}, 1)
		if err != nil {
			return err
		}
		if len(list) == 0 {
			// Another worker holds the settlement, or it is no longer
			// pending. While it is pending the event waits.
			r, err := get(ctx, s.DB, sc, payload.ExternalRef, payload.Attempt, false)
			if err == nil && r.SettlementPending {
				return outbox.ErrDeliveryDeferred
			}
			return nil
		}
		for _, c := range list {
			_, result, err := s.drive(ctx, c.sc, relayCall(env.CorrelationID), c.r, c.st, c.estimate, c.attempts, c.token)
			if result == outcomeQueued {
				return outbox.ErrDeliveryDeferred
			}
			if result == outcomeFailed {
				// The refusal is recorded on the reservation and emitted.
				return nil
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func errorName(err error) string {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return zserrors.Lookup(zsErr.Code).Name
	}
	return "internal_error"
}
