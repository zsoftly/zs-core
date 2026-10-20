// Package quota answers whether a tenant may use a feature now, and counts
// use, on Kill Bill.
//
// Kill Bill decides which plans a tenant is on: an entitlement check reads
// its active subscriptions, as package billing does, and fails closed.
// Core holds what Kill Bill lacks: the limit of each feature per plan, or
// for every plan, the limits staff set for one tenant, and counters. A day
// or month limit counts what callers consume in the window, raised to the
// meter's recorded usage when that is more. A concurrent limit counts what
// callers consume and release, such as running jobs, as leases that expire
// unless they are extended.
//
// Consume, Release and Extend take a pgx.Tx, so a product's admission runs
// inside its own transaction. Each takes a lock per tenant and feature, so
// concurrent consumers never exceed the limit. A lease belongs to the
// membership or API key that consumed it. Every tenant query goes through
// scope.Bind.
package quota

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
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
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// MaxConsume caps what one Consume takes.
const MaxConsume = 1_000_000

// Leases of concurrent quotas.
const (
	DefaultLease = time.Hour
	MinLease     = 10 * time.Second
	MaxLease     = 24 * time.Hour
)

// DriftEvery is how often quota.counter.drift may be audited for one
// tenant's feature.
const DriftEvery = 24 * time.Hour

// concurrentWindow is the window start of every concurrent counter.
var concurrentWindow = time.Unix(0, 0).UTC()

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

// Service is the entitlement check with quotas. Billing is required. DB is
// what CheckEntitlement reads through.
type Service struct {
	DB      DB
	Billing *billing.Service
	Log     *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// Call is who acts, why, and the correlation ID of the request.
type Call struct {
	Actor         events.Actor
	Reason        string
	CorrelationID string
	// Holder names who owns the leases this call takes, and who may
	// release or extend them: over HTTP the membership or the API key
	// behind the request. Empty means the actor itself, by its type and ID.
	Holder string
	// Staff marks a staff member with the support grant releasing another
	// holder's lease, with a reason, audited as quota.lease.staff_override.
	Staff bool
}

func (c Call) validate() error {
	if !c.Actor.Type.Valid() || (c.Actor.Type.RequiresID() && c.Actor.ID == "") {
		return errors.New("quota: an actor is required")
	}
	if c.CorrelationID == "" {
		return errors.New("quota: a correlation ID is required")
	}
	if utf8.RuneCountInString(c.Holder) > 200 {
		return errors.New("quota: the holder is too long")
	}
	if c.Staff && (c.Actor.Type != events.ActorStaff || strings.TrimSpace(c.Reason) == "") {
		return zserrors.New(zserrors.CodeReasonRequired, "")
	}
	return nil
}

func (c Call) holder() string {
	if c.Holder != "" {
		return c.Holder
	}
	return string(c.Actor.Type) + ":" + c.Actor.ID
}

// Entitlement is the answer of a check. Window is the quota's window when
// a quota applies.
type Entitlement struct {
	billing.Entitlement
	Window string `json:"window,omitempty"`
}

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)

func validation(message string) error { return zserrors.New(zserrors.CodeValidation, message) }

func unavailable(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
}

var errLeaseNotFound = zserrors.New(zserrors.CodeNotFound, "")

// effective is the quota that applies to a tenant's feature.
type effective struct {
	found    bool
	plan     string
	limit    int64
	window   string
	meter    string
	override bool
}

