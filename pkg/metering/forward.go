package metering

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/billing"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// ForwarderActor is the audit actor of the forwarder.
var ForwarderActor = events.Actor{Type: events.ActorSystem}

// Forwarding limits.
const (
	// rollupBatch is the most events one rollup pass of a tenant reads.
	// With MaxQuantityMicro per event, their sum fits an int64.
	rollupBatch = 5000
	// sendBatch is the most rollups one pass claims at a time. A small
	// batch keeps the claims of the rollups not yet sent short.
	sendBatch = 10
	// rollupClaim is how long a forwarder holds a rollup. It renews the
	// claim before each send, so the claim only has to outlast one Kill
	// Bill call.
	rollupClaim = 2 * time.Minute
	// Retry backoff of a rollup Kill Bill did not take: 10 seconds after
	// the first failure, doubling up to a day.
	rollupBackoffFirst = 10 * time.Second
	rollupBackoffMax   = 24 * time.Hour
	// partitionMonthsAhead is how many months after the current one get
	// their usage_events partition on each partition sweep.
	partitionMonthsAhead = 3
)

// Reasons a rollup is rejected instead of sent: Kill Bill 0.24.22 records
// usage dated before the subscription started, and usage on a cancelled
// subscription, and never bills either.
const (
	RejectBeforeStart = "before_subscription_start"
	RejectCancelled   = "subscription_cancelled"
)

// Rollup statuses.
const (
	RollupPending   = "pending"
	RollupForwarded = "forwarded"
	RollupRejected  = "rejected"
	RollupDiscarded = "discarded"
)

// EventRollupRejected is the audit action of a rollup Core refused to send.
const EventRollupRejected = "metering.rollup_rejected"

// ForwardReport counts what one pass did.
type ForwardReport struct {
	RolledUp  int
	Forwarded int
	// Duplicates counts rollups Kill Bill already held: an earlier attempt
	// recorded them and lost the answer.
	Duplicates int
	Failed     int
	// Rejected counts rollups refused because Kill Bill would not bill
	// them.
	Rejected int
	// Skipped counts rollups another forwarder claimed in the meantime.
	Skipped  int
	Backlogs int
}

