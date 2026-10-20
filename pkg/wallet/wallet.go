// Package wallet is the prepaid wallet view and the reservations before work
// of ZSoftly Core, on Kill Bill.
//
// Kill Bill is the financial authority and holds every credit. The wallet
// is a view: the Kill Bill account credit, less what Kill Bill will
// invoice and has not yet, minus the reservations Core holds, plus the
// brand's overdraft allowance. A reservation holds part of that credit
// before a job starts, and belongs to the membership or API key that made
// it. Settlement first writes its intent to the reservation, then writes
// the result to Kill Bill, as a usage record on a subscription or as an
// external charge that Kill Bill pays from the credit, then records the
// Kill Bill reference and releases the rest of the hold. A settlement Kill
// Bill cannot take yet stays pending and the sweeper completes it later.
// Core never stores a balance, a payment, an invoice or a price.
//
// Reserve, Settle, Release and Extend take a pgx.Tx, so a product's
// scheduler calls them inside its own transaction. Every write takes the
// tenant's advisory lock first, so concurrent callers of one tenant see
// each other's holds. Every tenant query goes through scope.Bind. Amounts
// are int64 minor units of the account currency.
package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/billing"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// Reservation statuses.
const (
	StatusHeld     = "held"
	StatusSettled  = "settled"
	StatusReleased = "released"
	StatusExpired  = "expired"
)

// Kill Bill references a settlement produces.
const (
	RefUsageRecord    = "usage_record"
	RefExternalCharge = "external_charge"
)

// How a settlement's amount was found.
const (
	RatedByExternalCharge = "external_charge"
	RatedByCatalogue      = "catalogue_price"
	RatedByNone           = "unrated"
)

// Leases and limits.
const (
	DefaultLease = 15 * time.Minute
	MinLease     = 10 * time.Second
	MaxLease     = 24 * time.Hour
	// MaxAmountMinor caps one hold, as billing.MaxCreditMinor caps a
	// credit.
	MaxAmountMinor = billing.MaxCreditMinor
	// MaxAttempt caps the caller's attempt number.
	MaxAttempt = 1_000_000
)

// Core events of the wallet extension.
const (
	EventReservationSettled     = "reservation.settled"
	EventReservationReleased    = "reservation.released"
	EventReservationExpired     = "reservation.expired"
	EventSettlementFailed       = "reservation.settlement_failed"
	EventLowBalance             = "wallet.low_balance"
	EventReconciliationMismatch = "wallet.reconciliation_mismatch"
	// EventSettlementRequested carries a settlement Kill Bill could not take
	// yet. The settlement relay claims it and completes the settlement. It
	// is an internal event type: see outbox.InternalEventTypes.
	EventSettlementRequested = "wallet.settlement.requested"
	// ActionSettlementStale audits a settlement that still waits for Kill
	// Bill a day after it was asked for.
	ActionSettlementStale = "wallet.settlement_stale"
	// ActionStaffOverride audits a staff member who settled, released or
	// extended a reservation another holder made.
	ActionStaffOverride = "wallet.reservation.staff_override"
)

// LowBalanceEvery is how often wallet.low_balance may be emitted for one
// tenant.
const LowBalanceEvery = 24 * time.Hour

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

// Service is the wallet. Billing is required. DB is needed by the view,
// the sweeper and the settlement relay. Metering records the usage a
// settlement sends, when a meter of the brand maps to its unit.
type Service struct {
	DB       DB
	Billing  *billing.Service
	Metering *metering.Service
	// CreditTTL is how long Reserve relies on the Kill Bill credit it last
	// read for a tenant in this process. Zero reads Kill Bill every time.
	CreditTTL time.Duration
	Log       *slog.Logger
	// Now defaults to time.Now.
	Now func() time.Time

	mu    sync.Mutex
	cache map[scope.Scope]cached
}

