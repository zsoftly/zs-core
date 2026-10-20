// Package audit records and queries the append-only audit trail.
//
// Every write in ZSoftly Core appends one audit event in the same transaction
// as the change, with the actor, brand, tenant, action, reason, outcome and a
// correlation ID. The table rejects UPDATE, DELETE and TRUNCATE, so a
// correction is a new event. Tenant events are written and queried with a
// full brand and tenant scope. Brand events, such as a brand apply or a staff
// grant, belong to one brand and no tenant, and have their own write and
// query. Queries are paginated with an opaque cursor, newest first.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Outcome is the result of an audited action.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomeDenied  Outcome = "denied"
)

// Valid reports whether o is a known outcome.
func (o Outcome) Valid() bool {
	return o == OutcomeSuccess || o == OutcomeFailure || o == OutcomeDenied
}

const (
	// MaxReasonLength caps the free-text reason in characters.
	MaxReasonLength = 1000
	// MaxCorrelationIDLength caps the correlation ID in bytes.
	MaxCorrelationIDLength = 128
	// MaxActorIDLength caps the actor ID in bytes.
	MaxActorIDLength = 255
	// MaxDetailsBytes caps the encoded details of one event.
	MaxDetailsBytes = 64 << 10
)

// Entry is what a caller records.
type Entry struct {
	Actor         events.Actor
	Action        string
	Reason        string
	Outcome       Outcome
	CorrelationID string
	// OccurredAt defaults to the current time.
	OccurredAt time.Time
	// Details is optional structured detail, such as a diff. It must never
	// hold secrets.
	Details map[string]any
}

// Event is a stored audit event.
type Event struct {
	ID            string
	BrandID       string
	TenantID      string // empty for a brand event
	Actor         events.Actor
	Action        string
	Reason        string
	Outcome       Outcome
	CorrelationID string
	OccurredAt    time.Time
	Details       map[string]any
}

func invalid(message string) error {
	return zserrors.New(zserrors.CodeValidation, message)
}

// Validate checks an entry.
func (e Entry) Validate() error {
	switch {
	case !e.Actor.Type.Valid():
		return invalid("The audit actor type is invalid.")
	case e.Actor.Type.RequiresID() && e.Actor.ID == "":
		return invalid("The audit actor ID is required.")
	case len(e.Actor.ID) > MaxActorIDLength:
		return invalid("The audit actor ID is too long.")
	case !events.ValidType(e.Action):
		return invalid("The audit action must be a dotted lowercase name.")
	case utf8.RuneCountInString(e.Reason) > MaxReasonLength || !utf8.ValidString(e.Reason):
		return invalid("The audit reason is too long or not valid text.")
	case !e.Outcome.Valid():
		return invalid("The audit outcome must be success, failure or denied.")
	case e.CorrelationID == "" || len(e.CorrelationID) > MaxCorrelationIDLength:
		return invalid("The audit correlation ID is required.")
	}
	return nil
}

func encodeDetails(details map[string]any) (any, error) {
	if details == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return nil, invalid("The audit details are not valid JSON.")
	}
	if len(encoded) > MaxDetailsBytes {
		return nil, invalid("The audit details are too large.")
	}
	return encoded, nil
}