// TrackingID is the Kill Bill tracking ID of a rollup: a digest of the
// tenant, the Kill Bill unit, the requested subscription, the day and the
// IDs of its events. The same set of events always gets the same tracking
// ID, so Kill Bill records it once however often it is sent.
func TrackingID(sc scope.Scope, unit, subscription string, day time.Time, eventIDs []idgen.UUID) string {
	ids := make([]string, len(eventIDs))
	for i, id := range eventIDs {
		ids[i] = id.String()
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, part := range append([]string{"zs-core.usage-rollup.v1", sc.Brand.String(), sc.Tenant.String(), unit, subscription,
		day.UTC().Format(time.DateOnly)}, ids...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "zs-usg-" + hex.EncodeToString(h.Sum(nil))[:40]
}

// EventSetFingerprint is the durable evidence of which events a protocol 1
// rollup holds: the sha256 of a fixed domain, the scope, the rollup key and
// the sorted event ids, each followed by a zero byte. Unlike TrackingID it
// keeps all 64 hex digits, as migration 22 requires.
func EventSetFingerprint(sc scope.Scope, unit, subscription string, day time.Time, eventIDs []idgen.UUID) string {
	ids := make([]string, len(eventIDs))
	for i, id := range eventIDs {
		ids[i] = id.String()
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, part := range append([]string{"zs-core.usage-event-set.v1", sc.Brand.String(), sc.Tenant.String(), unit, subscription,
		day.UTC().Format(time.DateOnly)}, ids...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// LegacyUnrolled counts the tenant's raw events accepted before dispatch
// protocol 1 that are in no rollup. The guard keeps them read-only and the
// forwarder never rolls them up, so they stay unbilled until staff decide
// what to do with them; this count keeps that visible.
func (s *Service) LegacyUnrolled(ctx context.Context, sc scope.Scope) (int, error) {
	args, err := sc.Bind()
	if err != nil {
		return 0, err
	}
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM usage_events
WHERE brand_id = $1 AND tenant_id = $2 AND rollup_id IS NULL AND reservation_id IS NULL AND acceptance_protocol = 0`, args...).Scan(&n); err != nil {
		return 0, unavailable(err)
	}
	return n, nil
}

// Forward runs one pass over every tenant: it rolls up the events not yet
// in a rollup, sends every due rollup to Kill Bill and emits
// metering.forward_backlog for tenants whose events waited too long. The
// partition sweeper, not the forwarder, creates the usage_events
// partitions.
func (s *Service) Forward(ctx context.Context) (ForwardReport, error) {
	var report ForwardReport
	if s.DB == nil || !s.Billing.Ready() {
		return report, zserrors.New(zserrors.CodeNotReady, "Forwarding needs the database and the Kill Bill admin settings.")
	}
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT brand_id, tenant_id FROM usage_events WHERE rollup_id IS NULL AND reservation_id IS NULL AND acceptance_protocol = 1`)
	if err != nil {
		return report, unavailable(err)
	}
	var tenants []scope.Scope
	for rows.Next() {
		var sc scope.Scope
		if err := rows.Scan(&sc.Brand, &sc.Tenant); err != nil {
			rows.Close()
			return report, unavailable(err)
		}
		tenants = append(tenants, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return report, unavailable(err)
	}
	for _, sc := range tenants {
		n, err := s.rollUp(ctx, sc)
		if err != nil {
			return report, err
		}
		report.RolledUp += n
	}
	if err := s.sendDue(ctx, nil, &report); err != nil {
		return report, err
	}
	n, err := s.backlogAlerts(ctx)
	report.Backlogs = n
	return report, err
}

// ForwardTenant rolls up and sends one tenant's usage now. The outbox relay
// of metering.usage.recorded calls it.
func (s *Service) ForwardTenant(ctx context.Context, sc scope.Scope) (ForwardReport, error) {
	var report ForwardReport
	if s.DB == nil || !s.Billing.Ready() {
		return report, zserrors.New(zserrors.CodeNotReady, "Forwarding needs the database and the Kill Bill admin settings.")
	}
	n, err := s.rollUp(ctx, sc)
	if err != nil {
		return report, err
	}
	report.RolledUp = n
	return report, s.sendDue(ctx, &sc, &report)
}

// EnsurePartitions creates the usage_events partitions of the month of now
// and the next three, under an advisory lock so instances do not race.
// Creating a partition that exists does nothing. When another instance
// holds the lock, it returns at once: that instance does the work.
func EnsurePartitions(ctx context.Context, db DB, now time.Time) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('zs-core.usage-partitions:' || current_schema(), 0))`).Scan(&locked); err != nil {
			return unavailable(err)
		}
		if !locked {
			return nil
		}
		month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		for i := 0; i <= partitionMonthsAhead; i++ {
			if _, err := tx.Exec(ctx, `SELECT usage_events_ensure_partition($1::date)`, month.AddDate(0, i, 0).Format(time.DateOnly)); err != nil {
				return unavailable(err)
			}
		}
		return nil
	})
}

// PartitionSweeper keeps the usage_events partitions ahead of time. It
// needs only the database, so it runs whether or not Kill Bill is
// configured.
type PartitionSweeper struct {
	DB  DB
	Log *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

// Run ensures the partitions every interval until ctx ends.
func (p *PartitionSweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		now := time.Now().UTC()
		if p.Now != nil {
			now = p.Now().UTC()
		}
		if err := EnsurePartitions(ctx, p.DB, now); err != nil && p.Log != nil {
			p.Log.Error("usage partition sweep failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type pendingEvent struct {
	id           idgen.UUID
	occurredAt   time.Time
	unit         string
	subscription string
	quantity     int64
}

// rollUp puts the tenant's events that are in no rollup yet into rollups,
// one per Kill Bill unit, requested subscription and day, in one
// transaction. Events locked by another forwarder are skipped.
func (s *Service) rollUp(ctx context.Context, sc scope.Scope) (int, error) {
	args, err := sc.Bind(rollupBatch)
	if err != nil {
		return 0, err
	}
	created := 0
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, occurred_at, killbill_unit, coalesce(subscription_id::text, ''), quantity_micro
FROM usage_events WHERE brand_id = $1 AND tenant_id = $2 AND rollup_id IS NULL AND reservation_id IS NULL AND acceptance_protocol = 1
ORDER BY occurred_at, id LIMIT $3 FOR UPDATE SKIP LOCKED`, args...)
		if err != nil {
			return unavailable(err)
		}
		evs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pendingEvent, error) {
			var e pendingEvent
			err := r.Scan(&e.id, &e.occurredAt, &e.unit, &e.subscription, &e.quantity)
			return e, err
		})
		if err != nil {
			return unavailable(err)
		}
		type key struct{ unit, subscription, day string }
		groups := map[key][]pendingEvent{}
		var order []key
		for _, e := range evs {
			k := key{e.unit, e.subscription, e.occurredAt.UTC().Format(time.DateOnly)}
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], e)
		}
		for _, k := range order {
			group := groups[k]
			ids := make([]idgen.UUID, len(group))
			var total int64
			var latest, earliest time.Time
			for i, e := range group {
				ids[i] = e.id
				if e.occurredAt.After(latest) {
					latest = e.occurredAt
				}
				if earliest.IsZero() || e.occurredAt.Before(earliest) {
					earliest = e.occurredAt
				}
				if total > 1<<63-1-e.quantity {
					return errOverflow
				}
				total += e.quantity
			}
			day, _ := time.Parse(time.DateOnly, k.day)
			rollupID := idgen.MustNewV7()
			var sub any
			if k.subscription != "" {
				sub = k.subscription
			}
			insert, _ := sc.Bind(rollupID, k.unit, sub, k.day, total, len(group), TrackingID(sc, k.unit, k.subscription, day, ids), s.now(), latest, earliest,
				EventSetFingerprint(sc, k.unit, k.subscription, day, ids))
			if _, err := tx.Exec(ctx, `INSERT INTO usage_rollups (brand_id, tenant_id, id, killbill_unit, requested_subscription_id,
    subscription_id, day, quantity_micro, events, tracking_id, next_attempt_at, record_at, period_start,
    send_protocol, send_provenance, event_set_fingerprint)
VALUES ($1, $2, $3, $4, $5::uuid, $5::uuid, $6::date, $7, $8, $9, $10, $11, $12, 1, 'never_attempted', $13)`, insert...); err != nil {
				return unavailable(err)
			}
			strIDs := make([]string, len(ids))
			for i, id := range ids {
				strIDs[i] = id.String()
			}
			update, _ := sc.Bind(rollupID, strIDs)
			if _, err := tx.Exec(ctx, `UPDATE usage_events SET rollup_id = $3
WHERE brand_id = $1 AND tenant_id = $2 AND id = ANY($4::uuid[]) AND rollup_id IS NULL`, update...); err != nil {
				return unavailable(err)
			}
			created++
		}
		return nil
	})
	return created, err
}