// cached is a tenant's Kill Bill credit and unbilled amount as one instance
// last read them, with the wallet version it read before. Hold reasons are
// never cached.
type cached struct {
	credit   int64
	unbilled int64
	version  int64
	at       time.Time
}

// Ready reports whether the service can call Kill Bill.
func (s *Service) Ready() bool { return s != nil && s.Billing.Ready() }

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

func (s *Service) metering() *metering.Service {
	if s.Metering != nil {
		return s.Metering
	}
	return &metering.Service{Now: s.Now}
}

var errNotReady = zserrors.New(zserrors.CodeNotReady, "The wallet needs the Kill Bill admin settings.")

// Call is who acts, why, and the correlation ID of the request.
type Call struct {
	Actor         events.Actor
	Reason        string
	CorrelationID string
	// Holder names who owns the holds this call makes, and who may settle,
	// release, extend or grow them: over HTTP the membership or the API key
	// behind the request. Empty means the actor itself, by its type and ID.
	Holder string
	// Staff marks a staff member with the support grant acting on another
	// holder's reservation: settle, release and extend go through, with a
	// reason, and are audited as wallet.reservation.staff_override. A staff
	// call never reserves.
	Staff bool
}

func (c Call) validate() error {
	if !c.Actor.Type.Valid() || (c.Actor.Type.RequiresID() && c.Actor.ID == "") {
		return errors.New("wallet: an actor is required")
	}
	if c.CorrelationID == "" || len(c.CorrelationID) > audit.MaxCorrelationIDLength {
		return errors.New("wallet: a correlation ID is required")
	}
	if utf8.RuneCountInString(c.Reason) > audit.MaxReasonLength {
		return validation("The reason is too long.")
	}
	if utf8.RuneCountInString(c.Holder) > 200 {
		return errors.New("wallet: the holder is too long")
	}
	if c.Staff && (c.Actor.Type != events.ActorStaff || strings.TrimSpace(c.Reason) == "") {
		return zserrors.New(zserrors.CodeReasonRequired, "")
	}
	return nil
}

// holder is who owns the holds the call makes.
func (c Call) holder() string {
	if c.Holder != "" {
		return c.Holder
	}
	return string(c.Actor.Type) + ":" + c.Actor.ID
}

// owns reports whether the call may change r: its holder made it, or the
// call is a staff override.
func (c Call) owns(r Reservation) bool { return c.Staff || r.holder == c.holder() }

func (c Call) billing() billing.Call {
	return billing.Call{Actor: c.Actor, Reason: c.Reason, CorrelationID: c.CorrelationID}
}

