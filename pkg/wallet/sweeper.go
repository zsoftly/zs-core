package wallet

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// SweeperActor is the audit actor of the wallet sweeper.
var SweeperActor = events.Actor{Type: events.ActorSystem}

// DefaultReconcileEvery is how often the sweeper compares one tenant's
// wallet with Kill Bill. Each comparison reads every invoice of the
// account, so it runs less often than lease expiry.
const DefaultReconcileEvery = time.Hour

// Mismatch kinds of the reconciliation sweep.
const (
	// MismatchUnknownCredit is a Kill Bill credit no Core credit explains,
	// such as one an operator added in KAUI.
	MismatchUnknownCredit = "unknown_credit"
	// MismatchMissingCredit is a Core credit Kill Bill does not hold with
	// its amount.
	MismatchMissingCredit = "missing_credit"
	// MismatchUnknownCharge is a settlement charge in Kill Bill that no
	// settled reservation names.
	MismatchUnknownCharge = "unknown_charge"
	// MismatchMissingCharge is a settled reservation whose external charge
	// Kill Bill does not hold with its amount.
	MismatchMissingCharge = "missing_charge"
	// MismatchUnknownUsage is usage Kill Bill holds beyond what Core
	// forwarded or settled: a tracking ID on an invoice Core never sent, or
	// more usage for a subscription and unit in a month.
	MismatchUnknownUsage = "unknown_usage"
	// MismatchMissingUsage is usage Core forwarded or settled that Kill
	// Bill does not hold: a tracking ID absent from the invoice that bills
	// its period, or less usage for a subscription and unit in a month.
	MismatchMissingUsage = "missing_usage"
	// MismatchHeldMismatch is a held reservation whose amount is not the
	// hold its own history records, or whose history ended it.
	MismatchHeldMismatch = "held_mismatch"
	// MismatchOverReserved is informational: the holds exceed the Kill Bill
	// credit, less the usage part of the unbilled amount, plus the overdraft
	// allowance. Recurring charges are left out, so a tenant whose holds
	// only crowd out a coming subscription renewal is not reported.
	MismatchOverReserved = "over_reserved"
	// MismatchPendingSettlementStale is a settlement still waiting for Kill
	// Bill StaleAfter after it was asked for.
	MismatchPendingSettlementStale = "pending_settlement_stale"
	// MismatchRejectedUsage is a usage rollup the forwarder refused to send
	// because Kill Bill would not bill it, with its quantity and events. It
	// stays until staff reassign or discard it.
	MismatchRejectedUsage = "rejected_usage"
)

// creditKinds are the findings that mean the Kill Bill credit changed in a
// way Core did not make. Reporting one moves the wallet version on.
var creditKinds = map[string]bool{MismatchUnknownCredit: true, MismatchMissingCredit: true, MismatchUnknownCharge: true, MismatchMissingCharge: true}

// Sweeper expires reservations past their lease, drives the settlements
// that wait for Kill Bill, and reconciles each tenant's wallet with Kill
// Bill. It never changes Kill Bill except to complete a settlement a
// caller asked for.
type Sweeper struct {
	Service *Service
	// Batch is how many reservations one expiry or settlement page reads.
	// Zero means 100.
	Batch int
	// ReconcileEvery is how often a tenant is reconciled. Zero means
	// DefaultReconcileEvery.
	ReconcileEvery time.Duration
}

// SweepReport counts what one pass did.
type SweepReport struct {
	Expired int
	// Settled counts pending settlements this pass completed, Pending those
	// Kill Bill still could not take, and Failed those it refused.
	Settled int
	Pending int
	Failed  int
	// Stale counts settlements reported stale for the first time.
	Stale      int
	Reconciled int
	// Mismatches counts the findings this pass reported for the first
	// time.
	Mismatches int
	Errors     int
}