type rollup struct {
	id                      idgen.UUID
	sc                      scope.Scope
	unit, trackingID        string
	requested, subscription string
	day, recordAt           time.Time
	periodStart             time.Time
	quantity                int64
	events                  int
	attempts                int
	token                   []byte
}

func newClaimToken() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b
}

// sendDue claims due rollups, of one tenant when only is set, a batch at a
// time under a claim token, and sends each to Kill Bill. Before each send
// it renews its claim. A rollup whose claim it lost, because the claim ran
// out and another forwarder took the rollup, is skipped and never sent.
func (s *Service) sendDue(ctx context.Context, only *scope.Scope, report *ForwardReport) error {
	for {
		var brandID, tenantID any
		if only != nil {
			brandID, tenantID = only.Brand, only.Tenant
		}
		token := newClaimToken()
		rows, err := s.DB.Query(ctx, `UPDATE usage_rollups r SET claimed_until = $1 + make_interval(secs => $2), claim_token = $6
FROM (SELECT id FROM usage_rollups WHERE status = 'pending' AND next_attempt_at <= $1
        AND (claimed_until IS NULL OR claimed_until <= $1)
        AND ($3::uuid IS NULL OR (brand_id = $3 AND tenant_id = $4))
      ORDER BY next_attempt_at, id LIMIT $5 FOR UPDATE SKIP LOCKED) due
WHERE r.id = due.id
RETURNING r.id, r.brand_id, r.tenant_id, r.killbill_unit, r.tracking_id, coalesce(r.requested_subscription_id::text, ''),
          coalesce(r.subscription_id::text, ''), r.day, r.record_at, r.period_start, r.quantity_micro, r.events, r.attempts`,
			s.now(), rollupClaim.Seconds(), brandID, tenantID, sendBatch, token)
		if err != nil {
			return unavailable(err)
		}
		due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rollup, error) {
			ro := rollup{token: token}
			err := r.Scan(&ro.id, &ro.sc.Brand, &ro.sc.Tenant, &ro.unit, &ro.trackingID, &ro.requested, &ro.subscription, &ro.day,
				&ro.recordAt, &ro.periodStart, &ro.quantity, &ro.events, &ro.attempts)
			return ro, err
		})
		if err != nil {
			return unavailable(err)
		}
		sort.Slice(due, func(i, j int) bool { return due[i].id.String() < due[j].id.String() })
		for _, ro := range due {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			held, err := s.renewClaim(ctx, ro)
			if err != nil {
				return err
			}
			if !held {
				report.Skipped++
				continue
			}
			outcome, err := s.send(ctx, ro)
			switch {
			case err != nil:
				report.Failed++
				s.log().WarnContext(ctx, "usage rollup not forwarded", "tenant_id", ro.sc.TenantID(), "tracking_id", ro.trackingID,
					"error", err.Error())
				if err := s.retryLater(ctx, ro, err); err != nil {
					return err
				}
			case outcome == sendRejected:
				report.Rejected++
			case outcome == sendLost:
				report.Skipped++
			case outcome == sendDuplicate:
				report.Duplicates++
				report.Forwarded++
			default:
				report.Forwarded++
			}
		}
		if len(due) < sendBatch {
			return nil
		}
	}
}