// Reservation is a hold. AmountMinor is the amount held. A settled
// reservation names the Kill Bill object its settlement produced.
type Reservation struct {
	ID                     string     `json:"id"`
	ExternalRef            string     `json:"external_ref"`
	Attempt                int        `json:"attempt"`
	Label                  string     `json:"label"`
	Currency               string     `json:"currency"`
	AmountMinor            int64      `json:"amount_minor"`
	Status                 string     `json:"status"`
	LeaseExpiresAt         time.Time  `json:"lease_expires_at"`
	SettlementPending      bool       `json:"settlement_pending"`
	SettledMinor           *int64     `json:"settled_minor"`
	RatedBy                string     `json:"rated_by,omitempty"`
	KillBillRefType        string     `json:"killbill_ref_type,omitempty"`
	KillBillRef            string     `json:"killbill_ref,omitempty"`
	KillBillSubscriptionID string     `json:"killbill_subscription_id,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	SettledAt              *time.Time `json:"settled_at"`
	ReleasedAt             *time.Time `json:"released_at"`
	ExpiredAt              *time.Time `json:"expired_at"`

	id     idgen.UUID
	holder string
}

// ShortfallError is the cause of insufficient_credit: how much the request
// exceeds what is available, in minor units.
type ShortfallError struct {
	ShortfallMinor int64
}

func (e *ShortfallError) Error() string {
	return fmt.Sprintf("wallet: short of %d minor units", e.ShortfallMinor)
}

// Shortfall returns the shortfall of an insufficient_credit error.
func Shortfall(err error) (int64, bool) {
	var s *ShortfallError
	if errors.As(err, &s) {
		return s.ShortfallMinor, true
	}
	return 0, false
}

func insufficient(shortfall int64) error {
	return zserrors.Wrap(zserrors.CodeInsufficientCredit,
		fmt.Sprintf("The request exceeds the available credit by %d minor units.", shortfall), &ShortfallError{ShortfallMinor: shortfall})
}

var (
	refPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)
	errNotFound = zserrors.New(zserrors.CodeReservationNotFound, "")
	errNotHeld  = zserrors.New(zserrors.CodeReservationNotHeld, "")
	errPending  = zserrors.New(zserrors.CodeReservationSettlementPending, "")
	errConflict = zserrors.New(zserrors.CodeReservationConflict, "")
	errClosed   = zserrors.New(zserrors.CodeReservationClosed, "")
	errOnHold   = func(reasons []string) error {
		return zserrors.New(zserrors.CodeWalletOnHold, "The wallet is on hold for this tenant: "+strings.Join(reasons, ", ")+".")
	}
)

// ValidRef reports whether an external reference has the accepted form.
func ValidRef(ref string) bool { return refPattern.MatchString(ref) }

func validation(message string) error { return zserrors.New(zserrors.CodeValidation, message) }

func unavailable(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
}

func checkKey(ref string, attempt int) error {
	if !ValidRef(ref) {
		return validation("The external_ref must be 1 to 200 letters, digits, dots, colons, dashes or underscores, starting with a letter or digit.")
	}
	if attempt < 0 || attempt > MaxAttempt {
		return validation("The attempt must be 0 to 1000000.")
	}
	return nil
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

// lockTenant takes the tenant's wallet lock for the rest of tx.
func lockTenant(ctx context.Context, tx pgx.Tx, sc scope.Scope) error {
	return billing.LockWallet(ctx, tx, sc)
}

const reservationColumns = `id, external_ref, attempt, label, currency, amount_minor, status, lease_expires_at, settlement_pending,
       settled_minor, coalesce(rated_by, ''), coalesce(killbill_ref_type, ''), coalesce(killbill_ref, ''),
       coalesce(killbill_subscription_id::text, ''), created_at, updated_at, settled_at, released_at, expired_at, created_by_actor`

func scanReservation(row pgx.Row, prefix ...any) (Reservation, error) {
	var r Reservation
	dest := append(prefix, &r.id, &r.ExternalRef, &r.Attempt, &r.Label, &r.Currency, &r.AmountMinor, &r.Status, &r.LeaseExpiresAt,
		&r.SettlementPending, &r.SettledMinor, &r.RatedBy, &r.KillBillRefType, &r.KillBillRef, &r.KillBillSubscriptionID,
		&r.CreatedAt, &r.UpdatedAt, &r.SettledAt, &r.ReleasedAt, &r.ExpiredAt, &r.holder)
	if err := row.Scan(dest...); err != nil {
		return Reservation{}, err
	}
	r.ID = idgen.Format(idgen.Reservation, r.id)
	r.LeaseExpiresAt, r.CreatedAt, r.UpdatedAt = r.LeaseExpiresAt.UTC(), r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	for _, t := range []**time.Time{&r.SettledAt, &r.ReleasedAt, &r.ExpiredAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return r, nil
}

// get reads a reservation by its key, locked for the rest of tx when lock
// is set.
func get(ctx context.Context, q Querier, sc scope.Scope, ref string, attempt int, lock bool) (Reservation, error) {
	args, err := sc.Bind(ref, attempt)
	if err != nil {
		return Reservation{}, err
	}
	sql := `SELECT ` + reservationColumns + ` FROM reservations WHERE brand_id = $1 AND tenant_id = $2 AND external_ref = $3 AND attempt = $4`
	if lock {
		sql += ` FOR UPDATE`
	}
	r, err := scanReservation(q.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, errNotFound
	}
	if err != nil {
		return Reservation{}, unavailable(err)
	}
	return r, nil
}

// Get returns a reservation by its external reference and attempt.
func Get(ctx context.Context, q Querier, sc scope.Scope, ref string, attempt int) (Reservation, error) {
	if err := checkKey(ref, attempt); err != nil {
		return Reservation{}, errNotFound
	}
	return get(ctx, q, sc, ref, attempt, false)
}

// heldMinor is the sum of the tenant's held reservations, pending
// settlements included.
func heldMinor(ctx context.Context, q Querier, sc scope.Scope) (int64, error) {
	held, _, err := heldAndVersion(ctx, q, sc)
	return held, err
}

// heldAndVersion reads the sum of the tenant's held reservations and the
// tenant's wallet version in one statement, so both come from one
// snapshot: a settlement that committed its release of a hold has also
// committed its version.
func heldAndVersion(ctx context.Context, q Querier, sc scope.Scope) (int64, int64, error) {
	args, err := sc.Bind()
	if err != nil {
		return 0, 0, err
	}
	var held, version int64
	var overflow bool
	if err := q.QueryRow(ctx, `SELECT h.total > 9223372036854775807, least(h.total, 9223372036854775807)::bigint,
       coalesce((SELECT version FROM wallet_versions v WHERE v.brand_id = $1 AND v.tenant_id = $2), 0)
FROM (SELECT coalesce(sum(amount_minor), 0) AS total FROM reservations WHERE brand_id = $1 AND tenant_id = $2 AND status = 'held') h`,
		args...).Scan(&overflow, &held, &version); err != nil {
		return 0, 0, unavailable(err)
	}
	if overflow {
		return 0, 0, zserrors.New(zserrors.CodeInternal, "")
	}
	return held, version, nil
}

// walletVersion reads the tenant's wallet version.
func walletVersion(ctx context.Context, q Querier, sc scope.Scope) (int64, error) {
	_, v, err := heldAndVersion(ctx, q, sc)
	return v, err
}

// Available is the headroom of a wallet, the one number the view and
// Reserve use: the Kill Bill credit, minus the unbilled amount, minus the
// holds, plus the overdraft allowance. It is negative when the tenant is
// overspent.
func Available(creditMinor, unbilledMinor, heldMinor, overdraftMinor int64) int64 {
	return creditMinor - unbilledMinor - heldMinor + overdraftMinor
}

// event appends a reservation event in tx.
func event(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, r Reservation, kind string, details map[string]any) error {
	var encoded any
	if details != nil {
		b, err := json.Marshal(details)
		if err != nil {
			return err
		}
		encoded = b
	}
	args, err := sc.Bind(idgen.MustNewV7(), r.id, kind, r.AmountMinor, string(call.Actor.Type), nullable(call.Actor.ID), call.CorrelationID, encoded)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO reservation_events (brand_id, tenant_id, id, reservation_id, kind, amount_minor, actor_type,
    actor_id, correlation_id, details) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`, args...); err != nil {
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

// record audits a reservation write and, when eventType is set, writes the
// Core event, both in tx.
func record(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, action, eventType string, r Reservation, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	details["reservation_id"] = r.ID
	details["external_ref"] = r.ExternalRef
	details["attempt"] = r.Attempt
	details["amount_minor"] = r.AmountMinor
	details["currency"] = r.Currency
	if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: action, Reason: strings.TrimSpace(call.Reason),
		Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: details}); err != nil {
		return err
	}
	if eventType == "" {
		return nil
	}
	return writeEvent(ctx, tx, sc, call, eventType, events.Aggregate{Type: "reservation", ID: r.ID}, details)
}

