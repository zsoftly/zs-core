// Package metering records usage with caller event IDs and forwards it to
// Kill Bill as usage records.
//
// Kill Bill rates and invoices usage. Core never stores a price. Core keeps
// what Kill Bill lacks: the brand's meters, each usage event with the
// caller's event ID so a retry is recorded once, daily summaries, and the
// rollups the forwarder sends. A rollup is a fixed set of events of one
// tenant, Kill Bill unit, subscription and day. Its Kill Bill tracking ID is
// a digest of that set, so a retry after a lost answer sends the same
// tracking ID and Kill Bill records it once.
//
// RecordUsage and RecordUsageBatch take a pgx.Tx, so a product records
// usage inside its own transaction. Every tenant query goes through
// scope.Bind. Quantities are int64 micro-units of the meter's unit, and a
// sum that would overflow is an error.
package metering

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// Limits of usage events.
const (
	// MaxBatch is the most events one batch takes.
	MaxBatch = 1000
	// MaxQuantityMicro caps one event: a thousand billion micro-units, a
	// million units.
	MaxQuantityMicro = 1_000_000_000_000_000
	// MaxDimensionValue caps a dimension value in characters.
	MaxDimensionValue = 200
	// MaxResourceRef caps the resource reference in characters.
	MaxResourceRef = 255
)

// Core events of the metering extension.
const (
	// EventUsageRecorded is written once per recording call. The forwarder
	// relay claims it and forwards the tenant's usage at once.
	EventUsageRecorded = "metering.usage.recorded"
	// EventForwardBacklog is emitted at most once a day per tenant when an
	// event waited more than BacklogAge to reach Kill Bill.
	EventForwardBacklog = "metering.forward_backlog"
)

// BacklogAge is how long an event may wait to reach Kill Bill before
// metering.forward_backlog is emitted.
const BacklogAge = 24 * time.Hour