// renewClaim extends the claim of a rollup before it is sent. It reports
// false when the claim token no longer holds the rollup.
func (s *Service) renewClaim(ctx context.Context, ro rollup) (bool, error) {
	args, _ := ro.sc.Bind(ro.id, s.now().Add(rollupClaim), ro.token)
	tag, err := s.DB.Exec(ctx, `UPDATE usage_rollups SET claimed_until = $4
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $5`, args...)
	if err != nil {
		return false, unavailable(err)
	}
	return tag.RowsAffected() == 1, nil
}

type sendOutcome int

const (
	sendForwarded sendOutcome = iota
	sendDuplicate
	sendRejected
	sendLost
)

// send writes one rollup to Kill Bill and records it. The subscription
// must take usage dated at the start of the rollup's period: a rollup
// whose period starts before the subscription started, or whose
// subscription is cancelled, is rejected instead, because Kill Bill would
// record it and never bill it.
func (s *Service) send(ctx context.Context, ro rollup) (sendOutcome, error) {
	acct, err := s.Billing.TenantAccount(ctx, s.DB, ro.sc)
	if err != nil {
		return 0, err
	}
	target, err := acct.UsageSubscriptionAt(ctx, ro.subscription, ro.unit, ro.periodStart)
	switch {
	case errors.Is(err, billing.ErrUsageBeforeStart):
		return s.reject(ctx, ro, RejectBeforeStart)
	case errors.Is(err, billing.ErrSubscriptionCancelled):
		return s.reject(ctx, ro, RejectCancelled)
	case err != nil:
		return 0, err
	}
	if ro.subscription == "" {
		// The subscription is chosen once and kept, so every retry of the
		// tracking ID goes to the same subscription.
		args, _ := ro.sc.Bind(ro.id, target.Subscription.SubscriptionID, ro.token)
		tag, err := s.DB.Exec(ctx, `UPDATE usage_rollups SET subscription_id = $4::uuid
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND subscription_id IS NULL AND claim_token = $5`, args...)
		if err != nil {
			return 0, unavailable(err)
		}
		if tag.RowsAffected() == 0 {
			return sendLost, nil
		}
		ro.subscription = target.Subscription.SubscriptionID
	}
	call := billing.Call{Actor: ForwarderActor, Reason: "usage forward", CorrelationID: ro.trackingID}
	duplicate, err := acct.RecordUsage(ctx, call, ro.subscription, ro.unit, ro.trackingID,
		[]billing.UsageAmount{{At: ro.recordAt, AmountMicro: ro.quantity}})
	if err != nil {
		return 0, err
	}
	now := s.now()
	err = pgx.BeginFunc(context.WithoutCancel(ctx), s.DB, func(tx pgx.Tx) error {
		ctx := context.WithoutCancel(ctx)
		args, _ := ro.sc.Bind(ro.id, now, ro.token)
		tag, err := tx.Exec(ctx, `UPDATE usage_rollups SET status = 'forwarded', forwarded_at = $4, claimed_until = NULL, claim_token = NULL, last_error = NULL
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $5`, args...)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 0 {
			// Another forwarder took the rollup after this one sent it. It
			// sends the same tracking ID, which Kill Bill records once, and
			// records the rollup itself.
			return nil
		}
		ev, _ := ro.sc.Bind(ro.id, ro.trackingID, now)
		if _, err := tx.Exec(ctx, `UPDATE usage_events SET forwarded_at = $5, killbill_tracking_id = $4
WHERE brand_id = $1 AND tenant_id = $2 AND rollup_id = $3`, ev...); err != nil {
			return unavailable(err)
		}
		// Kill Bill's unbilled amount for the tenant just grew.
		if err := billing.BumpWalletVersion(ctx, tx, ro.sc); err != nil {
			return err
		}
		_, err = audit.Write(ctx, tx, ro.sc, audit.Entry{Actor: ForwarderActor, Action: "metering.usage.forwarded", Outcome: audit.OutcomeSuccess,
			CorrelationID: ro.trackingID, Details: map[string]any{"killbill_subscription_id": ro.subscription,
				"killbill_tracking_id": ro.trackingID, "killbill_unit": ro.unit, "day": ro.day.UTC().Format(time.DateOnly),
				"quantity_micro": ro.quantity, "duplicate": duplicate}})
		return err
	})
	if err != nil {
		return 0, err
	}
	if duplicate {
		return sendDuplicate, nil
	}
	return sendForwarded, nil
}