// resolve picks the quota of a feature for the tenant's active plans: the
// largest limit of a quota for one of the plans, otherwise the quota for
// every plan. A staff override in force then replaces the limit. An
// override that names its window applies only while the plan's quota has
// that window, or when no quota covers the feature. One that names none
// takes the window of the plan's quota. Overrides made before the latest
// clearing override never apply again, even once the clearing one expires.
func resolve(ctx context.Context, q Querier, sc scope.Scope, feature string, plans []string, now time.Time) (effective, error) {
	quotas, err := brand.ListQuotas(ctx, q, sc.Brand, feature)
	if err != nil {
		return effective{}, err
	}
	var e effective
	for _, qr := range quotas {
		for _, p := range plans {
			if qr.Plan == p && (!e.found || qr.Limit > e.limit) {
				e = effective{found: true, plan: p, limit: qr.Limit, window: qr.Window, meter: qr.Meter}
			}
		}
	}
	if !e.found {
		for _, qr := range quotas {
			if qr.Plan == brand.AnyPlan && len(plans) > 0 {
				e = effective{found: true, plan: plans[0], limit: qr.Limit, window: qr.Window, meter: qr.Meter}
			}
		}
	}
	args, err := sc.Bind(feature, now)
	if err != nil {
		return effective{}, err
	}
	var (
		limit         *int64
		window, meter string
	)
	err = q.QueryRow(ctx, `WITH cut AS (
    SELECT created_at, id FROM quota_overrides WHERE brand_id = $1 AND tenant_id = $2 AND feature = $3 AND limit_value IS NULL
    ORDER BY created_at DESC, id DESC LIMIT 1)
SELECT o.limit_value, coalesce(o.window_kind, ''), coalesce(o.meter_slug, '') FROM quota_overrides o
WHERE o.brand_id = $1 AND o.tenant_id = $2 AND o.feature = $3 AND (o.expires_at IS NULL OR o.expires_at > $4)
  AND (NOT EXISTS (SELECT 1 FROM cut) OR (o.created_at, o.id) >= (SELECT created_at, id FROM cut))
ORDER BY o.created_at DESC, o.id DESC LIMIT 1`, args...).Scan(&limit, &window, &meter)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return effective{}, unavailable(err)
	case limit == nil:
	case window == "" && e.found:
		e.limit, e.override = *limit, true
	case window == "":
		// Only an override made when a quota covered the feature names no
		// window. The quota is gone, so it counts nothing.
		e = effective{limit: *limit, window: brand.WindowNone, override: true}
	case !e.found:
		e = effective{limit: *limit, window: window, meter: meter, override: true}
		if len(plans) > 0 {
			e.plan = plans[0]
		}
	case e.window == window:
		e.limit, e.override = *limit, true
	}
	return e, nil
}

// windowStart is the start of the window of a day or month quota that
// holds now, and the start of the next one.
func windowStart(window string, now time.Time) (time.Time, time.Time) {
	switch window {
	case brand.WindowDay:
		start := now.UTC().Truncate(24 * time.Hour)
		return start, start.Add(24 * time.Hour)
	case brand.WindowMonth:
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	}
	return concurrentWindow, time.Time{}
}

// summaryUnits is the meter's usage in a window from the summaries, in
// whole units, rounded up. A zero from counts since the first event.
func summaryUnits(ctx context.Context, q Querier, sc scope.Scope, meter string, from time.Time, now time.Time) (int64, error) {
	micro, err := metering.MeterUsage(ctx, q, sc, meter, from, now.Truncate(24*time.Hour).Add(24*time.Hour))
	if err != nil {
		return 0, err
	}
	units := micro / 1_000_000
	if micro%1_000_000 != 0 {
		units++
	}
	return units, nil
}

// counter reads a counter, 0 when it does not exist.
func counter(ctx context.Context, q Querier, sc scope.Scope, feature string, start time.Time) (int64, error) {
	args, err := sc.Bind(feature, start)
	if err != nil {
		return 0, err
	}
	var v int64
	err = q.QueryRow(ctx, `SELECT value FROM quota_counters WHERE brand_id = $1 AND tenant_id = $2 AND feature = $3 AND window_start = $4`, args...).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, unavailable(err)
	}
	return v, nil
}

// use is a tenant's use of a feature in its quota's window. counted is
// false when nothing counts the feature: a none window without a meter.
// summary is what the meter's usage summaries count in a day or month
// window. It is more than counter when usage was recorded beyond what was
// consumed: drift.
type use struct {
	counter, summary int64
	counted          bool
}

func (u use) used() int64 { return max(u.counter, u.summary) }

// drifted reports whether the summaries count more than the counter.
func (u use) drifted() bool { return u.summary > u.counter }