func writeEvent(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, eventType string, agg events.Aggregate, payload any) error {
	env, err := events.New(ctx, events.Params{Type: eventType, SchemaVersion: 1, Producer: producer(), Actor: call.Actor,
		Brand: sc.Brand, Tenant: sc.Tenant, Aggregate: agg, CorrelationID: call.CorrelationID, Payload: payload})
	if err != nil {
		return err
	}
	if err := outbox.Write(ctx, tx, env); err != nil {
		return unavailable(err)
	}
	return nil
}

var (
	producerOnce sync.Once
	producerVal  events.Producer
)

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

// credit returns the tenant's Kill Bill credit and unbilled amount, from
// the process cache when the entry is younger than CreditTTL and was read
// at version, the wallet version read under the tenant lock. Otherwise it
// reads Kill Bill, the unbilled amount first, then the credit, so usage
// invoiced in between is counted twice rather than never. When Kill Bill
// cannot rate the unbilled usage, it is wallet_unavailable: a hold is never
// granted against an unknown amount.
func (s *Service) credit(ctx context.Context, acct *billing.TenantAccount, sc scope.Scope, version int64) (int64, int64, error) {
	now := s.now()
	if s.CreditTTL > 0 {
		s.mu.Lock()
		c, ok := s.cache[sc]
		s.mu.Unlock()
		if ok && c.version == version && now.Sub(c.at) < s.CreditTTL {
			return c.credit, c.unbilled, nil
		}
	}
	unbilled, err := acct.Unbilled(ctx)
	if err != nil {
		return 0, 0, zserrors.Wrap(zserrors.CodeWalletUnavailable, "", err)
	}
	_, credit, err := acct.Credit(ctx)
	if err != nil {
		return 0, 0, err
	}
	s.remember(sc, credit, unbilled, version, now)
	return credit, unbilled, nil
}