// reject records a rollup Kill Bill would not bill, and audits it as
// metering.rollup_rejected. The reconciliation reports it as
// rejected_usage until staff reassign or discard it.
func (s *Service) reject(ctx context.Context, ro rollup, reason string) (sendOutcome, error) {
	ctx = context.WithoutCancel(ctx)
	lost := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		args, _ := ro.sc.Bind(ro.id, s.now(), reason, ro.token)
		tag, err := tx.Exec(ctx, `UPDATE usage_rollups SET status = 'rejected', reject_reason = $5, rejected_at = $4,
    claimed_until = NULL, claim_token = NULL WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $6`, args...)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 0 {
			lost = true
			return nil
		}
		details := map[string]any{"rollup_id": idgen.Format(idgen.Rollup, ro.id), "reason": reason, "killbill_unit": ro.unit,
			"quantity_micro": ro.quantity, "events": ro.events, "period_start": ro.periodStart.UTC().Format(time.RFC3339Nano),
			"killbill_tracking_id": ro.trackingID}
		if ro.subscription != "" {
			details["killbill_subscription_id"] = ro.subscription
		}
		_, err = audit.Write(ctx, tx, ro.sc, audit.Entry{Actor: ForwarderActor, Action: EventRollupRejected, Outcome: audit.OutcomeSuccess,
			CorrelationID: ro.trackingID, Details: details})
		return err
	})
	if err != nil {
		return 0, err
	}
	if lost {
		return sendLost, nil
	}
	s.log().WarnContext(ctx, "usage rollup rejected", "tenant_id", ro.sc.TenantID(), "tracking_id", ro.trackingID, "reason", reason)
	return sendRejected, nil
}

