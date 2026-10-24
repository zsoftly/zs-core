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

	"github.com/zsoftly/zs-core/internal/killbill"
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
	// sendBatch is the most rollups one pass claims at a time. A claim is
	// the durable record that a send may follow and an expired claim is
	// never taken over, so a pass claims one rollup and sends it before it
	// claims the next: a crash strands at most that one.
	sendBatch = 1
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
	Skipped int
	// Unconfirmed counts rollups whose send started and whose outcome Kill
	// Bill did not confirm. They are never sent again by the forwarder:
	// staff resolve them against Kill Bill.
	Unconfirmed int
	Backlogs    int
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
	swept, err := s.ResolveStranded(ctx)
	report.Unconfirmed += swept
	if err != nil {
		return report, err
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

func (s *Service) claimTTL() time.Duration {
	if s.ClaimTTL > 0 {
		return s.ClaimTTL
	}
	return rollupClaim
}

// ResolveStranded is the recovery sweep. It never sends anything: Kill
// Bill cannot be read back by tracking ID, so a rollup whose send may have
// happened is kept for staff, with a fixed reason and an audit row.
//   - A claim that ran out before any send started becomes
//     usage_claim_unconfirmed, because an expired claim never regains the
//     right to send.
//   - A started send nobody finished within strandedAfter becomes
//     usage_send_unconfirmed.
//
// It returns how many rollups it resolved.
func (s *Service) ResolveStranded(ctx context.Context) (int, error) {
	resolved := 0
	for _, q := range []struct{ reason, where string }{
		{"usage_claim_unconfirmed", `send_provenance = 'possibly_attempted' AND claim_from_never AND send_started_at IS NULL AND send_payload IS NULL
        AND claim_token IS NOT NULL AND claimed_until <= clock_timestamp() AND $2::timestamptz IS NOT NULL`},
		{"usage_send_unconfirmed", `send_started_at IS NOT NULL AND send_payload IS NOT NULL AND claim_token IS NOT NULL
        AND send_started_at <= $2`},
	} {
		type stranded struct {
			sc                  scope.Scope
			id                  idgen.UUID
			tracking, unit, sub string
			quantity            int64
			events              int
		}
		var list []stranded
		err := pgx.BeginFunc(context.WithoutCancel(ctx), s.DB, func(tx pgx.Tx) error {
			list = nil
			rows, err := tx.Query(ctx, `UPDATE usage_rollups r SET closure_unresolved_reason = $1, last_error = $1, claim_from_never = false
FROM (SELECT id FROM usage_rollups WHERE status = 'pending' AND send_protocol = 1 AND closure_unresolved_reason IS NULL AND `+q.where+`
      ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED) due
WHERE r.id = due.id
RETURNING r.brand_id, r.tenant_id, r.id, r.tracking_id, r.killbill_unit, coalesce(r.subscription_id::text, ''), r.quantity_micro, r.events`,
				q.reason, s.now().Add(-strandedAfter))
			if err != nil {
				return unavailable(err)
			}
			list, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (stranded, error) {
				var x stranded
				err := r.Scan(&x.sc.Brand, &x.sc.Tenant, &x.id, &x.tracking, &x.unit, &x.sub, &x.quantity, &x.events)
				return x, err
			})
			if err != nil {
				return unavailable(err)
			}
			for _, x := range list {
				if _, err := audit.Write(ctx, tx, x.sc, audit.Entry{Actor: ForwarderActor, Action: "metering.usage.send_unconfirmed", Outcome: audit.OutcomeFailure,
					CorrelationID: x.tracking, Details: map[string]any{"rollup_id": idgen.Format(idgen.Rollup, x.id), "reason": q.reason,
						"killbill_tracking_id": x.tracking, "killbill_subscription_id": x.sub, "killbill_unit": x.unit,
						"quantity_micro": x.quantity, "events": x.events}}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return resolved, err
		}
		for _, x := range list {
			s.log().ErrorContext(ctx, "usage rollup needs review", "tenant_id", x.sc.TenantID(), "tracking_id", x.tracking, "reason", q.reason)
		}
		resolved += len(list)
	}
	return resolved, nil
}

// claimDue claims one batch of due rollups, of one tenant when only is set,
// under a fresh claim token. Only an unsent protocol 1 rollup can be
// claimed, once: the claim is the durable record that a send may follow, so
// an expired claim is never taken over, and a legacy protocol 0 rollup is
// read-only under the guard and never claimed.
func (s *Service) claimDue(ctx context.Context, only *scope.Scope) ([]rollup, error) {
	var brandID, tenantID any
	if only != nil {
		brandID, tenantID = only.Brand, only.Tenant
	}
	token := newClaimToken()
	rows, err := s.DB.Query(ctx, `UPDATE usage_rollups r SET claimed_until = clock_timestamp() + make_interval(secs => $2), claim_token = $6,
    claim_from_never = true, send_provenance = 'possibly_attempted'
FROM (SELECT id FROM usage_rollups WHERE status = 'pending' AND next_attempt_at <= $1
        AND send_protocol = 1 AND send_provenance = 'never_attempted' AND NOT claim_from_never
        AND claim_token IS NULL AND claimed_until IS NULL AND send_started_at IS NULL AND send_payload IS NULL
        AND closure_unresolved_reason IS NULL
        AND ($3::uuid IS NULL OR (brand_id = $3 AND tenant_id = $4))
      ORDER BY next_attempt_at, id LIMIT $5 FOR UPDATE SKIP LOCKED) due
WHERE r.id = due.id
RETURNING r.id, r.brand_id, r.tenant_id, r.killbill_unit, r.tracking_id, coalesce(r.requested_subscription_id::text, ''),
          coalesce(r.subscription_id::text, ''), r.day, r.record_at, r.period_start, r.quantity_micro, r.events, r.attempts`,
		s.now(), s.claimTTL().Seconds(), brandID, tenantID, sendBatch, token)
	if err != nil {
		return nil, unavailable(err)
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rollup, error) {
		ro := rollup{token: token}
		err := r.Scan(&ro.id, &ro.sc.Brand, &ro.sc.Tenant, &ro.unit, &ro.trackingID, &ro.requested, &ro.subscription, &ro.day,
			&ro.recordAt, &ro.periodStart, &ro.quantity, &ro.events, &ro.attempts)
		return ro, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return due, nil
}

// sendDue claims due rollups, of one tenant when only is set, a batch at a
// time under a claim token, and sends each to Kill Bill. Before each send
// it renews its claim. A rollup whose claim it lost, because the claim ran
// out and another forwarder took the rollup, is skipped and never sent.
func (s *Service) sendDue(ctx context.Context, only *scope.Scope, report *ForwardReport) error {
	for {
		due, err := s.claimDue(ctx, only)
		if err != nil {
			return err
		}
		sort.Slice(due, func(i, j int) bool { return due[i].id.String() < due[j].id.String() })
		for i, ro := range due {
			if ctx.Err() != nil {
				// Claimed but not started: give them back, so a restart
				// sends them instead of leaving them to an expired claim.
				for _, rest := range due[i:] {
					if err := s.releaseClaim(ctx, rest); err != nil {
						return err
					}
				}
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
			case outcome == sendUnconfirmed:
				report.Unconfirmed++
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

// releaseClaim gives a claimed rollup whose send has not started back to the
// unsent state, with nothing else changed. A rollup whose claim this
// forwarder lost is left alone.
func (s *Service) releaseClaim(ctx context.Context, ro rollup) error {
	args, _ := ro.sc.Bind(ro.id, ro.token)
	if _, err := s.DB.Exec(context.WithoutCancel(ctx), `UPDATE usage_rollups SET claimed_until = NULL, claim_token = NULL,
    claim_from_never = false, send_provenance = 'never_attempted'
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $4
  AND send_protocol = 1 AND send_started_at IS NULL AND send_payload IS NULL AND closure_unresolved_reason IS NULL
  AND claimed_until > clock_timestamp()`, args...); err != nil {
		return unavailable(err)
	}
	return nil
}

// renewClaim extends the claim of a rollup before it is sent. It reports
// false when the claim token no longer holds the rollup.
func (s *Service) renewClaim(ctx context.Context, ro rollup) (bool, error) {
	args, _ := ro.sc.Bind(ro.id, s.claimTTL().Seconds(), ro.token)
	// Only a live claim before the send started can be renewed; the guard
	// never lets an expired claim regain authority. The new deadline always
	// moves forward.
	tag, err := s.DB.Exec(ctx, `UPDATE usage_rollups SET claimed_until = greatest(clock_timestamp() + make_interval(secs => $4), claimed_until + interval '1 millisecond')
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $5
  AND send_protocol = 1 AND send_started_at IS NULL AND closure_unresolved_reason IS NULL AND claimed_until > clock_timestamp()`, args...)
	if err != nil {
		return false, unavailable(err)
	}
	return tag.RowsAffected() == 1, nil
}

// bindTarget records, before anything is sent, the subscription and the Kill
// Bill endpoint, tenant and account the usage would land in. The binding is
// made once: it reports false when the claim no longer holds the rollup or
// the rollup is already bound to another target.
func (s *Service) bindTarget(ctx context.Context, ro rollup, subscription string, t killbill.UsageTarget) (bound, mismatch bool, err error) {
	args, _ := ro.sc.Bind(ro.id, subscription, t.EndpointFingerprint, t.TenantID, t.AccountID, ro.token)
	tag, err := s.DB.Exec(ctx, `UPDATE usage_rollups SET subscription_id = $4::uuid, target_endpoint_fingerprint = $5,
    target_killbill_tenant_id = $6::uuid, target_killbill_account_id = $7::uuid
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $8
  AND send_protocol = 1 AND send_started_at IS NULL AND closure_unresolved_reason IS NULL AND claimed_until > clock_timestamp()
  AND (subscription_id IS NULL OR subscription_id = $4::uuid)
  AND (target_endpoint_fingerprint IS NULL OR (target_endpoint_fingerprint, target_killbill_tenant_id, target_killbill_account_id)
       = ($5, $6::uuid, $7::uuid))`, args...)
	if err != nil {
		return false, false, unavailable(err)
	}
	if tag.RowsAffected() == 1 {
		return true, false, nil
	}
	// Told apart: a claim that is still ours but bound elsewhere is a target
	// mismatch, anything else is a lost claim.
	var differs bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM usage_rollups
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $8
  AND send_protocol = 1 AND send_started_at IS NULL AND claimed_until > clock_timestamp()
  AND ((subscription_id IS NOT NULL AND subscription_id <> $4::uuid)
       OR (target_endpoint_fingerprint IS NOT NULL AND (target_endpoint_fingerprint, target_killbill_tenant_id, target_killbill_account_id)
           IS DISTINCT FROM ($5, $6::uuid, $7::uuid))))`, args...).Scan(&differs); err != nil {
		return false, false, unavailable(err)
	}
	return false, differs, nil
}

// auditTargetMismatch records that a rollup bound to one Kill Bill target
// was met by another, after a change of the Kill Bill address or tenant.
// The rollup waits and is not sent, and staff must see why.
func (s *Service) auditTargetMismatch(ctx context.Context, ro rollup, subscription string) error {
	ctx = context.WithoutCancel(ctx)
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		_, err := audit.Write(ctx, tx, ro.sc, audit.Entry{Actor: ForwarderActor, Action: "metering.usage.target_mismatch", Outcome: audit.OutcomeFailure,
			CorrelationID: ro.trackingID, Details: map[string]any{"rollup_id": idgen.Format(idgen.Rollup, ro.id),
				"killbill_tracking_id": ro.trackingID, "killbill_subscription_id": subscription, "killbill_unit": ro.unit,
				"quantity_micro": ro.quantity, "events": ro.events}})
		return err
	})
}

// startSend stores the body and the time of the send on a live claim,
// before the POST. It reports false when the claim no longer holds the
// rollup or the send was already started.
func (s *Service) startSend(ctx context.Context, ro rollup, payload []byte) (bool, error) {
	args, _ := ro.sc.Bind(ro.id, s.now(), string(payload), ro.token)
	tag, err := s.DB.Exec(ctx, `UPDATE usage_rollups SET send_started_at = $4, send_payload = $5::jsonb
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $6
  AND send_protocol = 1 AND send_started_at IS NULL AND send_payload IS NULL AND closure_unresolved_reason IS NULL
  AND claimed_until > clock_timestamp()`, args...)
	if err != nil {
		return false, unavailable(err)
	}
	return tag.RowsAffected() == 1, nil
}

// markUnconfirmed keeps a started send whose outcome Kill Bill did not
// confirm. The rollup stays pending with its stored send and a fixed
// reason, and the forwarder never claims it again: staff resolve it
// against Kill Bill, so the usage is neither lost nor sent twice.
func (s *Service) markUnconfirmed(ctx context.Context, ro rollup, reason string, cause error) (sendOutcome, error) {
	ctx = context.WithoutCancel(ctx)
	lost := false
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		args, _ := ro.sc.Bind(ro.id, reason, ro.token)
		tag, err := tx.Exec(ctx, `UPDATE usage_rollups SET closure_unresolved_reason = $4, last_error = $4, claim_from_never = false
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $5
  AND send_protocol = 1 AND send_started_at IS NOT NULL AND closure_unresolved_reason IS NULL`, args...)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 0 {
			lost = true
			return nil
		}
		_, err = audit.Write(ctx, tx, ro.sc, audit.Entry{Actor: ForwarderActor, Action: "metering.usage.send_unconfirmed", Outcome: audit.OutcomeFailure,
			CorrelationID: ro.trackingID, Details: map[string]any{"rollup_id": idgen.Format(idgen.Rollup, ro.id), "reason": reason,
				"killbill_tracking_id": ro.trackingID, "killbill_subscription_id": ro.subscription, "killbill_unit": ro.unit,
				"quantity_micro": ro.quantity, "events": ro.events}})
		return err
	})
	if err != nil {
		return 0, err
	}
	if lost {
		return sendLost, nil
	}
	s.log().ErrorContext(ctx, "usage rollup send unconfirmed", "tenant_id", ro.sc.TenantID(), "tracking_id", ro.trackingID,
		"reason", reason, "error", errorName(cause))
	return sendUnconfirmed, nil
}

type sendOutcome int

const (
	sendForwarded sendOutcome = iota
	sendDuplicate
	sendRejected
	sendLost
	sendUnconfirmed
)

// send writes one rollup to Kill Bill and records it. The subscription
// must take usage dated at the start of the rollup's period: a rollup
// whose period starts before the subscription started, or whose
// subscription is cancelled, is rejected instead, because Kill Bill would
// record it and never bill it.
func (s *Service) send(ctx context.Context, ro rollup) (sendOutcome, error) {
	acct, err := s.Billing.TenantAccount(ctx, s.DB, ro.sc)
	if err != nil {
		return 0, preflight(reasonPreflight, err)
	}
	target, err := acct.UsageSubscriptionAt(ctx, ro.subscription, ro.unit, ro.periodStart)
	switch {
	case errors.Is(err, billing.ErrUsageBeforeStart):
		return s.reject(ctx, ro, RejectBeforeStart)
	case errors.Is(err, billing.ErrSubscriptionCancelled):
		return s.reject(ctx, ro, RejectCancelled)
	case err != nil:
		return 0, preflight(reasonSubscription, err)
	}
	// The subscription is chosen once and kept, so every retry of the
	// tracking ID goes to the same subscription and the same Kill Bill
	// target, bound before anything is sent.
	subscription := ro.subscription
	if subscription == "" {
		subscription = target.Subscription.SubscriptionID
	}
	kbTarget, err := acct.UsageTarget(ctx, subscription)
	if err != nil {
		return 0, preflight(reasonTarget, err)
	}
	bound, mismatch, err := s.bindTarget(ctx, ro, subscription, kbTarget)
	if err != nil {
		return 0, err
	}
	if mismatch {
		if err := s.auditTargetMismatch(ctx, ro, subscription); err != nil {
			return 0, err
		}
		return 0, preflight(reasonTarget, errTargetMismatch)
	}
	if !bound {
		return sendLost, nil
	}
	ro.subscription = subscription
	call := billing.Call{Actor: ForwarderActor, Reason: "usage forward", CorrelationID: ro.trackingID}
	payload, err := acct.UsagePayload(ro.subscription, ro.unit, ro.trackingID, []billing.UsageAmount{{At: ro.recordAt, AmountMicro: ro.quantity}})
	if err != nil {
		return 0, preflight(reasonPayload, err)
	}
	// The send is written down first: from here on a lost answer leaves a
	// durable record that Kill Bill may hold the usage.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	started, err := s.startSend(ctx, ro, payload)
	if err != nil {
		return 0, err
	}
	if !started {
		return sendLost, nil
	}
	sendCtx, cancelSend := context.WithTimeout(context.WithoutCancel(ctx), sendBound)
	res, sendErr := acct.SendUsage(sendCtx, call, payload)
	cancelSend()
	duplicate := res.Outcome == killbill.UsageHeld
	if res.Outcome != killbill.UsageSent && !duplicate {
		// Nothing is sent again on an unconfirmed answer, and the guard
		// allows no way back from a started send.
		reason := "usage_send_unconfirmed"
		if res.Outcome == killbill.UsageNotSent {
			reason = "usage_transport_unconfirmed"
		}
		return s.markUnconfirmed(ctx, ro, reason, sendErr)
	}
	now := s.now()
	finalize := func() error {
		return pgx.BeginFunc(context.WithoutCancel(ctx), s.DB, func(tx pgx.Tx) error {
			ctx := context.WithoutCancel(ctx)
			args, _ := ro.sc.Bind(ro.id, now, ro.token)
			tag, err := tx.Exec(ctx, `UPDATE usage_rollups SET status = 'forwarded', forwarded_at = $4, claimed_until = NULL, claim_token = NULL, claim_from_never = false, last_error = NULL
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $5
  AND send_protocol = 1 AND send_started_at IS NOT NULL AND send_payload IS NOT NULL AND closure_unresolved_reason IS NULL`, args...)
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
WHERE brand_id = $1 AND tenant_id = $2 AND rollup_id = $3 AND forwarded_at IS NULL AND killbill_tracking_id IS NULL`, ev...); err != nil {
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
	}
	// Kill Bill confirmed the usage, so the finish is retried a few times
	// before it is left to the recovery sweep. The send is never repeated.
	for attempt := 0; ; attempt++ {
		if err = finalize(); err == nil || attempt == finalizeAttempts-1 {
			break
		}
		time.Sleep(time.Duration(attempt+1) * finalizeBackoff)
	}
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
    claimed_until = NULL, claim_token = NULL, claim_from_never = false, send_provenance = 'never_attempted'
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $6
  AND send_protocol = 1 AND send_started_at IS NULL AND send_payload IS NULL AND closure_unresolved_reason IS NULL
  AND claimed_until > clock_timestamp()`, args...)
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

// finalizeAttempts is how many times the database write that marks a
// confirmed send forwarded is tried, finalizeBackoff apart and growing.
const (
	finalizeAttempts = 3
	finalizeBackoff  = 200 * time.Millisecond
	// sendBound is the most a started send may take: the client's own
	// timeout, kept apart from the caller's context so a shutdown after the
	// send started still sees the answer.
	sendBound = 25 * time.Second
	// strandedAfter is how long a started send may stay unfinished before
	// the sweep takes it for unconfirmed.
	strandedAfter = 10 * time.Minute
)

// The reasons a rollup can wait before its send has started. The guard
// accepts no other last_error on a retry, and none holds Kill Bill's text.
const (
	reasonPreflight    = "usage_preflight_unavailable"
	reasonPayload      = "usage_payload_not_ready"
	reasonSubscription = "usage_subscription_unconfirmed"
	reasonTarget       = "usage_target_unconfirmed"
)

// preflightError is a failure before the send started, so nothing reached
// Kill Bill and the rollup can go back to unsent and wait.
type preflightError struct {
	reason string
	err    error
}

func (e *preflightError) Error() string { return e.err.Error() }
func (e *preflightError) Unwrap() error { return e.err }

var errTargetMismatch = errors.New("the rollup is bound to another Kill Bill target")

// preflight tags a failure before the send started with the reason the
// rollup waits. A Kill Bill that is down or unreachable is always
// usage_preflight_unavailable, whichever step found it.
func preflight(reason string, err error) error {
	if zserrors.IsCode(err, zserrors.CodeBillingUnavailable) {
		reason = reasonPreflight
	}
	return &preflightError{reason: reason, err: err}
}

// retryLater gives a rollup whose send has not started back to the unsent
// state and sets when the next attempt is due. A rollup whose claim this
// forwarder lost, or whose claim ran out, is left alone: an expired claim
// never regains the right to send.
func (s *Service) retryLater(ctx context.Context, ro rollup, cause error) error {
	wait := rollupBackoffFirst
	for i := 0; i < ro.attempts && wait < rollupBackoffMax; i++ {
		wait *= 2
	}
	wait = min(wait, rollupBackoffMax)
	reason := reasonPreflight
	var pre *preflightError
	if errors.As(cause, &pre) {
		reason = pre.reason
	}
	args, _ := ro.sc.Bind(ro.id, s.now().Add(wait), reason, ro.token)
	if _, err := s.DB.Exec(context.WithoutCancel(ctx), `UPDATE usage_rollups SET attempts = attempts + 1, next_attempt_at = greatest($4::timestamptz, clock_timestamp() + interval '1 second'),
    claimed_until = NULL, claim_token = NULL, claim_from_never = false, send_provenance = 'never_attempted', last_error = $5
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND claim_token = $6
  AND send_protocol = 1 AND send_started_at IS NULL AND send_payload IS NULL AND closure_unresolved_reason IS NULL
  AND claimed_until > clock_timestamp()`, args...); err != nil {
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
		} else if report.RolledUp > 0 || report.Forwarded > 0 || report.Failed > 0 || report.Rejected > 0 || report.Unconfirmed > 0 || report.Backlogs > 0 {
			s.log().Info("usage forward", "rolled_up", report.RolledUp, "forwarded", report.Forwarded,
				"duplicates", report.Duplicates, "failed", report.Failed, "rejected", report.Rejected, "skipped", report.Skipped,
				"unconfirmed", report.Unconfirmed, "backlogs", report.Backlogs)
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