func (s *Service) remember(sc scope.Scope, credit, unbilled, version int64, at time.Time) {
	if s.CreditTTL <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[scope.Scope]cached{}
	}
	s.cache[sc] = cached{credit: credit, unbilled: unbilled, version: version, at: at}
}

// forget drops the cached credit of a tenant after Core changed it in Kill
// Bill.
func (s *Service) forget(sc scope.Scope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, sc)
}

// ReserveRequest asks to hold credit. Currency is optional and must be the
// account currency when set. Lease defaults to DefaultLease.
type ReserveRequest struct {
	ExternalRef string
	Attempt     int
	Label       string
	AmountMinor int64
	Currency    string
	Lease       time.Duration
}

// Reserve holds AmountMinor of the tenant's Kill Bill credit until the
// lease ends, in tx. It takes the tenant's wallet lock first. Available is
// the Kill Bill credit, less what Kill Bill will invoice at the end of the
// period and has not yet, minus the tenant's held reservations read in tx,
// plus the brand's overdraft allowance. The credit and the unbilled amount
// come from the process cache only while it was filled at the wallet
// version read in tx, and for at most CreditTTL. When Kill Bill cannot
// rate the unbilled amount, Reserve is wallet_unavailable. A request above
// what is available is insufficient_credit, with the shortfall in the
// error. A suspended tenant, a blocked Kill Bill account or an overdue
// state that disables entitlement is wallet_on_hold. These are read live on
// every call.
//
// The reservation is keyed by ExternalRef and Attempt, and belongs to the
// call's holder. A replay with an amount equal to or below the hold
// returns the hold unchanged with created false. A larger amount grows the
// hold by the difference, checked against what is available. A hold never
// shrinks. A replay of a reservation that was settled, released or expired
// is reservation_closed, and one whose settlement waits for Kill Bill
// cannot grow: reservation_settlement_pending. Another holder's
// reservation under the same key is reservation_not_found.
func (s *Service) Reserve(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, req ReserveRequest) (Reservation, bool, error) {
	if !s.Ready() {
		return Reservation{}, false, errNotReady
	}
	if err := call.validate(); err != nil {
		return Reservation{}, false, err
	}
	if call.Staff {
		return Reservation{}, false, zserrors.New(zserrors.CodePermissionDenied, "Staff do not reserve credit for a tenant.")
	}
	if err := checkKey(req.ExternalRef, req.Attempt); err != nil {
		return Reservation{}, false, err
	}
	if req.AmountMinor <= 0 || req.AmountMinor > MaxAmountMinor {
		return Reservation{}, false, validation("The amount_minor must be positive and at most 10000000000.")
	}
	label := strings.TrimSpace(req.Label)
	if utf8.RuneCountInString(label) > 200 || !utf8.ValidString(label) || strings.ContainsAny(label, "\x00\r\n") {
		return Reservation{}, false, validation("The label must be at most 200 characters of one line.")
	}
	lease, err := checkLease(req.Lease)
	if err != nil {
		return Reservation{}, false, err
	}
	if err := lockTenant(ctx, tx, sc); err != nil {
		return Reservation{}, false, err
	}
	existing, err := get(ctx, tx, sc, req.ExternalRef, req.Attempt, true)
	switch {
	case errors.Is(err, errNotFound):
	case err != nil:
		return Reservation{}, false, err
	case !call.owns(existing):
		return Reservation{}, false, errNotFound
	case existing.Status != StatusHeld:
		return Reservation{}, false, errClosed
	case req.AmountMinor <= existing.AmountMinor:
		return existing, false, nil
	case existing.SettlementPending:
		return Reservation{}, false, errPending
	}
	acct, err := s.Billing.TenantAccount(ctx, tx, sc)
	if err != nil {
		return Reservation{}, false, err
	}
	if acct.Tenant().Status != tenant.StatusActive {
		return Reservation{}, false, errOnHold([]string{billing.HoldTenantSuspended})
	}
	if req.Currency != "" && req.Currency != acct.Currency() {
		return Reservation{}, false, validation("The currency must be the tenant's billing currency, " + acct.Currency() + ".")
	}
	reasons, err := acct.HoldReasons(ctx)
	if err != nil {
		return Reservation{}, false, err
	}
	if (billing.Snapshot{HoldReasons: reasons}).HoldsEntitlement() {
		return Reservation{}, false, errOnHold(reasons)
	}
	held, version, err := heldAndVersion(ctx, tx, sc)
	if err != nil {
		return Reservation{}, false, err
	}
	credit, unbilled, err := s.credit(ctx, acct, sc, version)
	if err != nil {
		return Reservation{}, false, err
	}
	overdraft := acct.Brand().Manifest.Billing.Overdraft()
	need := req.AmountMinor
	if existing.id != idgen.Nil {
		need = req.AmountMinor - existing.AmountMinor
	}
	available := Available(credit, unbilled, held, overdraft)
	if need > available {
		return Reservation{}, false, insufficient(need - available)
	}
	now := s.now()
	var r Reservation
	if existing.id != idgen.Nil {
		args, _ := sc.Bind(existing.id, req.AmountMinor, now)
		r, err = scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET amount_minor = $4, updated_at = $5
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+reservationColumns, args...))
		if err != nil {
			return Reservation{}, false, unavailable(err)
		}
		if err := event(ctx, tx, sc, call, r, "grow", map[string]any{"from_minor": existing.AmountMinor}); err != nil {
			return Reservation{}, false, err
		}
		if err := record(ctx, tx, sc, call, "wallet.reservation.grown", "", r, map[string]any{"from_minor": existing.AmountMinor}); err != nil {
			return Reservation{}, false, err
		}
	} else {
		args, _ := sc.Bind(idgen.MustNewV7(), acct.Currency(), req.ExternalRef, req.Attempt, label, req.AmountMinor, now.Add(lease),
			string(call.Actor.Type), nullable(call.Actor.ID), call.CorrelationID, now, call.holder())
		r, err = scanReservation(tx.QueryRow(ctx, `INSERT INTO reservations (brand_id, tenant_id, id, currency, external_ref, attempt, label,
    amount_minor, lease_expires_at, actor_type, actor_id, correlation_id, created_at, updated_at, created_by_actor)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13, $14) RETURNING `+reservationColumns, args...))
		if err != nil {
			return Reservation{}, false, unavailable(err)
		}
		if err := event(ctx, tx, sc, call, r, "reserve", nil); err != nil {
			return Reservation{}, false, err
		}
		if err := record(ctx, tx, sc, call, "wallet.reservation.created", "", r, nil); err != nil {
			return Reservation{}, false, err
		}
	}
	if err := s.lowBalance(ctx, tx, sc, call, acct, available-need, acct.Currency()); err != nil {
		return Reservation{}, false, err
	}
	return r, existing.id == idgen.Nil, nil
}