// measure counts a tenant's use of a feature in its quota's window. A
// concurrent window reads its counter, the sum of the leases. A day or
// month window reads its counter, what Consume admitted, and the meter's
// usage summaries. A none window with a meter counts the meter's usage
// since the first event.
func measure(ctx context.Context, q Querier, sc scope.Scope, feature string, e effective, now time.Time) (use, error) {
	switch e.window {
	case brand.WindowConcurrent:
		v, err := counter(ctx, q, sc, feature, concurrentWindow)
		return use{counter: v, counted: true}, err
	case brand.WindowDay, brand.WindowMonth:
		start, _ := windowStart(e.window, now)
		c, err := counter(ctx, q, sc, feature, start)
		if err != nil {
			return use{}, err
		}
		units, err := summaryUnits(ctx, q, sc, e.meter, start, now)
		return use{counter: c, summary: units, counted: true}, err
	}
	if e.meter == "" {
		return use{}, nil
	}
	units, err := summaryUnits(ctx, q, sc, e.meter, time.Time{}, now)
	return use{counter: units, counted: true}, err
}

// raise lifts a day or month counter to the meter's usage when the
// summaries count more, in tx, and audits the drift as quota.counter.drift
// at most once per DriftEvery for the tenant's feature. The counter stays
// authoritative for admission: it only ever moves up to the summaries.
func raise(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, feature string, e effective, u use, now time.Time) error {
	if !u.drifted() || (e.window != brand.WindowDay && e.window != brand.WindowMonth) {
		return nil
	}
	start, _ := windowStart(e.window, now)
	args, err := sc.Bind(feature, start, u.summary, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO quota_counters (brand_id, tenant_id, feature, window_start, value, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (brand_id, tenant_id, feature, window_start) DO UPDATE SET value = greatest(quota_counters.value, EXCLUDED.value), updated_at = $6`,
		args...); err != nil {
		return unavailable(err)
	}
	kind := "quota.drift:" + feature
	if len(kind) > 100 {
		kind = kind[:100]
	}
	ok, err := metering.ClaimAlert(ctx, tx, sc, kind, now, DriftEvery)
	if err != nil || !ok {
		return err
	}
	_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "quota.counter.drift", Outcome: audit.OutcomeSuccess,
		CorrelationID: call.CorrelationID, Details: map[string]any{"feature": feature, "window": e.window,
			"window_start": start.Format(time.RFC3339), "counter": u.counter, "summary": u.summary}})
	return err
}

// answer builds the entitlement of a feature from the Kill Bill state and
// the quota.
func answer(st billing.EntitlementState, feature string, e effective, used int64, counted bool) Entitlement {
	plan, granted := st.Grants(feature)
	if e.found {
		granted, plan = true, e.plan
	}
	if !granted {
		return Entitlement{Entitlement: billing.Entitlement{Reason: billing.ReasonNotInPlan}}
	}
	out := Entitlement{Entitlement: billing.Entitlement{Allowed: true, Reason: billing.ReasonIncluded, Plan: plan}}
	if !e.found && !e.override {
		return out
	}
	out.Window = e.window
	limit := e.limit
	out.Limit = &limit
	if counted {
		u, remaining := used, max(e.limit-used, 0)
		out.Used, out.Remaining = &u, &remaining
		out.Allowed = used < e.limit
	} else {
		out.Allowed = e.limit > 0
	}
	if !out.Allowed {
		out.Reason = billing.ReasonQuotaExceeded
	}
	return out
}

// CheckActor is the audit actor of a drift a check finds.
var CheckActor = events.Actor{Type: events.ActorSystem}

// CheckEntitlement answers whether the tenant may use feature now:
// {allowed, limit, used, remaining, reason, plan}. It reads the tenant's
// active Kill Bill subscriptions and fails closed, as package billing does:
// no active subscription, a hold, a closed tenant or an unreachable Kill
// Bill all give allowed false with the reason. A feature is granted by an
// active plan's catalogue product or usage unit, or by a quota for an
// active plan or for every plan. A quota's limit, or a staff override in
// force, then caps it: allowed is false with quota_exceeded once used
// reaches the limit. Without a quota the feature has no limit. A day or
// month counter the usage summaries outgrew is raised to them, and the
// drift audited.
func (s *Service) CheckEntitlement(ctx context.Context, sc scope.Scope, feature string) Entitlement {
	if !billing.ValidFeature(feature) {
		return Entitlement{Entitlement: billing.Entitlement{Reason: billing.ReasonFeatureInvalid}}
	}
	if !s.Billing.Ready() || s.DB == nil {
		return Entitlement{Entitlement: billing.Entitlement{Reason: billing.ReasonBillingUnavailable}}
	}
	st := s.Billing.EntitlementState(ctx, sc)
	if st.Deny != "" {
		return Entitlement{Entitlement: billing.Entitlement{Reason: st.Deny}}
	}
	now := s.now()
	e, err := resolve(ctx, s.DB, sc, feature, st.Plans(), now)
	if err != nil {
		s.log().WarnContext(ctx, "entitlement check failed closed", "error", err.Error())
		return Entitlement{Entitlement: billing.Entitlement{Reason: billing.ReasonBillingUnavailable}}
	}
	u, err := measure(ctx, s.DB, sc, feature, e, now)
	if err != nil {
		s.log().WarnContext(ctx, "entitlement check failed closed", "error", err.Error())
		return Entitlement{Entitlement: billing.Entitlement{Reason: billing.ReasonBillingUnavailable}}
	}
	if u.drifted() {
		call := Call{Actor: CheckActor, CorrelationID: "quota-check-" + sc.TenantID()}
		if err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
			return raise(ctx, tx, sc, call, feature, e, u, now)
		}); err != nil {
			// The answer counts the summaries either way.
			s.log().WarnContext(ctx, "quota counter not raised", "error", err.Error())
		}
	}
	return answer(st, feature, e, u.used(), u.counted)
}

// lockFeature takes the lock of a tenant's feature for the rest of tx. The
// key names the schema, as the migration lock does.
func lockFeature(ctx context.Context, tx pgx.Tx, sc scope.Scope, feature string) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('zs-core.quota:' || current_schema() || ':' || $1::text || ':' || $2::text || ':' || $3, 0))`,
		sc.Brand, sc.Tenant, feature); err != nil {
		return unavailable(err)
	}
	return nil
}

// denied turns a refused entitlement into the error of a Consume.
func denied(reason string) error {
	switch reason {
	case billing.ReasonBillingUnavailable:
		return zserrors.New(zserrors.CodeBillingUnavailable, "")
	case billing.ReasonWalletOnHold:
		return zserrors.New(zserrors.CodeWalletOnHold, "")
	case billing.ReasonTenantNotFound:
		return zserrors.New(zserrors.CodeTenantNotFound, "")
	}
	return zserrors.New(zserrors.CodeQuotaExceeded, "The feature is not available to the tenant now: "+reason+".")
}

func checkLease(lease time.Duration) (time.Duration, error) {
	if lease == 0 {
		return DefaultLease, nil
	}
	if lease < MinLease || lease > MaxLease {
		return 0, validation("The lease must be 10 seconds to 24 hours.")
	}
	return lease, nil
}

func checkFeatureRef(feature, ref string) error {
	if !billing.ValidFeature(feature) {
		return validation("The feature must be 1 to 100 letters, digits, dots, colons, dashes or underscores.")
	}
	if !refPattern.MatchString(ref) {
		return validation("The ref must be 1 to 200 letters, digits, dots, colons, dashes or underscores, starting with a letter or digit.")
	}
	return nil
}

// Consume is ConsumeFor with the default lease.
func (s *Service) Consume(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, feature, ref string, n int64) (Entitlement, error) {
	return s.ConsumeFor(ctx, tx, sc, call, feature, ref, n, 0)
}

// lease is a lease row.
type lease struct {
	amount      int64
	window      string
	windowStart time.Time
	holder      string
	expiresAt   time.Time
}

func getLease(ctx context.Context, tx pgx.Tx, sc scope.Scope, feature, ref string) (*lease, error) {
	args, err := sc.Bind(feature, ref)
	if err != nil {
		return nil, err
	}
	var l lease
	err = tx.QueryRow(ctx, `SELECT amount, window_kind, window_start, created_by_actor, expires_at FROM quota_leases
WHERE brand_id = $1 AND tenant_id = $2 AND feature = $3 AND ref = $4 FOR UPDATE`, args...).Scan(&l.amount, &l.window, &l.windowStart, &l.holder, &l.expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable(err)
	}
	l.windowStart = l.windowStart.UTC()
	return &l, nil
}

// ConsumeFor takes n of a feature's quota for ref, such as a job ID, in tx,
// and returns the entitlement after. It holds the lock of the tenant's
// feature, so concurrent consumers never pass the limit together.
//
// A concurrent quota takes n as a lease that expires after lease, one hour
// by default, unless it is extended or released. A day or month quota
// counts n against the window's counter, which is what admission trusts,
// raised first to the meter's recorded usage when that is more. A none
// window cannot be consumed: quota_not_concurrent. The same ref again, by
// the same holder and in the same window, returns the entitlement and
// takes nothing more. Another holder's ref is not_found. A consumption past
// the limit is quota_exceeded. It fails closed as CheckEntitlement does:
// billing_unavailable, wallet_on_hold, tenant_not_found, or quota_exceeded
// with the reason.
func (s *Service) ConsumeFor(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, feature, ref string, n int64, leaseFor time.Duration) (Entitlement, error) {
	if err := call.validate(); err != nil {
		return Entitlement{}, err
	}
	if call.Staff {
		return Entitlement{}, zserrors.New(zserrors.CodePermissionDenied, "Staff do not consume a tenant's quota.")
	}
	if err := checkFeatureRef(feature, ref); err != nil {
		return Entitlement{}, err
	}
	if n <= 0 || n > MaxConsume {
		return Entitlement{}, validation("The amount must be 1 to 1000000.")
	}
	leaseFor, err := checkLease(leaseFor)
	if err != nil {
		return Entitlement{}, err
	}
	if !s.Billing.Ready() {
		return Entitlement{}, zserrors.New(zserrors.CodeNotReady, "Quotas need the Kill Bill admin settings.")
	}
	if err := lockFeature(ctx, tx, sc, feature); err != nil {
		return Entitlement{}, err
	}
	acct, err := s.Billing.TenantAccount(ctx, tx, sc)
	if err != nil {
		if zserrors.IsCode(err, zserrors.CodeTenantNotFound) {
			return Entitlement{}, err
		}
		return Entitlement{}, zserrors.Wrap(zserrors.CodeBillingUnavailable, "", err)
	}
	st := acct.EntitlementState(ctx)
	if st.Deny != "" {
		return Entitlement{}, denied(st.Deny)
	}
	now := s.now()
	e, err := resolve(ctx, tx, sc, feature, st.Plans(), now)
	if err != nil {
		return Entitlement{}, err
	}
	if !e.found {
		// An override caps a feature the plan grants. It grants nothing.
		if _, granted := st.Grants(feature); !granted {
			return Entitlement{}, denied(billing.ReasonNotInPlan)
		}
	}
	start, end := windowStart(e.window, now)
	switch e.window {
	case brand.WindowConcurrent:
		end = now.Add(leaseFor)
	case brand.WindowDay, brand.WindowMonth:
	default:
		return Entitlement{}, zserrors.New(zserrors.CodeQuotaNotConcurrent, "")
	}
	have, err := getLease(ctx, tx, sc, feature, ref)
	if err != nil {
		return Entitlement{}, err
	}
	if have != nil && have.holder != call.holder() {
		return Entitlement{}, errLeaseNotFound
	}
	if have != nil && have.window == e.window && have.windowStart.Equal(start) {
		if have.amount != n {
			return Entitlement{}, zserrors.New(zserrors.CodeConflict, "This ref already holds another amount of the feature.")
		}
		u, err := measure(ctx, tx, sc, feature, e, now)
		if err != nil {
			return Entitlement{}, err
		}
		return answer(st, feature, e, u.used(), true), nil
	}
	if have != nil {
		// The ref's lease is of an earlier window or another kind of quota:
		// it no longer counts. A concurrent one is released first.
		if err := dropLease(ctx, tx, sc, call, feature, ref, *have, "quota.lease.replaced", now); err != nil {
			return Entitlement{}, err
		}
	}
	u, err := measure(ctx, tx, sc, feature, e, now)
	if err != nil {
		return Entitlement{}, err
	}
	if err := raise(ctx, tx, sc, call, feature, e, u, now); err != nil {
		return Entitlement{}, err
	}
	used := u.used()
	if used > e.limit-n {
		return Entitlement{}, zserrors.New(zserrors.CodeQuotaExceeded, "")
	}
	insert, _ := sc.Bind(feature, ref, n, e.window, start, end, call.holder(), string(call.Actor.Type), nullable(call.Actor.ID), now)
	if _, err := tx.Exec(ctx, `INSERT INTO quota_leases (brand_id, tenant_id, feature, ref, amount, window_kind, window_start, expires_at,
    created_by_actor, actor_type, actor_id, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)`, insert...); err != nil {
		return Entitlement{}, unavailable(err)
	}
	if err := addCounter(ctx, tx, sc, feature, start, n, now); err != nil {
		return Entitlement{}, err
	}
	details := map[string]any{"feature": feature, "ref": ref, "amount": n, "used": used + n, "limit": e.limit, "window": e.window}
	if e.window == brand.WindowConcurrent {
		details["expires_at"] = end.Format(time.RFC3339)
	}
	if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "quota.consumed", Reason: strings.TrimSpace(call.Reason),
		Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: details}); err != nil {
		return Entitlement{}, err
	}
	return answer(st, feature, e, used+n, true), nil
}