// Run sweeps every interval until ctx ends.
func (w *Sweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := w.SweepOnce(ctx)
		if err != nil {
			w.Service.log().Error("wallet sweep failed", "error", err.Error())
		} else if report.Expired > 0 || report.Settled > 0 || report.Pending > 0 || report.Failed > 0 || report.Stale > 0 ||
			report.Mismatches > 0 || report.Errors > 0 {
			w.Service.log().Info("wallet sweep", "expired", report.Expired, "settled", report.Settled, "pending", report.Pending,
				"failed", report.Failed, "stale", report.Stale, "reconciled", report.Reconciled, "mismatches", report.Mismatches,
				"errors", report.Errors)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Sweeper) batch() int {
	if w.Batch <= 0 {
		return 100
	}
	return w.Batch
}

// SweepOnce expires every reservation past its lease, drives every pending
// settlement whose backoff ended, reports the stale ones, then reconciles
// the tenants that are due.
func (w *Sweeper) SweepOnce(ctx context.Context) (SweepReport, error) {
	var report SweepReport
	s := w.Service
	if !s.Ready() || s.DB == nil {
		return report, errNotReady
	}
	n, err := w.expire(ctx)
	report.Expired = n
	if err != nil {
		return report, err
	}
	if err := w.settle(ctx, &report); err != nil {
		return report, err
	}
	stale, err := w.stale(ctx)
	report.Stale = stale
	if err != nil {
		return report, err
	}
	every := w.ReconcileEvery
	if every <= 0 {
		every = DefaultReconcileEvery
	}
	due, err := w.dueTenants(ctx, every)
	if err != nil {
		return report, err
	}
	for _, sc := range due {
		found, err := s.Reconcile(ctx, sc)
		if err != nil {
			report.Errors++
			s.log().Warn("wallet reconciliation failed", "tenant_id", sc.TenantID(), "error", err.Error())
			continue
		}
		report.Reconciled++
		report.Mismatches += found
	}
	return report, nil
}

// expire marks held reservations past their lease expired, a page at a
// time, each page in its own transaction. A reservation whose settlement
// waits for Kill Bill never expires: the settlement completes or fails
// first. A reservation another transaction holds is skipped until the next
// pass.
func (w *Sweeper) expire(ctx context.Context) (int, error) {
	s := w.Service
	batch := w.batch()
	total := 0
	for {
		n := 0
		err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
			now := s.now()
			rows, err := tx.Query(ctx, `SELECT brand_id, tenant_id, `+reservationColumns+` FROM reservations
WHERE status = 'held' AND NOT settlement_pending AND lease_expires_at <= $1
ORDER BY lease_expires_at LIMIT $2 FOR UPDATE SKIP LOCKED`, now, batch)
			if err != nil {
				return unavailable(err)
			}
			type due struct {
				sc scope.Scope
				r  Reservation
			}
			var list []due
			for rows.Next() {
				var d due
				r, err := scanReservation(rows, &d.sc.Brand, &d.sc.Tenant)
				if err != nil {
					rows.Close()
					return unavailable(err)
				}
				d.r = r
				list = append(list, d)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return unavailable(err)
			}
			call := Call{Actor: SweeperActor, Reason: "lease expired", CorrelationID: idgen.MustNewV7().String()}
			for _, d := range list {
				args, _ := d.sc.Bind(d.r.id, now)
				r, err := scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET status = 'expired', expired_at = $4, updated_at = $4
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+reservationColumns, args...))
				if err != nil {
					return unavailable(err)
				}
				details := map[string]any{"lease_expired_at": d.r.LeaseExpiresAt.UTC().Format(time.RFC3339)}
				if err := event(ctx, tx, d.sc, call, r, "expire", details); err != nil {
					return err
				}
				if err := record(ctx, tx, d.sc, call, "wallet.reservation.expired", EventReservationExpired, r, details); err != nil {
					return err
				}
			}
			n = len(list)
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < batch {
			return total, nil
		}
	}
}