// lowBalance emits wallet.low_balance in tx when available, as Available
// counts it, is below the brand's threshold, at most once per
// LowBalanceEvery for the tenant.
func (s *Service) lowBalance(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, acct *billing.TenantAccount, available int64, currency string) error {
	threshold := acct.Brand().Manifest.Billing.LowBalance()
	if threshold <= 0 || available >= threshold {
		return nil
	}
	ok, err := metering.ClaimAlert(ctx, tx, sc, EventLowBalance, s.now(), LowBalanceEvery)
	if err != nil || !ok {
		return err
	}
	payload := map[string]any{"available_minor": available, "threshold_minor": threshold, "currency": currency}
	if _, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: EventLowBalance, Outcome: audit.OutcomeSuccess,
		CorrelationID: call.CorrelationID, Details: payload}); err != nil {
		return err
	}
	return writeEvent(ctx, tx, sc, call, EventLowBalance, events.Aggregate{Type: "tenant", ID: sc.TenantID()}, payload)
}

// Extend renews the lease of a held reservation to lease from now, in tx.
// It works while a settlement waits for Kill Bill. Only the reservation's
// holder, or a staff override, may extend it: anyone else is
// reservation_not_found.
func (s *Service) Extend(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, ref string, attempt int, lease time.Duration) (Reservation, error) {
	if err := call.validate(); err != nil {
		return Reservation{}, err
	}
	if err := checkKey(ref, attempt); err != nil {
		return Reservation{}, errNotFound
	}
	lease, err := checkLease(lease)
	if err != nil {
		return Reservation{}, err
	}
	if err := lockTenant(ctx, tx, sc); err != nil {
		return Reservation{}, err
	}
	r, err := get(ctx, tx, sc, ref, attempt, true)
	if err != nil {
		return Reservation{}, err
	}
	if !call.owns(r) {
		return Reservation{}, errNotFound
	}
	if r.Status != StatusHeld {
		return Reservation{}, errNotHeld
	}
	holder := r.holder
	now := s.now()
	args, _ := sc.Bind(r.id, now.Add(lease), now)
	r, err = scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET lease_expires_at = $4, updated_at = $5
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+reservationColumns, args...))
	if err != nil {
		return Reservation{}, unavailable(err)
	}
	details := map[string]any{"lease_expires_at": r.LeaseExpiresAt.Format(time.RFC3339)}
	if err := event(ctx, tx, sc, call, r, "extend", details); err != nil {
		return Reservation{}, err
	}
	if err := staffOverride(ctx, tx, sc, call, r, holder, "extend"); err != nil {
		return Reservation{}, err
	}
	return r, record(ctx, tx, sc, call, "wallet.reservation.extended", "", r, details)
}