// addCounter adds delta to the tenant's counter of a feature and window,
// never below zero.
func addCounter(ctx context.Context, tx pgx.Tx, sc scope.Scope, feature string, start time.Time, delta int64, now time.Time) error {
	args, err := sc.Bind(feature, start, delta, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO quota_counters (brand_id, tenant_id, feature, window_start, value, updated_at)
VALUES ($1, $2, $3, $4, greatest($5, 0), $6)
ON CONFLICT (brand_id, tenant_id, feature, window_start) DO UPDATE SET value = greatest(quota_counters.value + $5, 0), updated_at = $6`,
		args...); err != nil {
		return unavailable(err)
	}
	return nil
}

// dropLease deletes a lease in tx. A concurrent lease gives its amount
// back to the counter. A day or month consumption stays counted in its
// window. It is audited as action.
func dropLease(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, feature, ref string, l lease, action string, now time.Time) error {
	args, err := sc.Bind(feature, ref)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM quota_leases WHERE brand_id = $1 AND tenant_id = $2 AND feature = $3 AND ref = $4`, args...); err != nil {
		return unavailable(err)
	}
	if l.window == brand.WindowConcurrent {
		if err := addCounter(ctx, tx, sc, feature, concurrentWindow, -l.amount, now); err != nil {
			return err
		}
	}
	_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: action, Reason: strings.TrimSpace(call.Reason),
		Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: map[string]any{"feature": feature, "ref": ref,
			"amount": l.amount, "window": l.window, "holder": l.holder}})
	return err
}