// settle drives every pending settlement whose backoff ended and that no
// other worker holds, a page at a time. The relay drives a settlement as
// soon as it is queued. The sweeper drives it again whatever became of the
// relay's event, so a settlement is never left behind.
func (w *Sweeper) settle(ctx context.Context, report *SweepReport) error {
	s := w.Service
	for {
		list, err := s.claim(ctx, true, nil, w.batch())
		if err != nil {
			return err
		}
		for _, c := range list {
			call := relayCall(idgen.MustNewV7().String())
			_, result, err := s.drive(ctx, c.sc, call, c.r, c.st, c.estimate, c.attempts, c.token)
			switch {
			case result == outcomeFailed:
				report.Failed++
			case err != nil:
				report.Errors++
				s.log().Warn("pending settlement not driven", "reservation_id", c.r.ID, "error", err.Error())
			case result == outcomeSettled:
				report.Settled++
			case result == outcomeQueued:
				report.Pending++
			}
		}
		if len(list) < w.batch() || ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// stale reports each settlement that still waits for Kill Bill StaleAfter
// after it was asked for, once: a settlement_stale event in its history
// and the audit action wallet.settlement_stale. The reconciliation reports
// it as pending_settlement_stale until it completes.
func (w *Sweeper) stale(ctx context.Context) (int, error) {
	s := w.Service
	n := 0
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		now := s.now()
		rows, err := tx.Query(ctx, `UPDATE reservations SET stale_reported_at = $1
WHERE settlement_pending AND stale_reported_at IS NULL AND settle_requested_at <= $2
RETURNING brand_id, tenant_id, settle_requested_at, settle_attempts, `+reservationColumns, now, now.Add(-StaleAfter))
		if err != nil {
			return unavailable(err)
		}
		type item struct {
			sc          scope.Scope
			r           Reservation
			requestedAt time.Time
			attempts    int
		}
		var list []item
		for rows.Next() {
			var it item
			r, err := scanReservation(rows, &it.sc.Brand, &it.sc.Tenant, &it.requestedAt, &it.attempts)
			if err != nil {
				rows.Close()
				return unavailable(err)
			}
			it.r = r
			list = append(list, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return unavailable(err)
		}
		call := Call{Actor: SweeperActor, Reason: "settlement waits for Kill Bill", CorrelationID: idgen.MustNewV7().String()}
		for _, it := range list {
			details := map[string]any{"requested_at": it.requestedAt.UTC().Format(time.RFC3339), "attempts": it.attempts}
			if err := event(ctx, tx, it.sc, call, it.r, "settlement_stale", details); err != nil {
				return err
			}
			if err := record(ctx, tx, it.sc, call, ActionSettlementStale, "", it.r, details); err != nil {
				return err
			}
		}
		n = len(list)
		return nil
	})
	return n, err
}

// dueTenants returns the tenants of active brands, not closed, whose last
// reconciliation is older than every.
func (w *Sweeper) dueTenants(ctx context.Context, every time.Duration) ([]scope.Scope, error) {
	s := w.Service
	rows, err := s.DB.Query(ctx, `SELECT t.brand_id, t.id FROM tenants t
JOIN brands b ON b.id = t.brand_id AND b.status = 'active' AND b.applied_at IS NOT NULL
LEFT JOIN wallet_reconciliations r ON r.brand_id = t.brand_id AND r.tenant_id = t.id
WHERE t.status <> 'closed' AND (r.last_run_at IS NULL OR r.last_run_at <= $1)
ORDER BY r.last_run_at NULLS FIRST, t.id LIMIT 1000`, s.now().Add(-every))
	if err != nil {
		return nil, unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (scope.Scope, error) {
		var sc scope.Scope
		err := r.Scan(&sc.Brand, &sc.Tenant)
		return sc, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// finding is one thing Kill Bill holds that Core's records do not explain.
type finding struct {
	kind, ref string
	details   map[string]any
}

// Reconcile compares one tenant's wallet with Kill Bill and reports what
// does not match, as wallet.reconciliation_mismatch, once per finding. It
// never changes Kill Bill. A finding that no longer holds is forgotten, so
// it is reported again if it comes back. It returns the number of findings
// reported for the first time.
//
// It checks that every Kill Bill credit is a credit Core wrote and that
// every Core credit is in Kill Bill with its amount, that every settlement
// charge in Kill Bill is a settled reservation's and that every settled
// reservation's charge is in Kill Bill with its amount, that the usage Kill
// Bill holds is the usage Core sent, that the holds match their history,
// and it reports the settlements that wait too long, the usage the
// forwarder refused, and holds above the credit. It reads the unbilled
// amount before the credit.
func (s *Service) Reconcile(ctx context.Context, sc scope.Scope) (int, error) {
	if !s.Ready() || s.DB == nil {
		return 0, errNotReady
	}
	acct, err := s.Billing.TenantAccount(ctx, s.DB, sc)
	if err != nil {
		return 0, err
	}
	unbilled, err := acct.UnbilledAmounts(ctx)
	if err != nil {
		return 0, err
	}
	snap, err := acct.Snapshot(ctx)
	if err != nil {
		return 0, err
	}
	invoices, err := acct.Invoices(ctx)
	if err != nil {
		return 0, err
	}
	core, err := billing.LoadCoreCredits(ctx, s.DB, sc)
	if err != nil {
		return 0, err
	}
	charges, err := settledCharges(ctx, s.DB, sc)
	if err != nil {
		return 0, err
	}
	held, err := heldMinor(ctx, s.DB, sc)
	if err != nil {
		return 0, err
	}
	var found []finding
	seenCredits := map[string]bool{}
	seenCharges := map[string]bool{}
	for _, in := range invoices {
		for _, it := range in.Items {
			amount, err := billing.ToMinor(it.Amount, snap.Currency)
			if err != nil {
				return 0, billing.KBError(err)
			}
			amount = max(amount, -amount)
			id := strings.ToLower(it.InvoiceItemID)
			switch it.ItemType {
			case "CREDIT_ADJ":
				want, ok := core.Items[id]
				switch {
				case ok:
					seenCredits[id] = true
					if want != amount {
						found = append(found, finding{MismatchMissingCredit, id, map[string]any{"killbill_amount_minor": amount, "core_amount_minor": want}})
					}
				case core.Pending[it.Description]:
				default:
					found = append(found, finding{MismatchUnknownCredit, id, map[string]any{"killbill_amount_minor": amount, "killbill_invoice_id": it.InvoiceID}})
				}
			case "EXTERNAL_CHARGE":
				if !strings.HasPrefix(it.Description, ChargeDescriptionPrefix) {
					continue
				}
				want, ok := charges[id]
				switch {
				case !ok:
					found = append(found, finding{MismatchUnknownCharge, id, map[string]any{"killbill_amount_minor": amount}})
				case want != amount:
					seenCharges[id] = true
					found = append(found, finding{MismatchMissingCharge, id, map[string]any{"killbill_amount_minor": amount, "core_amount_minor": want}})
				default:
					seenCharges[id] = true
				}
			}
		}
	}
	for id, amount := range core.Items {
		if !seenCredits[id] {
			found = append(found, finding{MismatchMissingCredit, id, map[string]any{"core_amount_minor": amount}})
		}
	}
	for id, amount := range charges {
		if !seenCharges[id] {
			found = append(found, finding{MismatchMissingCharge, id, map[string]any{"core_amount_minor": amount}})
		}
	}
	usage, err := s.usageFindings(ctx, sc, acct, invoices)
	if err != nil {
		return 0, err
	}
	found = append(found, usage...)
	holds, err := heldFindings(ctx, s.DB, sc)
	if err != nil {
		return 0, err
	}
	found = append(found, holds...)
	stale, err := staleFindings(ctx, s.DB, sc, s.now())
	if err != nil {
		return 0, err
	}
	found = append(found, stale...)
	rejected, err := metering.RejectedRollups(ctx, s.DB, sc)
	if err != nil {
		return 0, err
	}
	for _, r := range rejected {
		details := map[string]any{"rollup_id": r.ID, "reason": r.RejectReason, "killbill_unit": r.KillBillUnit, "quantity_micro": r.QuantityMicro,
			"events": r.Events, "period_start": r.PeriodStart.Format(time.RFC3339Nano), "killbill_tracking_id": r.TrackingID}
		if r.Subscription != "" {
			details["killbill_subscription_id"] = r.Subscription
		}
		found = append(found, finding{MismatchRejectedUsage, r.ID, details})
	}
	overdraft := acct.Brand().Manifest.Billing.Overdraft()
	if held > snap.CreditMinor-unbilled.UsageMinor+overdraft {
		found = append(found, finding{MismatchOverReserved, "wallet", map[string]any{"killbill_credit_minor": snap.CreditMinor,
			"unbilled_usage_minor": unbilled.UsageMinor, "reserved_minor": held, "overdraft_minor": overdraft, "informational": true}})
	}
	return s.recordFindings(ctx, sc, found)
}

// inWindow reports whether a subscription's usage is reconciled: it is not
// cancelled or expired, or it stopped after since. A stop date Core cannot
// read keeps it in.
func inWindow(sub billing.Subscription, since time.Time) bool {
	if sub.State != "CANCELLED" && sub.State != "EXPIRED" {
		return true
	}
	for _, d := range []string{sub.CancelledDate, sub.BillingEndDate} {
		if d == "" {
			continue
		}
		if t, ok := parseDay(d); ok {
			return !t.Before(since)
		}
	}
	return true
}

// parseDay reads a Kill Bill date or date and time.
func parseDay(v string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z07:00", time.DateOnly} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// usageFindings compares the usage Kill Bill holds with the usage Core
// forwarded in rollups and settled from reservations, two ways.
//
// Per tracking ID, where Kill Bill exposes it: an invoice lists the
// tracking IDs of the usage it bills. One Core never sent is
// unknown_usage. One Core sent, for a subscription whose invoiced usage
// period covers its record date, that no invoice lists is missing_usage.
// So a missing and an unknown record of one month are reported apart.
//
// Per subscription, unit and month, from the month of the brand's backfill
// window less two months to the current one, for every subscription not
// cancelled before three months ago: Kill Bill's usage reads carry no
// tracking ID, so the totals are compared.
func (s *Service) usageFindings(ctx context.Context, sc scope.Scope, acct *billing.TenantAccount, invoices []killbill.Invoice) ([]finding, error) {
	subs, err := acct.Subscriptions(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	first := monthStart(now.Add(-acct.Brand().Manifest.Billing.Backfill())).AddDate(0, -2, 0)
	this := monthStart(now)
	since := now.AddDate(0, -3, 0)
	var out []finding
	for from := first; !from.After(this); from = from.AddDate(0, 1, 0) {
		to := from.AddDate(0, 1, 0)
		core, err := coreUsage(ctx, s.DB, sc, from, to)
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			if !inWindow(sub, since) {
				continue
			}
			billed, err := acct.BilledUsage(ctx, sub.ID, from, to)
			if err != nil {
				return nil, err
			}
			units := map[string]int64{}
			for _, u := range billed.Units {
				units[u.Unit] += u.QuantityMicro
			}
			for key := range core {
				if _, ok := units[key.unit]; !ok && key.sub == strings.ToLower(sub.ID) {
					units[key.unit] = 0
				}
			}
			for unit, kb := range units {
				want := core[usageKey{strings.ToLower(sub.ID), unit}]
				if kb == want {
					continue
				}
				kind := MismatchUnknownUsage
				if kb < want {
					kind = MismatchMissingUsage
				}
				out = append(out, finding{kind, sub.ID + ":" + unit + ":" + from.Format("2006-01"), map[string]any{
					"killbill_subscription_id": sub.ID, "killbill_unit": unit, "month": from.Format("2006-01"),
					"killbill_quantity_micro": kb, "core_quantity_micro": want}})
			}
		}
	}
	tracked, err := s.trackingFindings(ctx, sc, invoices)
	if err != nil {
		return nil, err
	}
	return append(out, tracked...), nil
}

// coreTracking is a tracking ID Core used. sent is true when Core holds
// Kill Bill's acceptance of it: a forwarded rollup or a settled usage
// settlement.
type coreTracking struct {
	sub      string
	recordAt time.Time
	quantity int64
	sent     bool
}

// span is a period of usage an invoice bills for a subscription.
type span struct{ from, to time.Time }

// trackingFindings reports the per tracking ID findings of usageFindings.
func (s *Service) trackingFindings(ctx context.Context, sc scope.Scope, invoices []killbill.Invoice) ([]finding, error) {
	invoiced := map[string]string{}
	spans := map[string][]span{}
	for _, in := range invoices {
		for _, id := range in.TrackingIDs {
			invoiced[id] = in.InvoiceID
		}
		for _, it := range in.Items {
			if it.ItemType != "USAGE" || it.SubscriptionID == "" {
				continue
			}
			from, ok1 := parseDay(it.StartDate)
			to, ok2 := parseDay(it.EndDate)
			if ok1 && ok2 {
				sub := strings.ToLower(it.SubscriptionID)
				spans[sub] = append(spans[sub], span{from, to})
			}
		}
	}
	if len(invoiced) == 0 && len(spans) == 0 {
		return nil, nil
	}
	known, err := coreTrackingIDs(ctx, s.DB, sc)
	if err != nil {
		return nil, err
	}
	var out []finding
	for id, invoice := range invoiced {
		if _, ok := known[id]; !ok {
			out = append(out, finding{MismatchUnknownUsage, "tracking:" + id, map[string]any{"killbill_tracking_id": id, "killbill_invoice_id": invoice}})
		}
	}
	for id, c := range known {
		if !c.sent || invoiced[id] != "" {
			continue
		}
		for _, sp := range spans[c.sub] {
			if !c.recordAt.Before(sp.from) && c.recordAt.Before(sp.to) {
				out = append(out, finding{MismatchMissingUsage, "tracking:" + id, map[string]any{"killbill_tracking_id": id,
					"killbill_subscription_id": c.sub, "record_at": c.recordAt.UTC().Format(time.RFC3339), "core_quantity_micro": c.quantity}})
				break
			}
		}
	}
	return out, nil
}

// coreTrackingIDs returns every tracking ID Core used for the tenant: its
// rollups, its settled usage settlements, and the usage settlements that
// are pending or whose earlier send may have reached Kill Bill.
func coreTrackingIDs(ctx context.Context, q Querier, sc scope.Scope) (map[string]coreTracking, error) {
	args, err := sc.Bind()
	if err != nil {
		return nil, err
	}
	out := map[string]coreTracking{}
	rows, err := q.Query(ctx, `SELECT tracking_id, lower(coalesce(subscription_id::text, '')), record_at, quantity_micro, status = 'forwarded'
FROM usage_rollups WHERE brand_id = $1 AND tenant_id = $2`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	for rows.Next() {
		var id string
		var c coreTracking
		if err := rows.Scan(&id, &c.sub, &c.recordAt, &c.quantity, &c.sent); err != nil {
			rows.Close()
			return nil, unavailable(err)
		}
		out[id] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	rows, err = q.Query(ctx, `SELECT external_ref, attempt, lower(coalesce(killbill_subscription_id::text, '')),
       coalesce((sent_settlement->>'requested_at')::timestamptz, created_at),
       coalesce((sent_settlement->'settlement'->'usage'->>'quantity_micro')::bigint, 0), status = 'settled'
FROM reservations WHERE brand_id = $1 AND tenant_id = $2 AND sent_settlement->'settlement' ? 'usage'`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ref     string
			attempt int
			c       coreTracking
		)
		if err := rows.Scan(&ref, &attempt, &c.sub, &c.recordAt, &c.quantity, &c.sent); err != nil {
			return nil, unavailable(err)
		}
		out[UsageTrackingID(sc, ref, attempt)] = c
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

type usageKey struct{ sub, unit string }

// coreUsage sums the usage Core sent to Kill Bill, by subscription and
// unit, with a record date from from, inclusive, to to, exclusive: the
// forwarded rollups and the usage settlements.
func coreUsage(ctx context.Context, q Querier, sc scope.Scope, from, to time.Time) (map[usageKey]int64, error) {
	args, err := sc.Bind(from, to)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT lower(subscription_id::text), killbill_unit, sum(quantity_micro)::bigint FROM usage_rollups
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'forwarded' AND record_at >= $3 AND record_at < $4
GROUP BY 1, 2
UNION ALL
SELECT lower(killbill_subscription_id::text), sent_settlement->'settlement'->'usage'->>'unit',
       sum((sent_settlement->'settlement'->'usage'->>'quantity_micro')::bigint)::bigint FROM reservations
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'settled' AND killbill_ref_type = 'usage_record'
  AND (sent_settlement->>'requested_at')::timestamptz >= $3 AND (sent_settlement->>'requested_at')::timestamptz < $4
GROUP BY 1, 2`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	out := map[usageKey]int64{}
	for rows.Next() {
		var k usageKey
		var n int64
		if err := rows.Scan(&k.sub, &k.unit, &n); err != nil {
			return nil, unavailable(err)
		}
		out[k] += n
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// staleFindings reports each settlement that waits for Kill Bill longer
// than StaleAfter.
func staleFindings(ctx context.Context, q Querier, sc scope.Scope, now time.Time) ([]finding, error) {
	args, err := sc.Bind(now.Add(-StaleAfter))
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT id, settle_requested_at, settle_attempts, amount_minor FROM reservations
WHERE brand_id = $1 AND tenant_id = $2 AND settlement_pending AND settle_requested_at <= $3`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var out []finding
	for rows.Next() {
		var (
			id          idgen.UUID
			requestedAt time.Time
			attempts    int
			amount      int64
		)
		if err := rows.Scan(&id, &requestedAt, &attempts, &amount); err != nil {
			return nil, unavailable(err)
		}
		rid := idgen.Format(idgen.Reservation, id)
		out = append(out, finding{MismatchPendingSettlementStale, rid, map[string]any{"reservation_id": rid,
			"requested_at": requestedAt.UTC().Format(time.RFC3339), "attempts": attempts, "reserved_minor": amount}})
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// heldFindings reports each held reservation whose amount is not the hold
// its latest history event records, or whose latest event ended it, so the
// held total always matches the reservation rows and their history.
func heldFindings(ctx context.Context, q Querier, sc scope.Scope) ([]finding, error) {
	args, err := sc.Bind()
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT r.id, r.amount_minor, e.amount_minor, coalesce(e.kind, '') FROM reservations r
LEFT JOIN LATERAL (SELECT amount_minor, kind FROM reservation_events ev
    WHERE ev.brand_id = r.brand_id AND ev.tenant_id = r.tenant_id AND ev.reservation_id = r.id
    ORDER BY ev.occurred_at DESC, ev.id DESC LIMIT 1) e ON true
WHERE r.brand_id = $1 AND r.tenant_id = $2 AND r.status = 'held'`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var out []finding
	for rows.Next() {
		var (
			id     idgen.UUID
			amount int64
			last   *int64
			kind   string
		)
		if err := rows.Scan(&id, &amount, &last, &kind); err != nil {
			return nil, unavailable(err)
		}
		ended := kind == "settle" || kind == "release" || kind == "expire"
		if last != nil && *last == amount && !ended {
			continue
		}
		details := map[string]any{"reservation_id": idgen.Format(idgen.Reservation, id), "reserved_minor": amount, "last_event": kind}
		if last != nil {
			details["history_minor"] = *last
		}
		out = append(out, finding{MismatchHeldMismatch, idgen.Format(idgen.Reservation, id), details})
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// settledCharges returns the external charges of the tenant's settled
// reservations by Kill Bill item ID, with their amounts.
func settledCharges(ctx context.Context, q Querier, sc scope.Scope) (map[string]int64, error) {
	args, err := sc.Bind()
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT lower(killbill_ref), coalesce(settled_minor, 0) FROM reservations
WHERE brand_id = $1 AND tenant_id = $2 AND status = 'settled' AND killbill_ref_type = 'external_charge'`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var amount int64
		if err := rows.Scan(&id, &amount); err != nil {
			return nil, unavailable(err)
		}
		out[id] = amount
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// recordFindings stores the findings of one reconciliation and reports the
// new ones, in one transaction.
func (s *Service) recordFindings(ctx context.Context, sc scope.Scope, found []finding) (int, error) {
	now := s.now()
	reported := 0
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		call := Call{Actor: SweeperActor, Reason: "reconciliation", CorrelationID: idgen.MustNewV7().String()}
		bumped := false
		kinds, refs := make([]string, 0, len(found)), make([]string, 0, len(found))
		for _, f := range found {
			kinds, refs = append(kinds, f.kind), append(refs, f.ref)
			details, err := json.Marshal(f.details)
			if err != nil {
				return err
			}
			args, _ := sc.Bind(f.kind, f.ref, details, now)
			tag, err := tx.Exec(ctx, `INSERT INTO wallet_reconciliation_findings (brand_id, tenant_id, kind, killbill_ref, details, first_seen_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)
ON CONFLICT (brand_id, tenant_id, kind, killbill_ref) DO NOTHING`, args...)
			if err != nil {
				return unavailable(err)
			}
			if tag.RowsAffected() == 0 {
				if _, err := tx.Exec(ctx, `UPDATE wallet_reconciliation_findings SET last_seen_at = $5, details = $6::jsonb
WHERE brand_id = $1 AND tenant_id = $2 AND kind = $3 AND killbill_ref = $4`, append(args[:4], now, details)...); err != nil {
					return unavailable(err)
				}
				continue
			}
			if creditKinds[f.kind] && !bumped {
				// The Kill Bill credit changed in a way Core did not make.
				if err := billing.BumpWalletVersion(ctx, tx, sc); err != nil {
					return err
				}
				bumped = true
			}
			payload := map[string]any{"kind": f.kind, "killbill_ref": f.ref}
			for k, v := range f.details {
				payload[k] = v
			}
			if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: EventReconciliationMismatch, Reason: call.Reason,
				Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: payload}); err != nil {
				return err
			}
			if err := writeEvent(ctx, tx, sc, call, EventReconciliationMismatch, events.Aggregate{Type: "tenant", ID: sc.TenantID()}, payload); err != nil {
				return err
			}
			reported++
		}
		args, _ := sc.Bind(kinds, refs)
		if _, err := tx.Exec(ctx, `DELETE FROM wallet_reconciliation_findings f WHERE f.brand_id = $1 AND f.tenant_id = $2
  AND NOT EXISTS (SELECT 1 FROM unnest($3::text[], $4::text[]) AS k(kind, ref) WHERE k.kind = f.kind AND k.ref = f.killbill_ref)`, args...); err != nil {
			return unavailable(err)
		}
		args, _ = sc.Bind(now)
		if _, err := tx.Exec(ctx, `INSERT INTO wallet_reconciliations (brand_id, tenant_id, last_run_at) VALUES ($1, $2, $3)
ON CONFLICT (brand_id, tenant_id) DO UPDATE SET last_run_at = EXCLUDED.last_run_at`, args...); err != nil {
			return unavailable(err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return reported, nil
}

// RunRelay runs the settlement relay until ctx ends: it claims only
// wallet.settlement.requested events, an internal type no other relay
// claims.
func (s *Service) RunRelay(ctx context.Context, reader outbox.Reader, interval time.Duration) {
	outbox.NewRelay("wallet-settlement", reader, s.RelayHandler(), 0, 0, s.Log).Run(ctx, interval)
}