// Release returns a held reservation, in tx. Releasing a released
// reservation returns it. A reservation that settled or expired is
// reservation_not_held, and one whose settlement waits for Kill Bill is
// reservation_settlement_pending. Only the reservation's holder, or a staff
// override, may release it: anyone else is reservation_not_found.
func (s *Service) Release(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, ref string, attempt int) (Reservation, error) {
	if err := call.validate(); err != nil {
		return Reservation{}, err
	}
	if err := checkKey(ref, attempt); err != nil {
		return Reservation{}, errNotFound
	}
	if err := lockTenant(ctx, tx, sc); err != nil {
		return Reservation{}, err
	}
	r, err := get(ctx, tx, sc, ref, attempt, true)
	if err != nil {
		return Reservation{}, err
	}
	switch {
	case !call.owns(r):
		return Reservation{}, errNotFound
	case r.Status == StatusReleased:
		return r, nil
	case r.Status != StatusHeld:
		return Reservation{}, errNotHeld
	case r.SettlementPending:
		return Reservation{}, errPending
	}
	holder := r.holder
	now := s.now()
	args, _ := sc.Bind(r.id, now)
	r, err = scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET status = 'released', released_at = $4, updated_at = $4
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+reservationColumns, args...))
	if err != nil {
		return Reservation{}, unavailable(err)
	}
	if err := event(ctx, tx, sc, call, r, "release", nil); err != nil {
		return Reservation{}, err
	}
	if err := staffOverride(ctx, tx, sc, call, r, holder, "release"); err != nil {
		return Reservation{}, err
	}
	return r, record(ctx, tx, sc, call, "wallet.reservation.released", EventReservationReleased, r, nil)
}