const insertSQL = `
INSERT INTO audit_events (
    brand_id, tenant_id, id, actor_type, actor_id, action, reason, outcome, correlation_id, occurred_at, details
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

// Write appends entry in tx under scope s and returns the stored event.
func Write(ctx context.Context, tx pgx.Tx, s scope.Scope, entry Entry) (Event, error) {
	if err := s.Validate(); err != nil {
		return Event{}, err
	}
	return write(ctx, tx, s.Brand, s.Tenant, entry)
}

// WriteBrand appends a brand event, one that belongs to brand and to no
// tenant, in tx.
func WriteBrand(ctx context.Context, tx pgx.Tx, brand idgen.UUID, entry Entry) (Event, error) {
	if brand.IsNil() {
		return Event{}, scope.ErrMissing
	}
	return write(ctx, tx, brand, idgen.Nil, entry)
}

func write(ctx context.Context, tx pgx.Tx, brand, tenant idgen.UUID, entry Entry) (Event, error) {
	if tx == nil {
		return Event{}, fmt.Errorf("audit write requires a transaction")
	}
	if err := entry.Validate(); err != nil {
		return Event{}, err
	}
	details, err := encodeDetails(entry.Details)
	if err != nil {
		return Event{}, err
	}
	id, err := idgen.NewV7()
	if err != nil {
		return Event{}, err
	}
	occurredAt := entry.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	// Postgres stores microseconds; truncating here keeps the returned value
	// and the cursor identical to what a later query reads back.
	occurredAt = occurredAt.UTC().Truncate(time.Microsecond)

	var tenantArg any
	tenantID := ""
	if !tenant.IsNil() {
		tenantArg = tenant
		tenantID = idgen.Format(idgen.Tenant, tenant)
	}
	if _, err := tx.Exec(ctx, insertSQL, brand, tenantArg, id, string(entry.Actor.Type), nullable(entry.Actor.ID),
		entry.Action, nullable(entry.Reason), string(entry.Outcome), entry.CorrelationID, occurredAt, details); err != nil {
		return Event{}, fmt.Errorf("insert audit event: %w", err)
	}
	return Event{
		ID:            idgen.Format(idgen.Audit, id),
		BrandID:       idgen.Format(idgen.Brand, brand),
		TenantID:      tenantID,
		Actor:         entry.Actor,
		Action:        entry.Action,
		Reason:        entry.Reason,
		Outcome:       entry.Outcome,
		CorrelationID: entry.CorrelationID,
		OccurredAt:    occurredAt,
		Details:       entry.Details,
	}, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// Pagination limits.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Filter narrows a query. Zero values do not filter. OccurredAfter is
// inclusive and OccurredBefore is exclusive.
type Filter struct {
	ActorID        string
	Action         string
	OccurredAfter  time.Time
	OccurredBefore time.Time
	Limit          int
	Cursor         string
}

// Page is one page of results. NextCursor is empty on the last page.
type Page struct {
	Events     []Event
	NextCursor string
}

// Querier is the subset of pgxpool.Pool and pgx.Tx that Query needs.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Query returns audit events for scope s, newest first.
func Query(ctx context.Context, db Querier, s scope.Scope, f Filter) (Page, error) {
	args, err := s.Bind()
	if err != nil {
		return Page{}, err
	}
	return query(ctx, db, args, "brand_id = $1 AND tenant_id = $2", s.BrandID(), s.TenantID(), f)
}

// QueryBrand returns the brand events of brand, newest first. Tenant events
// are never included.
func QueryBrand(ctx context.Context, db Querier, brand idgen.UUID, f Filter) (Page, error) {
	if brand.IsNil() {
		return Page{}, scope.ErrMissing
	}
	return query(ctx, db, []any{brand}, "brand_id = $1 AND tenant_id IS NULL", idgen.Format(idgen.Brand, brand), "", f)
}

func query(ctx context.Context, db Querier, args []any, where, brandID, tenantID string, f Filter) (Page, error) {
	limit := f.Limit
	switch {
	case limit == 0:
		limit = DefaultLimit
	case limit < 0 || limit > MaxLimit:
		return Page{}, zserrors.New(zserrors.CodeInvalidLimit, fmt.Sprintf("The limit must be between 1 and %d.", MaxLimit))
	}
	if f.Action != "" && !events.ValidType(f.Action) {
		return Page{}, invalid("The action filter must be a dotted lowercase name.")
	}
	if len(f.ActorID) > MaxActorIDLength {
		return Page{}, invalid("The actor filter is too long.")
	}
	if !f.OccurredAfter.IsZero() && !f.OccurredBefore.IsZero() && !f.OccurredAfter.Before(f.OccurredBefore) {
		return Page{}, invalid("occurred_after must be earlier than occurred_before.")
	}

	var sql strings.Builder
	sql.WriteString(`SELECT id, actor_type, actor_id, action, reason, outcome, correlation_id, occurred_at, details
FROM audit_events
WHERE `)
	sql.WriteString(where)
	add := func(clause string, values ...any) {
		placeholders := make([]any, len(values))
		for i, v := range values {
			args = append(args, v)
			placeholders[i] = len(args)
		}
		fmt.Fprintf(&sql, clause, placeholders...)
	}
	if f.ActorID != "" {
		add(" AND actor_id = $%d", f.ActorID)
	}
	if f.Action != "" {
		add(" AND action = $%d", f.Action)
	}
	if !f.OccurredAfter.IsZero() {
		add(" AND occurred_at >= $%d", f.OccurredAfter.UTC())
	}
	if !f.OccurredBefore.IsZero() {
		add(" AND occurred_at < $%d", f.OccurredBefore.UTC())
	}
	if f.Cursor != "" {
		c, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, err
		}
		add(" AND (occurred_at, id) < ($%d, $%d)", c.occurredAt, c.id)
	}
	add(" ORDER BY occurred_at DESC, id DESC LIMIT $%d", limit+1)

	rows, err := db.Query(ctx, sql.String(), args...)
	if err != nil {
		return Page{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	defer rows.Close()

	page := Page{Events: make([]Event, 0, limit)}
	var lastID idgen.UUID
	for rows.Next() {
		var (
			id          idgen.UUID
			actorType   string
			actorID     *string
			reason      *string
			outcome     string
			eventAction string
			details     []byte
			ev          Event
		)
		if err := rows.Scan(&id, &actorType, &actorID, &eventAction, &reason, &outcome, &ev.CorrelationID, &ev.OccurredAt, &details); err != nil {
			return Page{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
		}
		if len(page.Events) == limit {
			page.NextCursor = encodeCursor(cursor{occurredAt: page.Events[limit-1].OccurredAt, id: lastID})
			break
		}
		ev.ID = idgen.Format(idgen.Audit, id)
		ev.BrandID = brandID
		ev.TenantID = tenantID
		ev.Actor = events.Actor{Type: events.ActorType(actorType), ID: deref(actorID)}
		ev.Action = eventAction
		ev.Reason = deref(reason)
		ev.Outcome = Outcome(outcome)
		ev.OccurredAt = ev.OccurredAt.UTC()
		if len(details) > 0 {
			if err := json.Unmarshal(details, &ev.Details); err != nil {
				return Page{}, zserrors.Wrap(zserrors.CodeInternal, "", err)
			}
		}
		page.Events = append(page.Events, ev)
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return Page{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return page, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