// Release returns what ref holds of a feature's concurrent quota, in tx,
// and reports the amount returned. Releasing a ref that holds nothing
// returns 0 and changes nothing, so a retry is safe. A day or month
// consumption is never given back: releasing it returns 0. Only the
// lease's holder, or a staff override, may release it: anyone else is
// not_found. Release never calls Kill Bill: a release always goes through.
func (s *Service) Release(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, feature, ref string) (int64, error) {
	if err := call.validate(); err != nil {
		return 0, err
	}
	if err := checkFeatureRef(feature, ref); err != nil {
		return 0, err
	}
	if err := lockFeature(ctx, tx, sc, feature); err != nil {
		return 0, err
	}
	l, err := getLease(ctx, tx, sc, feature, ref)
	if err != nil || l == nil {
		return 0, err
	}
	if !call.Staff && l.holder != call.holder() {
		return 0, errLeaseNotFound
	}
	if l.window != brand.WindowConcurrent {
		return 0, nil
	}
	if err := dropLease(ctx, tx, sc, call, feature, ref, *l, "quota.released", s.now()); err != nil {
		return 0, err
	}
	if call.Staff && l.holder != call.holder() {
		if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "quota.lease.staff_override", Reason: strings.TrimSpace(call.Reason),
			Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: map[string]any{"feature": feature, "ref": ref,
				"amount": l.amount, "holder": l.holder}}); err != nil {
			return 0, err
		}
	}
	return l.amount, nil
}