// staffOverride audits a staff call that changed another holder's
// reservation, in tx.
func staffOverride(ctx context.Context, tx pgx.Tx, sc scope.Scope, call Call, r Reservation, holder, operation string) error {
	if !call.Staff || holder == call.holder() {
		return nil
	}
	_, err := audit.Write(ctx, tx, sc, audit.Entry{Actor: call.Actor, Action: ActionStaffOverride, Reason: strings.TrimSpace(call.Reason),
		Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID, Details: map[string]any{"reservation_id": r.ID,
			"external_ref": r.ExternalRef, "attempt": r.Attempt, "operation": operation, "holder": holder}})
	return err
}

// List returns the tenant's reservations, newest first, a page at a time.
// status and ref narrow the list when set.
func List(ctx context.Context, q Querier, sc scope.Scope, status, ref string, limit int, cursor string) ([]Reservation, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		return nil, "", zserrors.New(zserrors.CodeInvalidLimit, "")
	}
	if status != "" && status != StatusHeld && status != StatusSettled && status != StatusReleased && status != StatusExpired {
		return nil, "", validation("The status must be held, settled, released or expired.")
	}
	afterAt, afterID, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var at any
	if !afterAt.IsZero() {
		at = afterAt
	}
	args, err := sc.Bind(status, ref, at, afterID, limit+1)
	if err != nil {
		return nil, "", err
	}
	rows, err := q.Query(ctx, `SELECT `+reservationColumns+` FROM reservations
WHERE brand_id = $1 AND tenant_id = $2 AND ($3 = '' OR status = $3) AND ($4 = '' OR external_ref = $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6::uuid))
ORDER BY created_at DESC, id DESC LIMIT $7`, args...)
	if err != nil {
		return nil, "", unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Reservation, error) { return scanReservation(r) })
	if err != nil {
		return nil, "", unavailable(err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.id)
	}
	if out == nil {
		out = []Reservation{}
	}
	return out, next, nil
}

const cursorVersion = "r1"

func encodeCursor(at time.Time, id idgen.UUID) string {
	return hex.EncodeToString([]byte(cursorVersion + "|" + at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(c string) (time.Time, idgen.UUID, error) {
	if c == "" {
		return time.Time{}, idgen.Nil, nil
	}
	bad := zserrors.New(zserrors.CodeInvalidCursor, "")
	raw, err := hex.DecodeString(c)
	if err != nil || len(c) > 256 {
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

// digest is a short hex SHA-256 of parts.
func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