// retryLater ends the claim of a rollup Kill Bill did not take and sets
// when the next attempt is due. A rollup whose claim this forwarder lost is
// left to the forwarder that holds it.
func (s *Service) retryLater(ctx context.Context, ro rollup, cause error) error {
	wait := rollupBackoffFirst
	for i := 0; i < ro.attempts && wait < rollupBackoffMax; i++ {
		wait *= 2
	}
	wait = min(wait, rollupBackoffMax)
	args, _ := ro.sc.Bind(ro.id, s.now().Add(wait), errorName(cause), ro.token)
	if _, err := s.DB.Exec(context.WithoutCancel(ctx), `UPDATE usage_rollups SET attempts = attempts + 1, next_attempt_at = $4,
    claimed_until = NULL, claim_token = NULL, last_error = $5
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $6`, args...); err != nil {
		return unavailable(err)
	}
	return nil
}

// errorName is the error code name of err, never its text, which may hold
// Kill Bill's.
func errorName(err error) string {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return zserrors.Lookup(zsErr.Code).Name
	}
	return "internal_error"
}

// backlogAlerts emits metering.forward_backlog for every tenant with an
// event received more than BacklogAge ago and not yet in Kill Bill, at
// most once per BacklogAge per tenant.
func (s *Service) backlogAlerts(ctx context.Context) (int, error) {
	now := s.now()
	rows, err := s.DB.Query(ctx, `SELECT e.brand_id, e.tenant_id, count(*), min(e.received_at) FROM usage_events e
WHERE e.forwarded_at IS NULL AND e.received_at < $1
  AND NOT EXISTS (SELECT 1 FROM usage_rollups r WHERE r.brand_id = e.brand_id AND r.tenant_id = e.tenant_id AND r.id = e.rollup_id
                  AND r.status = 'discarded')
GROUP BY e.brand_id, e.tenant_id`, now.Add(-BacklogAge))
	if err != nil {
		return 0, unavailable(err)
	}
	type backlog struct {
		sc     scope.Scope
		count  int64
		oldest time.Time
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (backlog, error) {
		var b backlog
		err := r.Scan(&b.sc.Brand, &b.sc.Tenant, &b.count, &b.oldest)
		return b, err
	})
	if err != nil {
		return 0, unavailable(err)
	}
	sent := 0
	for _, b := range list {
		emitted := false
		err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
			ok, err := claimAlert(ctx, tx, b.sc, EventForwardBacklog, now, BacklogAge)
			if err != nil || !ok {
				return err
			}
			correlation := idgen.MustNewV7().String()
			payload := map[string]any{"events": b.count, "oldest_received_at": b.oldest.UTC().Format(time.RFC3339)}
			env, err := events.New(ctx, events.Params{Type: EventForwardBacklog, SchemaVersion: 1, Producer: producer(), Actor: ForwarderActor,
				Brand: b.sc.Brand, Tenant: b.sc.Tenant, Aggregate: events.Aggregate{Type: "tenant", ID: b.sc.TenantID()},
				CorrelationID: correlation, Payload: payload})
			if err != nil {
				return err
			}
			if err := outbox.Write(ctx, tx, env); err != nil {
				return unavailable(err)
			}
			if _, err := audit.Write(ctx, tx, b.sc, audit.Entry{Actor: ForwarderActor, Action: EventForwardBacklog, Outcome: audit.OutcomeSuccess,
				CorrelationID: correlation, Details: payload}); err != nil {
				return err
			}
			emitted = true
			return nil
		})
		if err != nil {
			return sent, err
		}
		if emitted {
			sent++
		}
	}
	return sent, nil
}