// Lease is a concurrent lease after Extend.
type Lease struct {
	Feature   string    `json:"feature"`
	Ref       string    `json:"ref"`
	Amount    int64     `json:"amount"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Extend renews a concurrent lease to leaseFor from now, in tx. A ref that
// holds nothing, or another holder's, is not_found.
func (s *Service) Extend(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, feature, ref string, leaseFor time.Duration) (Lease, error) {
	if err := call.validate(); err != nil {
		return Lease{}, err
	}
	if err := checkFeatureRef(feature, ref); err != nil {
		return Lease{}, err
	}
	leaseFor, err := checkLease(leaseFor)
	if err != nil {
		return Lease{}, err
	}
	if err := lockFeature(ctx, tx, sc, feature); err != nil {
		return Lease{}, err
	}
	l, err := getLease(ctx, tx, sc, feature, ref)
	if err != nil {
		return Lease{}, err
	}
	if l == nil || l.window != brand.WindowConcurrent || (!call.Staff && l.holder != call.holder()) {
		return Lease{}, errLeaseNotFound
	}
	now := s.now()
	args, _ := sc.Bind(feature, ref, now.Add(leaseFor), now)
	if _, err := tx.Exec(ctx, `UPDATE quota_leases SET expires_at = $5, updated_at = $6
WHERE brand_id = $1 AND tenant_id = $2 AND feature = $3 AND ref = $4`, args...); err != nil {
		return Lease{}, unavailable(err)
	}
	out := Lease{Feature: feature, Ref: ref, Amount: l.amount, ExpiresAt: now.Add(leaseFor)}
	_, err = audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "quota.lease.extended", Reason: strings.TrimSpace(call.Reason),
		Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: map[string]any{"feature": feature, "ref": ref,
			"amount": l.amount, "expires_at": out.ExpiresAt.Format(time.RFC3339)}})
	return out, err
}

// SweeperActor is the audit actor of the lease sweeper.
var SweeperActor = events.Actor{Type: events.ActorSystem}

// Sweeper releases concurrent leases past their expiry and audits each as
// quota.lease.expired. Day and month consumptions past their window are
// dropped without an audit: their window is over.
type Sweeper struct {
	Service *Service
	// Batch is how many leases one page reads. Zero means 100.
	Batch int
}

// SweepOnce releases every lease past its expiry and returns how many
// concurrent leases it released.
func (w *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	s := w.Service
	if s.DB == nil {
		return 0, zserrors.New(zserrors.CodeNotReady, "")
	}
	batch := w.Batch
	if batch <= 0 {
		batch = 100
	}
	released := 0
	for {
		now := s.now()
		rows, err := s.DB.Query(ctx, `SELECT brand_id, tenant_id, feature, ref FROM quota_leases WHERE expires_at <= $1
ORDER BY expires_at LIMIT $2`, now, batch)
		if err != nil {
			return released, unavailable(err)
		}
		type due struct {
			sc           scope.Scope
			feature, ref string
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
			var d due
			err := r.Scan(&d.sc.Brand, &d.sc.Tenant, &d.feature, &d.ref)
			return d, err
		})
		if err != nil {
			return released, unavailable(err)
		}
		call := Call{Actor: SweeperActor, Reason: "lease expired", CorrelationID: idgen.MustNewV7().String()}
		for _, d := range list {
			err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
				if err := lockFeature(ctx, tx, d.sc, d.feature); err != nil {
					return err
				}
				l, err := getLease(ctx, tx, d.sc, d.feature, d.ref)
				if err != nil || l == nil || l.expiresAt.After(now) {
					return err
				}
				if l.window != brand.WindowConcurrent {
					args, _ := d.sc.Bind(d.feature, d.ref)
					_, err := tx.Exec(ctx, `DELETE FROM quota_leases WHERE brand_id = $1 AND tenant_id = $2 AND feature = $3 AND ref = $4`, args...)
					return unavailableOrNil(err)
				}
				if err := dropLease(ctx, tx, d.sc, call, d.feature, d.ref, *l, "quota.lease.expired", now); err != nil {
					return err
				}
				released++
				return nil
			})
			if err != nil {
				return released, err
			}
		}
		if len(list) < batch {
			return released, nil
		}
	}
}

// Run sweeps every interval until ctx ends.
func (w *Sweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := w.SweepOnce(ctx)
		if err != nil {
			w.Service.log().Error("quota lease sweep failed", "error", err.Error())
		} else if n > 0 {
			w.Service.log().Info("quota lease sweep", "released", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func unavailableOrNil(err error) error {
	if err == nil {
		return nil
	}
	return unavailable(err)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Override is a limit staff set for one tenant's feature. A nil Limit
// clears the overrides before it. Window and Meter are set when the
// override names its window.
type Override struct {
	ID        string     `json:"id"`
	Feature   string     `json:"feature"`
	Limit     *int64     `json:"limit"`
	Window    string     `json:"window,omitempty"`
	Meter     string     `json:"meter,omitempty"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// OverrideRequest is a staff limit for one tenant's feature. Window, and
// Meter for a day or month window, are needed when no quota of the brand
// covers the feature. Otherwise they are optional, and an override that
// names a window applies only while the plan's quota has it.
type OverrideRequest struct {
	Feature   string
	Limit     *int64
	Window    string
	Meter     string
	ExpiresAt *time.Time
}

// SetOverride records a staff limit for the tenant's feature in tx. The
// latest override in force replaces the plan's limit. An override with a
// nil Limit clears the ones before it for good, even when it expires
// itself. call.Reason is required. ExpiresAt, when set, is in the future
// and within 10 years.
func (s *Service) SetOverride(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, req OverrideRequest) (Override, error) {
	if err := call.validate(); err != nil {
		return Override{}, err
	}
	if call.Actor.ID == "" {
		return Override{}, errors.New("quota: an override needs a named actor")
	}
	feature, limit, expiresAt := req.Feature, req.Limit, req.ExpiresAt
	if !billing.ValidFeature(feature) {
		return Override{}, validation("The feature must be 1 to 100 letters, digits, dots, colons, dashes or underscores.")
	}
	reason := strings.TrimSpace(call.Reason)
	if reason == "" {
		return Override{}, zserrors.New(zserrors.CodeReasonRequired, "")
	}
	if utf8.RuneCountInString(reason) > audit.MaxReasonLength {
		return Override{}, validation("The reason is too long.")
	}
	if limit != nil && (*limit < 0 || *limit > brand.MaxQuotaLimit) {
		return Override{}, validation("The limit must be 0 to 1000000000000000, or null to clear the override.")
	}
	t, err := tenant.Get(ctx, tx, sc)
	if err != nil {
		return Override{}, err
	}
	if t.Status == tenant.StatusClosed {
		return Override{}, tenant.ErrNotFound
	}
	window, meter := req.Window, req.Meter
	switch {
	case limit == nil && (window != "" || meter != ""):
		return Override{}, validation("A clearing override names no window.")
	case window == "" && meter != "":
		return Override{}, validation("The meter needs a window.")
	case window == brand.WindowDay || window == brand.WindowMonth:
		if _, err := brand.MeterBySlug(ctx, tx, sc.Brand, meter); err != nil {
			return Override{}, validation("A day or month override counts the usage of an active meter of the brand, so it needs one.")
		}
	case window == brand.WindowConcurrent:
		if meter != "" {
			return Override{}, validation("A concurrent override takes no meter.")
		}
	case window == brand.WindowNone:
		if meter != "" {
			if _, err := brand.MeterBySlug(ctx, tx, sc.Brand, meter); err != nil {
				return Override{}, validation("The meter must be an active meter of the brand.")
			}
		}
	case window != "":
		return Override{}, validation("The window must be none, day, month or concurrent.")
	}
	if limit != nil && window == "" {
		quotas, err := brand.ListQuotas(ctx, tx, sc.Brand, feature)
		if err != nil {
			return Override{}, err
		}
		if len(quotas) == 0 {
			return Override{}, validation("No quota of the brand covers the feature, so the override must name its window.")
		}
	}
	now := s.now()
	if expiresAt != nil {
		e := expiresAt.UTC().Truncate(time.Microsecond)
		if !e.After(now) || e.After(now.AddDate(10, 0, 0)) {
			return Override{}, validation("The expires_at must be in the future and within 10 years.")
		}
		expiresAt = &e
	}
	id := idgen.MustNewV7()
	args, err := sc.Bind(id, feature, limit, nullable(window), nullable(meter), reason, expiresAt, string(call.Actor.Type), call.Actor.ID, call.CorrelationID, now)
	if err != nil {
		return Override{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO quota_overrides (brand_id, tenant_id, id, feature, limit_value, window_kind, meter_slug, reason, expires_at,
    actor_type, actor_id, correlation_id, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, args...); err != nil {
		return Override{}, unavailable(err)
	}
	o := Override{ID: idgen.Format(idgen.Quota, id), Feature: feature, Limit: limit, Window: window, Meter: meter, Reason: reason,
		ExpiresAt: expiresAt, CreatedAt: now}
	details := map[string]any{"override_id": o.ID, "feature": feature, "limit": limit}
	if window != "" {
		details["window"] = window
	}
	if meter != "" {
		details["meter"] = meter
	}
	if expiresAt != nil {
		details["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: "quota.override.set", Reason: reason,
		Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: details}); err != nil {
		return Override{}, err
	}
	return o, nil
}