// DB is the subset of pgxpool.Pool the service needs.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Querier is the subset of pgxpool.Pool and pgx.Tx the reads need.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Service records, reads and forwards usage. Recording needs nothing but a
// transaction. Forwarding and BilledUsage need DB and Billing.
type Service struct {
	DB      DB
	Billing *billing.Service
	Log     *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s != nil && s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s == nil || s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// Call is who records and the correlation ID of the request.
type Call struct {
	Actor         events.Actor
	CorrelationID string
}

func (c Call) validate() error {
	if !c.Actor.Type.Valid() || (c.Actor.Type.RequiresID() && c.Actor.ID == "") {
		return errors.New("metering: an actor is required")
	}
	if c.CorrelationID == "" {
		return errors.New("metering: a correlation ID is required")
	}
	return nil
}

// Event is one usage event a caller records. Quantity is in micro-units of
// the meter's unit. OccurredAt defaults to now. Subscription optionally
// names the Kill Bill subscription the usage goes to; otherwise the
// forwarder picks the tenant's active subscription whose plan prices the
// meter's Kill Bill unit.
type Event struct {
	Meter         string            `json:"meter"`
	EventID       string            `json:"event_id"`
	QuantityMicro int64             `json:"quantity_micro"`
	OccurredAt    time.Time         `json:"occurred_at"`
	Dimensions    map[string]string `json:"dimensions"`
	ResourceRef   string            `json:"resource_ref,omitempty"`
	Subscription  string            `json:"subscription,omitempty"`
}

// Usage is a recorded usage event.
type Usage struct {
	ID                 string            `json:"id"`
	Meter              string            `json:"meter"`
	EventID            string            `json:"event_id"`
	QuantityMicro      int64             `json:"quantity_micro"`
	Dimensions         map[string]string `json:"dimensions"`
	ResourceRef        string            `json:"resource_ref,omitempty"`
	Subscription       string            `json:"subscription,omitempty"`
	ReservationID      string            `json:"reservation_id,omitempty"`
	OccurredAt         time.Time         `json:"occurred_at"`
	ReceivedAt         time.Time         `json:"received_at"`
	ForwardedAt        *time.Time        `json:"forwarded_at"`
	KillBillTrackingID string            `json:"killbill_tracking_id,omitempty"`
}

var (
	eventIDPattern     = regexp.MustCompile(`^[\x21-\x7E]{1,255}$`)
	meterSlugPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	killBillIDPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	errMeterNotFound   = zserrors.New(zserrors.CodeMeterNotFound, "")
	errUsageConflict   = zserrors.New(zserrors.CodeUsageEventConflict, "")
	errOverflow        = zserrors.New(zserrors.CodeUsageOverflow, "")
	errTenantNotActive = tenant.ErrNotFound
)

func validation(message string) error { return zserrors.New(zserrors.CodeValidation, message) }

func unavailable(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
}

// recording is what one recording call reads once: the brand's window and
// the meters it used.
type recording struct {
	now             time.Time
	backfill, ahead time.Duration
	meters          map[string]brand.MeterRecord
}

func (s *Service) begin(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call) (*recording, error) {
	if err := call.validate(); err != nil {
		return nil, err
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	t, err := tenant.Get(ctx, tx, sc)
	if err != nil {
		return nil, err
	}
	// A suspended tenant's work that already ran is still billed. A
	// closed tenant has no billing surface.
	if t.Status == tenant.StatusClosed {
		return nil, errTenantNotActive
	}
	rec, err := brand.Get(ctx, tx, sc.Brand)
	if err != nil {
		return nil, err
	}
	if rec.Manifest == nil {
		return nil, zserrors.New(zserrors.CodeNotReady, "The brand is not applied.")
	}
	return &recording{now: s.now(), backfill: rec.Manifest.Billing.Backfill(), ahead: rec.Manifest.Billing.Future(),
		meters: map[string]brand.MeterRecord{}}, nil
}

func (r *recording) meter(ctx context.Context, tx pgx.Tx, brandID idgen.UUID, slug string) (brand.MeterRecord, error) {
	if m, ok := r.meters[slug]; ok {
		return m, nil
	}
	if !meterSlugPattern.MatchString(slug) {
		return brand.MeterRecord{}, errMeterNotFound
	}
	m, err := brand.MeterBySlug(ctx, tx, brandID, slug)
	if err != nil {
		return brand.MeterRecord{}, err
	}
	r.meters[slug] = m
	return m, nil
}

// normalize checks an event against its meter and the brand's window and
// returns it with OccurredAt set and truncated to microseconds, as the
// database keeps it.
func (r *recording) normalize(ev Event, m brand.MeterRecord, window bool) (Event, error) {
	if !eventIDPattern.MatchString(ev.EventID) {
		return Event{}, validation("The event_id must be 1 to 255 printable ASCII characters.")
	}
	if ev.QuantityMicro <= 0 || ev.QuantityMicro > MaxQuantityMicro {
		return Event{}, validation("The quantity_micro must be positive and at most 1000000000000000.")
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = r.now
	}
	ev.OccurredAt = ev.OccurredAt.UTC().Truncate(time.Microsecond)
	if window && (ev.OccurredAt.Before(r.now.Add(-r.backfill)) || ev.OccurredAt.After(r.now.Add(r.ahead))) {
		return Event{}, zserrors.New(zserrors.CodeUsageOutOfWindow, "The occurred_at must be at most "+r.backfill.String()+" ago and at most "+r.ahead.String()+" ahead.")
	}
	if utf8.RuneCountInString(ev.ResourceRef) > MaxResourceRef || !utf8.ValidString(ev.ResourceRef) || strings.ContainsAny(ev.ResourceRef, "\x00\r\n") {
		return Event{}, validation("The resource_ref must be at most 255 characters of one line.")
	}
	if ev.Subscription != "" && !killBillIDPattern.MatchString(ev.Subscription) {
		return Event{}, validation("The subscription must be a Kill Bill subscription ID.")
	}
	if ev.Dimensions == nil {
		ev.Dimensions = map[string]string{}
	}
	for k, v := range ev.Dimensions {
		if !slices.Contains(m.Dimensions, k) {
			return Event{}, validation("The dimension " + safeKey(k) + " is not a dimension of meter " + m.Slug + ".")
		}
		if v == "" || utf8.RuneCountInString(v) > MaxDimensionValue || !utf8.ValidString(v) || strings.ContainsAny(v, "\x00\r\n") {
			return Event{}, validation("A dimension value must be 1 to 200 characters of one line.")
		}
	}
	return ev, nil
}

// safeKey names a dimension key in a message only when it has the
// accepted form.
func safeKey(k string) string {
	if brand.DimensionKey.MatchString(k) {
		return k
	}
	return "key"
}

// canonicalDimensions is the JSON of the dimensions with sorted keys, and
// its digest.
func canonicalDimensions(d map[string]string) ([]byte, string) {
	keys := slices.Sorted(maps.Keys(d))
	buf := []byte{'{'}
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(d[k])
		buf = append(append(append(buf, kb...), ':'), vb...)
	}
	buf = append(buf, '}')
	sum := sha256.Sum256(buf)
	return buf, hex.EncodeToString(sum[:])
}

// fingerprint is the digest of an event's payload. A replay with another
// payload under the same event ID is usage_event_conflict.
func fingerprint(ev Event) string {
	dims, _ := canonicalDimensions(ev.Dimensions)
	h := sha256.New()
	for _, part := range []string{"zs-core.usage.v1", ev.Meter, ev.EventID, ev.OccurredAt.UTC().Format(time.RFC3339Nano),
		ev.ResourceRef, ev.Subscription, string(dims)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	var q [8]byte
	for i := 0; i < 8; i++ {
		q[i] = byte(uint64(ev.QuantityMicro) >> (56 - 8*i))
	}
	h.Write(q[:])
	return hex.EncodeToString(h.Sum(nil))
}

// settled names the reservation whose settlement sent an event to Kill Bill
// itself, and the tracking ID it went under.
type settled struct {
	reservationID idgen.UUID
	trackingID    string
}

// insert records one normalized event in tx, or replays it. via is set
// when a reservation's settlement sent the event to Kill Bill: the event is
// written already forwarded.
func (r *recording) insert(ctx context.Context, tx pgx.Tx, sc scope.Scope, m brand.MeterRecord, ev Event, via *settled) (Usage, bool, error) {
	fp := fingerprint(ev)
	id := idgen.MustNewV7()
	args, err := sc.Bind(m.ID, ev.EventID, id, ev.OccurredAt, fp)
	if err != nil {
		return Usage{}, false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO usage_event_keys (brand_id, tenant_id, meter_id, event_id, usage_id, occurred_at, fingerprint)
VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (brand_id, tenant_id, meter_id, event_id) DO NOTHING`, args...)
	if err != nil {
		return Usage{}, false, unavailable(err)
	}
	if tag.RowsAffected() == 0 {
		var (
			have     string
			usageID  idgen.UUID
			occurred time.Time
		)
		if err := tx.QueryRow(ctx, `SELECT fingerprint, usage_id, occurred_at FROM usage_event_keys
WHERE brand_id = $1 AND tenant_id = $2 AND meter_id = $3 AND event_id = $4`, args[:4]...).Scan(&have, &usageID, &occurred); err != nil {
			return Usage{}, false, unavailable(err)
		}
		if have != fp {
			return Usage{}, false, errUsageConflict
		}
		u, err := getUsage(ctx, tx, sc, usageID, occurred)
		return u, true, err
	}
	dims, dimsKey := canonicalDimensions(ev.Dimensions)
	var sub any
	if ev.Subscription != "" {
		sub = ev.Subscription
	}
	var rsv, forwardedAt, tracking any
	if via != nil {
		rsv, forwardedAt, tracking = via.reservationID, r.now, via.trackingID
	}
	insertArgs, _ := sc.Bind(id, m.ID, ev.EventID, m.KillBillUnit, ev.QuantityMicro, dims, dimsKey, nullable(ev.ResourceRef), sub, rsv, ev.OccurredAt, r.now,
		forwardedAt, tracking)
	if _, err := tx.Exec(ctx, `INSERT INTO usage_events (brand_id, tenant_id, id, meter_id, event_id, killbill_unit, quantity_micro,
    dimensions, dimensions_key, resource_ref, subscription_id, reservation_id, occurred_at, received_at, forwarded_at, killbill_tracking_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11::uuid, $12, $13, $14, $15, $16)`, insertArgs...); err != nil {
		return Usage{}, false, unavailable(err)
	}
	if err := addToSummary(ctx, tx, sc, m.ID, ev.OccurredAt, dims, dimsKey, ev.QuantityMicro); err != nil {
		return Usage{}, false, err
	}
	u := Usage{ID: idgen.Format(idgen.Usage, id), Meter: m.Slug, EventID: ev.EventID, QuantityMicro: ev.QuantityMicro,
		Dimensions: ev.Dimensions, ResourceRef: ev.ResourceRef, Subscription: ev.Subscription, OccurredAt: ev.OccurredAt, ReceivedAt: r.now}
	if via != nil {
		now := r.now
		u.ReservationID = idgen.Format(idgen.Reservation, via.reservationID)
		u.ForwardedAt, u.KillBillTrackingID = &now, via.trackingID
	}
	return u, false, nil
}

// addToSummary adds a quantity to the day's summary. A sum past the int64
// range is usage_overflow and changes nothing.
func addToSummary(ctx context.Context, tx pgx.Tx, sc scope.Scope, meterID idgen.UUID, at time.Time, dims []byte, dimsKey string, quantity int64) error {
	args, err := sc.Bind(meterID, at.UTC().Format(time.DateOnly), dimsKey, dims, quantity)
	if err != nil {
		return err
	}
	var one int
	err = tx.QueryRow(ctx, `INSERT INTO usage_summaries (brand_id, tenant_id, meter_id, day, dimensions_key, dimensions, quantity_micro, events)
VALUES ($1, $2, $3, $4::date, $5, $6::jsonb, $7, 1)
ON CONFLICT (brand_id, tenant_id, meter_id, day, dimensions_key) DO UPDATE SET
    quantity_micro = usage_summaries.quantity_micro + EXCLUDED.quantity_micro,
    events = usage_summaries.events + 1, updated_at = now()
WHERE usage_summaries.quantity_micro <= 9223372036854775807 - EXCLUDED.quantity_micro
RETURNING 1`, args...).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return errOverflow
	}
	if err != nil {
		return unavailable(err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RecordUsage records one usage event in tx. It is idempotent on the
// tenant, the meter and the event ID: the same payload again returns the
// recorded event with replayed true, another payload is
// usage_event_conflict. An unknown or retired meter is meter_not_found, a
// dimension key the meter does not declare is validation_failed, and an
// occurred_at outside the brand's backfill window is usage_out_of_window.
// The event is forwarded to Kill Bill later, by the forwarder.
func (s *Service) RecordUsage(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, ev Event) (Usage, bool, error) {
	out, replayed, err := s.RecordUsageBatch(ctx, tx, sc, call, []Event{ev})
	if err != nil {
		return Usage{}, false, err
	}
	return out[0], replayed[0], nil
}

// RecordUsageBatch records up to MaxBatch events in tx, all or nothing.
// The batch runs in a savepoint of tx: the first refused event rolls the
// savepoint back, so a caller that keeps tx after the error finds nothing
// of the batch in it. Each event is idempotent as in RecordUsage. One
// metering.usage.recorded event and one audit event cover the batch, when
// it recorded anything new.
func (s *Service) RecordUsageBatch(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, batch []Event) ([]Usage, []bool, error) {
	if len(batch) == 0 || len(batch) > MaxBatch {
		return nil, nil, validation("A batch holds 1 to 1000 events.")
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	out, replays, err := s.recordBatch(ctx, sp, sc, call, batch)
	if err != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return nil, nil, unavailable(rbErr)
		}
		return nil, nil, err
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, nil, unavailable(err)
	}
	return out, replays, nil
}

func (s *Service) recordBatch(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, batch []Event) ([]Usage, []bool, error) {
	r, err := s.begin(ctx, tx, sc, call)
	if err != nil {
		return nil, nil, err
	}
	out := make([]Usage, 0, len(batch))
	replays := make([]bool, 0, len(batch))
	seen := map[string]bool{}
	recorded := map[string]int{}
	for _, ev := range batch {
		m, err := r.meter(ctx, tx, sc.Brand, ev.Meter)
		if err != nil {
			return nil, nil, err
		}
		ev, err = r.normalize(ev, m, true)
		if err != nil {
			return nil, nil, err
		}
		key := m.Slug + "\x00" + ev.EventID
		u, replayed, err := r.insert(ctx, tx, sc, m, ev, nil)
		if err != nil {
			return nil, nil, err
		}
		if seen[key] {
			// The same event twice in one batch: the second is a replay of
			// the first.
			replayed = true
		}
		seen[key] = true
		if !replayed {
			recorded[m.Slug]++
		}
		out = append(out, u)
		replays = append(replays, replayed)
	}
	if len(recorded) == 0 {
		return out, replays, nil
	}
	if err := s.recorded(ctx, tx, sc, call, recorded); err != nil {
		return nil, nil, err
	}
	return out, replays, nil
}

// recorded writes the outbox event that wakes the forwarder and the audit
// event of a recording call.
func (s *Service) recorded(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, counts map[string]int) error {
	total := 0
	for _, n := range counts {
		total += n
	}
	env, err := events.New(ctx, events.Params{
		Type: EventUsageRecorded, SchemaVersion: 1, Producer: producer(), Actor: call.Actor,
		Brand: sc.Brand, Tenant: sc.Tenant, Aggregate: events.Aggregate{Type: "tenant", ID: sc.TenantID()},
		CorrelationID: call.CorrelationID, Payload: map[string]any{"events": total, "meters": counts},
	})
	if err != nil {
		return err
	}
	if err := outbox.Write(ctx, tx, env); err != nil {
		return unavailable(err)
	}
	_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "metering.usage.recorded", Outcome: audit.OutcomeSuccess,
		CorrelationID: call.CorrelationID, Details: map[string]any{"events": total, "meters": counts}})
	return err
}

// settledMeter returns the brand's active meter whose Kill Bill unit is
// unit, and the event a settlement records on it, normalized. ok is false
// when no meter maps to the unit.
func (s *Service) settledMeter(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, unit string, ev Event) (*recording, brand.MeterRecord, Event, bool, error) {
	r, err := s.begin(ctx, tx, sc, call)
	if err != nil {
		return nil, brand.MeterRecord{}, Event{}, false, err
	}
	var slug string
	if err := tx.QueryRow(ctx, `SELECT slug FROM meters WHERE brand_id = $1 AND killbill_unit = $2 AND status = 'active' ORDER BY slug LIMIT 1`,
		sc.Brand, unit).Scan(&slug); errors.Is(err, pgx.ErrNoRows) {
		return nil, brand.MeterRecord{}, Event{}, false, nil
	} else if err != nil {
		return nil, brand.MeterRecord{}, Event{}, false, unavailable(err)
	}
	m, err := r.meter(ctx, tx, sc.Brand, slug)
	if err != nil {
		return nil, brand.MeterRecord{}, Event{}, false, err
	}
	ev.Meter = slug
	// A settlement records what ran when it was asked for. A settlement
	// that waited for Kill Bill keeps that date, so the backfill window
	// does not apply.
	if ev, err = r.normalize(ev, m, false); err != nil {
		return nil, brand.MeterRecord{}, Event{}, false, err
	}
	return r, m, ev, true, nil
}

// CheckSettledUsage reports, before a settlement calls Kill Bill, whether
// the usage event it will record when it completes can be recorded: the
// event's key is free, or holds this very event. Another event under the
// key is usage_event_conflict, so a settlement never writes to Kill Bill
// what it could not then record.
func (s *Service) CheckSettledUsage(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, unit string, ev Event) error {
	_, m, ev, ok, err := s.settledMeter(ctx, tx, sc, call, unit, ev)
	if err != nil || !ok {
		return err
	}
	args, err := sc.Bind(m.ID, ev.EventID)
	if err != nil {
		return err
	}
	var have string
	err = tx.QueryRow(ctx, `SELECT fingerprint FROM usage_event_keys WHERE brand_id = $1 AND tenant_id = $2 AND meter_id = $3 AND event_id = $4`,
		args...).Scan(&have)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return unavailable(err)
	case have != fingerprint(ev):
		return errUsageConflict
	}
	return nil
}

// RecordSettledUsage records, in tx, the usage a reservation's settlement
// sent to Kill Bill under trackingID, on the brand's active meter whose
// Kill Bill unit is unit, when there is one. The settlement calls it only
// once Kill Bill took the usage, in the transaction that marks the
// reservation settled, so no event is left behind by a settlement that
// failed. ev.OccurredAt is when the settlement was asked for. The event
// names the reservation and is written already forwarded, so the forwarder
// never sends it. It returns the usage, or nil when no meter maps to the
// unit. A retry with the same event replays it.
func (s *Service) RecordSettledUsage(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, reservationID idgen.UUID, unit, trackingID string, ev Event) (*Usage, error) {
	r, m, ev, ok, err := s.settledMeter(ctx, tx, sc, call, unit, ev)
	if err != nil || !ok {
		return nil, err
	}
	u, replayed, err := r.insert(ctx, tx, sc, m, ev, &settled{reservationID: reservationID, trackingID: trackingID})
	if err != nil {
		return nil, err
	}
	if !replayed {
		if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "metering.usage.recorded", Outcome: audit.OutcomeSuccess,
			CorrelationID: call.CorrelationID, Details: map[string]any{"events": 1, "meters": map[string]int{m.Slug: 1},
				"reservation_id": idgen.Format(idgen.Reservation, reservationID)}}); err != nil {
			return nil, err
		}
	}
	return &u, nil
}

const usageColumns = `e.id, m.slug, e.event_id, e.quantity_micro, e.dimensions, coalesce(e.resource_ref, ''),
       coalesce(e.subscription_id::text, ''), e.reservation_id, e.occurred_at, e.received_at, e.forwarded_at,
       coalesce(e.killbill_tracking_id, '')`

func scanUsage(row pgx.Row) (Usage, error) {
	var (
		u     Usage
		id    idgen.UUID
		dims  []byte
		rsvID *idgen.UUID
	)
	if err := row.Scan(&id, &u.Meter, &u.EventID, &u.QuantityMicro, &dims, &u.ResourceRef, &u.Subscription, &rsvID,
		&u.OccurredAt, &u.ReceivedAt, &u.ForwardedAt, &u.KillBillTrackingID); err != nil {
		return Usage{}, err
	}
	u.ID = idgen.Format(idgen.Usage, id)
	if rsvID != nil {
		u.ReservationID = idgen.Format(idgen.Reservation, *rsvID)
	}
	u.Dimensions = map[string]string{}
	if err := json.Unmarshal(dims, &u.Dimensions); err != nil {
		return Usage{}, err
	}
	u.OccurredAt, u.ReceivedAt = u.OccurredAt.UTC(), u.ReceivedAt.UTC()
	if u.ForwardedAt != nil {
		f := u.ForwardedAt.UTC()
		u.ForwardedAt = &f
	}
	return u, nil
}

func getUsage(ctx context.Context, q Querier, sc scope.Scope, id idgen.UUID, occurred time.Time) (Usage, error) {
	args, err := sc.Bind(id, occurred)
	if err != nil {
		return Usage{}, err
	}
	u, err := scanUsage(q.QueryRow(ctx, `SELECT `+usageColumns+` FROM usage_events e JOIN meters m ON m.brand_id = e.brand_id AND m.id = e.meter_id
WHERE e.brand_id = $1 AND e.tenant_id = $2 AND e.id = $3 AND e.occurred_at = $4`, args...))
	if err != nil {
		return Usage{}, unavailable(err)
	}
	return u, nil
}

// Filter narrows a read of usage. A zero From or To is open.
type Filter struct {
	Meter string
	From  time.Time
	To    time.Time
}

// MaxLimit caps a page of usage events.
const MaxLimit = 200

// ListUsageEvents returns the tenant's usage events, newest first, a page
// at a time. cursor is the next_cursor of the previous page.
func ListUsageEvents(ctx context.Context, q Querier, sc scope.Scope, f Filter, limit int, cursor string) ([]Usage, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxLimit {
		return nil, "", zserrors.New(zserrors.CodeInvalidLimit, "")
	}
	afterAt, afterID, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if f.Meter != "" && !meterSlugPattern.MatchString(f.Meter) {
		return []Usage{}, "", nil
	}
	args, err := sc.Bind(f.Meter, nullTime(f.From), nullTime(f.To), nullTime(afterAt), afterID, limit+1)
	if err != nil {
		return nil, "", err
	}
	rows, err := q.Query(ctx, `SELECT `+usageColumns+` FROM usage_events e JOIN meters m ON m.brand_id = e.brand_id AND m.id = e.meter_id
WHERE e.brand_id = $1 AND e.tenant_id = $2 AND ($3 = '' OR m.slug = $3)
  AND ($4::timestamptz IS NULL OR e.occurred_at >= $4) AND ($5::timestamptz IS NULL OR e.occurred_at < $5)
  AND ($6::timestamptz IS NULL OR (e.occurred_at, e.id) < ($6, $7::uuid))
ORDER BY e.occurred_at DESC, e.id DESC LIMIT $8`, args...)
	if err != nil {
		return nil, "", unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Usage, error) { return scanUsage(r) })
	if err != nil {
		return nil, "", unavailable(err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		id, _ := idgen.ParseAs(idgen.Usage, last.ID)
		next = encodeCursor(last.OccurredAt, id)
	}
	if out == nil {
		out = []Usage{}
	}
	return out, next, nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// SummaryRow is the usage of one meter, day and dimension set.
type SummaryRow struct {
	Meter         string            `json:"meter"`
	Day           string            `json:"day"`
	Dimensions    map[string]string `json:"dimensions"`
	QuantityMicro int64             `json:"quantity_micro"`
	Events        int64             `json:"events"`
}

// MeterTotal is the usage of one meter over the period.
type MeterTotal struct {
	Meter         string `json:"meter"`
	Unit          string `json:"unit"`
	QuantityMicro int64  `json:"quantity_micro"`
	Events        int64  `json:"events"`
}

// Summary is the tenant's usage over a period of days, from Core's
// summaries.
type Summary struct {
	From   string       `json:"from"`
	To     string       `json:"to"`
	Totals []MeterTotal `json:"totals"`
	Rows   []SummaryRow `json:"rows"`
}

// MaxSummaryDays caps the period of a summary.
const MaxSummaryDays = 366

// GetUsageSummary returns the tenant's usage per meter, day and dimension
// set from from, inclusive, to to, exclusive, both days in UTC, and the
// total per meter. A total past the int64 range is usage_overflow.
func GetUsageSummary(ctx context.Context, q Querier, sc scope.Scope, meter string, from, to time.Time) (Summary, error) {
	from, to = from.UTC().Truncate(24*time.Hour), to.UTC().Truncate(24*time.Hour)
	if !to.After(from) || to.Sub(from) > MaxSummaryDays*24*time.Hour {
		return Summary{}, validation("The period must be 1 to 366 days, with to after from.")
	}
	args, err := sc.Bind(meter, from.Format(time.DateOnly), to.Format(time.DateOnly))
	if err != nil {
		return Summary{}, err
	}
	rows, err := q.Query(ctx, `SELECT m.slug, m.unit, s.day::text, s.dimensions, s.quantity_micro, s.events
FROM usage_summaries s JOIN meters m ON m.brand_id = s.brand_id AND m.id = s.meter_id
WHERE s.brand_id = $1 AND s.tenant_id = $2 AND ($3 = '' OR m.slug = $3) AND s.day >= $4::date AND s.day < $5::date
ORDER BY m.slug, s.day, s.dimensions_key`, args...)
	if err != nil {
		return Summary{}, unavailable(err)
	}
	defer rows.Close()
	out := Summary{From: from.Format(time.DateOnly), To: to.Format(time.DateOnly), Totals: []MeterTotal{}, Rows: []SummaryRow{}}
	totals := map[string]*MeterTotal{}
	for rows.Next() {
		var (
			row  SummaryRow
			unit string
			dims []byte
		)
		if err := rows.Scan(&row.Meter, &unit, &row.Day, &dims, &row.QuantityMicro, &row.Events); err != nil {
			return Summary{}, unavailable(err)
		}
		row.Dimensions = map[string]string{}
		if err := json.Unmarshal(dims, &row.Dimensions); err != nil {
			return Summary{}, unavailable(err)
		}
		out.Rows = append(out.Rows, row)
		t, ok := totals[row.Meter]
		if !ok {
			t = &MeterTotal{Meter: row.Meter, Unit: unit}
			totals[row.Meter] = t
		}
		if t.QuantityMicro > 1<<63-1-row.QuantityMicro {
			return Summary{}, errOverflow
		}
		t.QuantityMicro += row.QuantityMicro
		t.Events += row.Events
	}
	if err := rows.Err(); err != nil {
		return Summary{}, unavailable(err)
	}
	for _, t := range totals {
		out.Totals = append(out.Totals, *t)
	}
	sort.Slice(out.Totals, func(i, j int) bool { return out.Totals[i].Meter < out.Totals[j].Meter })
	return out, nil
}

// MeterUsage returns the tenant's usage of a meter, in micro-units, from
// from, inclusive, to to, exclusive, both days in UTC, from the summaries.
// A zero from counts from the first event.
func MeterUsage(ctx context.Context, q Querier, sc scope.Scope, meterSlug string, from, to time.Time) (int64, error) {
	args, err := sc.Bind(meterSlug, nullDay(from), to.UTC().Format(time.DateOnly))
	if err != nil {
		return 0, err
	}
	var (
		total    int64
		overflow bool
	)
	if err := q.QueryRow(ctx, `SELECT coalesce(sum(s.quantity_micro), 0) > 9223372036854775807,
       least(coalesce(sum(s.quantity_micro), 0), 9223372036854775807)::bigint
FROM usage_summaries s JOIN meters m ON m.brand_id = s.brand_id AND m.id = s.meter_id
WHERE s.brand_id = $1 AND s.tenant_id = $2 AND m.slug = $3 AND ($4::date IS NULL OR s.day >= $4::date) AND s.day < $5::date`, args...).Scan(&overflow, &total); err != nil {
		return 0, unavailable(err)
	}
	if overflow {
		return 0, errOverflow
	}
	return total, nil
}

func nullDay(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.DateOnly)
}

// RebuildSummaries recomputes the tenant's summaries of the days from
// from, inclusive, to to, exclusive, from its usage events, in tx.
func RebuildSummaries(ctx context.Context, tx pgx.Tx, sc scope.Scope, from, to time.Time) error {
	args, err := sc.Bind(from.UTC().Format(time.DateOnly), to.UTC().Format(time.DateOnly))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_summaries WHERE brand_id = $1 AND tenant_id = $2 AND day >= $3::date AND day < $4::date`, args...); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_summaries (brand_id, tenant_id, meter_id, day, dimensions_key, dimensions, quantity_micro, events)
SELECT brand_id, tenant_id, meter_id, (occurred_at AT TIME ZONE 'UTC')::date, dimensions_key, min(dimensions::text)::jsonb,
       sum(quantity_micro), count(*)
FROM usage_events
WHERE brand_id = $1 AND tenant_id = $2 AND occurred_at >= $3::date AT TIME ZONE 'UTC' AND occurred_at < $4::date AT TIME ZONE 'UTC'
GROUP BY brand_id, tenant_id, meter_id, (occurred_at AT TIME ZONE 'UTC')::date, dimensions_key`, args...); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "22003" {
			return errOverflow
		}
		return unavailable(err)
	}
	return nil
}

var (
	producerOnce sync.Once
	producerVal  events.Producer
)

// producer is the producer of the events this package writes.
func producer() events.Producer {
	producerOnce.Do(func() {
		host, err := os.Hostname()
		if err != nil || host == "" || len(host) > 160 {
			host = "zs-core"
		}
		producerVal = events.Producer{Service: "zs-core", Instance: host}
	})
	return producerVal
}

const cursorVersion = "u1"

// encodeCursor is the opaque cursor after an event.
func encodeCursor(at time.Time, id idgen.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorVersion + "|" + at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(c string) (time.Time, idgen.UUID, error) {
	if c == "" {
		return time.Time{}, idgen.Nil, nil
	}
	bad := zserrors.New(zserrors.CodeInvalidCursor, "")
	if len(c) > 128 {
		return time.Time{}, idgen.Nil, bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, idgen.Nil, bad
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 || parts[0] != cursorVersion {
		return time.Time{}, idgen.Nil, bad
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return time.Time{}, idgen.Nil, bad
	}
	id, err := idgen.ParseUUID(parts[2])
	if err != nil {
		return time.Time{}, idgen.Nil, bad
	}
	return at, id, nil
}

// BilledUsage reads the usage Kill Bill holds for the tenant from from,
// inclusive, to to, exclusive: for one subscription of the tenant, or for
// every one when subscription is empty. Kill Bill rated nothing here: this
// is the quantity it will bill.
func (s *Service) BilledUsage(ctx context.Context, sc scope.Scope, subscription string, from, to time.Time) ([]billing.BilledUsage, error) {
	if s.DB == nil || !s.Billing.Ready() {
		return nil, zserrors.New(zserrors.CodeNotReady, "Billed usage needs the Kill Bill admin settings.")
	}
	if subscription != "" && !killBillIDPattern.MatchString(subscription) {
		return nil, zserrors.New(zserrors.CodeNotFound, "")
	}
	acct, err := s.Billing.TenantAccount(ctx, s.DB, sc)
	if err != nil {
		return nil, err
	}
	ids := []string{subscription}
	if subscription == "" {
		subs, err := acct.Subscriptions(ctx)
		if err != nil {
			return nil, err
		}
		ids = ids[:0]
		for _, sub := range subs {
			ids = append(ids, sub.ID)
		}
	}
	out := []billing.BilledUsage{}
	for _, id := range ids {
		b, err := acct.BilledUsage(ctx, id, from, to)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}