// claimAlert records that an alert of kind is sent for the tenant now,
// unless one was sent within every. It reports whether the caller sends
// it. The row is locked until tx ends, so two instances never both send.
func claimAlert(ctx context.Context, tx pgx.Tx, sc scope.Scope, kind string, now time.Time, every time.Duration) (bool, error) {
	args, err := sc.Bind(kind, now, now.Add(-every))
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO tenant_alerts (brand_id, tenant_id, kind, last_sent_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (brand_id, tenant_id, kind) DO UPDATE SET last_sent_at = EXCLUDED.last_sent_at
WHERE tenant_alerts.last_sent_at <= $5`, args...)
	if err != nil {
		return false, unavailable(err)
	}
	return tag.RowsAffected() == 1, nil
}

// RelayHandler forwards the tenant of each metering.usage.recorded event.
// A failed send is retried by the rollup's own backoff, so only a database
// failure makes the outbox retry the event.
func (s *Service) RelayHandler() outbox.Handler {
	return outbox.HandlerFunc(func(ctx context.Context, env events.Envelope) error {
		if env.Type != EventUsageRecorded {
			return outbox.ErrDeliveryTerminal
		}
		sc := scope.Scope{Brand: env.Brand(), Tenant: env.Tenant()}
		if err := sc.Validate(); err != nil {
			return outbox.ErrDeliveryTerminal
		}
		_, err := s.ForwardTenant(ctx, sc)
		if err != nil && !zserrors.IsCode(err, zserrors.CodeDatabaseUnavailable) {
			s.log().WarnContext(ctx, "usage forward after recording failed", "tenant_id", sc.TenantID(), "error", err.Error())
			return nil
		}
		return err
	})
}

// Run forwards every interval until ctx ends, and runs the outbox relay of
// metering.usage.recorded events in between, so recorded usage reaches
// Kill Bill without waiting for the next pass.
func (s *Service) Run(ctx context.Context, reader outbox.Reader, interval time.Duration) {
	relay := outbox.NewRelay("metering-forward", reader, s.RelayHandler(), 0, 0, s.Log)
	go relay.Run(ctx, time.Second)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := s.Forward(ctx)
		if err != nil {
			s.log().Error("usage forward failed", "error", err.Error())
		} else if report.RolledUp > 0 || report.Forwarded > 0 || report.Failed > 0 || report.Rejected > 0 || report.Backlogs > 0 {
			s.log().Info("usage forward", "rolled_up", report.RolledUp, "forwarded", report.Forwarded,
				"duplicates", report.Duplicates, "failed", report.Failed, "rejected", report.Rejected, "skipped", report.Skipped,
				"backlogs", report.Backlogs)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ClaimAlert is claimAlert for the wallet extension, which shares the
// tenant_alerts table.
func ClaimAlert(ctx context.Context, tx pgx.Tx, sc scope.Scope, kind string, now time.Time, every time.Duration) (bool, error) {
	return claimAlert(ctx, tx, sc, kind, now, every)
}
