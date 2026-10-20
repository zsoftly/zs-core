// Package audit records and queries the append-only audit trail.
//
// Every write in ZSoftly Core appends one audit event in the same transaction
// as the change, with the actor, brand, tenant, action, reason, outcome and a
// correlation ID. The table rejects UPDATE, DELETE and TRUNCATE, so a
// correction is a new event. Queries are always scoped to one brand and one
// tenant and are paginated with an opaque cursor, newest first.
package audit

import (
	"context"
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
}

// Event is a stored audit event.
type Event struct {
	ID            string
	BrandID       string
	TenantID      string
	Actor         events.Actor
	Action        string
	Reason        string
	Outcome       Outcome
	CorrelationID string
	OccurredAt    time.Time
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

const insertSQL = `
INSERT INTO audit_events (
    brand_id, tenant_id, id, actor_type, actor_id, action, reason, outcome, correlation_id, occurred_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// Write appends entry in tx under scope s and returns the stored event.
func Write(ctx context.Context, tx pgx.Tx, s scope.Scope, entry Entry) (Event, error) {
	if tx == nil {
		return Event{}, fmt.Errorf("audit write requires a transaction")
	}
	if err := entry.Validate(); err != nil {
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

	args, err := s.Bind(id, string(entry.Actor.Type), nullable(entry.Actor.ID), entry.Action,
		nullable(entry.Reason), string(entry.Outcome), entry.CorrelationID, occurredAt)
	if err != nil {
		return Event{}, err
	}
	if _, err := tx.Exec(ctx, insertSQL, args...); err != nil {
		return Event{}, fmt.Errorf("insert audit event: %w", err)
	}
	return Event{
		ID:            idgen.Format(idgen.Audit, id),
		BrandID:       s.BrandID(),
		TenantID:      s.TenantID(),
		Actor:         entry.Actor,
		Action:        entry.Action,
		Reason:        entry.Reason,
		Outcome:       entry.Outcome,
		CorrelationID: entry.CorrelationID,
		OccurredAt:    occurredAt,
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

	args, err := s.Bind()
	if err != nil {
		return Page{}, err
	}
	var sql strings.Builder
	sql.WriteString(`SELECT id, actor_type, actor_id, action, reason, outcome, correlation_id, occurred_at
FROM audit_events
WHERE brand_id = $1 AND tenant_id = $2`)
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
			ev          Event
		)
		if err := rows.Scan(&id, &actorType, &actorID, &eventAction, &reason, &outcome, &ev.CorrelationID, &ev.OccurredAt); err != nil {
			return Page{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
		}
		if len(page.Events) == limit {
			page.NextCursor = encodeCursor(cursor{occurredAt: page.Events[limit-1].OccurredAt, id: lastID})
			break
		}
		ev.ID = idgen.Format(idgen.Audit, id)
		ev.BrandID = s.BrandID()
		ev.TenantID = s.TenantID()
		ev.Actor = events.Actor{Type: events.ActorType(actorType), ID: deref(actorID)}
		ev.Action = eventAction
		ev.Reason = deref(reason)
		ev.Outcome = Outcome(outcome)
		ev.OccurredAt = ev.OccurredAt.UTC()
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
