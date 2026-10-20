package outbox

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// DefaultClaimDuration is how long a relay holds the events it claimed. When
// a relay crashes, its events become due again after this time.
const DefaultClaimDuration = 5 * time.Minute

// claimTokenBytes is the length of the random token of one claim.
const claimTokenBytes = 16

// ErrClaimLost means the event is no longer held by the claim: the claim
// expired and another relay claimed the event, or it was already published.
var ErrClaimLost = errors.New("outbox claim lost")

// DB is the subset of pgxpool.Pool the reader needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PostgresReader claims and acknowledges rows of the outbox table. Several
// readers can share one table: each due event is claimed by one of them.
type PostgresReader struct {
	db       DB
	claimFor time.Duration
}

// NewPostgresReader returns a reader over db with DefaultClaimDuration.
func NewPostgresReader(db DB) *PostgresReader {
	return &PostgresReader{db: db, claimFor: DefaultClaimDuration}
}

// WithClaimDuration returns a copy of r whose claims last d. A claim must
// outlast the delivery of a whole batch.
func (r *PostgresReader) WithClaimDuration(d time.Duration) *PostgresReader {
	c := *r
	if d > 0 {
		c.claimFor = d
	}
	return &c
}

// The claim locks due rows with FOR UPDATE SKIP LOCKED and stamps them with
// the claim token and expiry in the same statement, so two relays never claim
// the same row. The order matches outbox_dispatch_ready_idx.
const claimSQL = `
WITH due AS (
    SELECT id
    FROM outbox
    WHERE published_at IS NULL
      AND quarantined_at IS NULL
      AND next_attempt_at <= now()
      AND (claimed_until IS NULL OR claimed_until <= now())
    ORDER BY next_attempt_at, occurred_at, id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE outbox AS o
SET claimed_until = now() + make_interval(secs => $2), claim_token = $3
FROM due
WHERE o.id = due.id
RETURNING o.id, o.envelope, o.next_attempt_at, o.occurred_at`

// Claim takes up to limit due events, oldest due first, and holds them until
// the claim expires. A row whose envelope cannot be decoded or fails
// validation is quarantined with last_dispatch_error 'decode_failure' and
// reported in Claim.Quarantined; the other rows are returned.
func (r *PostgresReader) Claim(ctx context.Context, limit int) (Claim, error) {
	token := make([]byte, claimTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return Claim{}, fmt.Errorf("generate outbox claim token: %w", err)
	}
	// The deadline comes from this process's clock and is taken before the
	// claim, so it ends no later than the claim in the database.
	claim := Claim{Token: token, Until: time.Now().Add(r.claimFor)}

	rows, err := r.db.Query(ctx, claimSQL, limit, r.claimFor.Seconds(), token)
	if err != nil {
		return Claim{}, fmt.Errorf("claim outbox events: %w", err)
	}
	type claimed struct {
		id       idgen.UUID
		envelope events.Envelope
		due      time.Time
		occurred time.Time
	}
	var good []claimed
	for rows.Next() {
		var (
			c   claimed
			raw []byte
		)
		if err := rows.Scan(&c.id, &raw, &c.due, &c.occurred); err != nil {
			rows.Close()
			return Claim{}, fmt.Errorf("scan outbox row: %w", err)
		}
		if err := json.Unmarshal(raw, &c.envelope); err != nil || c.envelope.StorageID() != c.id {
			claim.Quarantined = append(claim.Quarantined, c.id)
			continue
		}
		good = append(good, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Claim{}, fmt.Errorf("read outbox: %w", err)
	}

	if len(claim.Quarantined) > 0 {
		ids := make([]string, len(claim.Quarantined))
		for i, id := range claim.Quarantined {
			ids[i] = id.String()
		}
		if _, err := r.db.Exec(ctx, quarantineDecodeSQL, ids, token); err != nil {
			return Claim{}, fmt.Errorf("quarantine undecodable outbox events: %w", err)
		}
	}
	sort.Slice(good, func(i, j int) bool {
		a, b := good[i], good[j]
		if !a.due.Equal(b.due) {
			return a.due.Before(b.due)
		}
		if !a.occurred.Equal(b.occurred) {
			return a.occurred.Before(b.occurred)
		}
		return a.id.String() < b.id.String()
	})
	for _, c := range good {
		claim.Events = append(claim.Events, c.envelope)
	}
	return claim, nil
}

const quarantineDecodeSQL = `
UPDATE outbox
SET quarantined_at = now(), last_dispatch_error = 'decode_failure',
    claimed_until = NULL, claim_token = NULL
WHERE id = ANY($1::uuid[]) AND claim_token = $2 AND published_at IS NULL`

const markPublishedSQL = `
UPDATE outbox
SET published_at = now(), claimed_until = NULL, claim_token = NULL
WHERE id = $1 AND claim_token = $2 AND published_at IS NULL`

// MarkPublished records the delivery of an event held by the claim token.
func (r *PostgresReader) MarkPublished(ctx context.Context, token []byte, id idgen.UUID) error {
	tag, err := r.db.Exec(ctx, markPublishedSQL, id, token)
	if err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}

// The retry delay is 5 seconds doubled per attempt, capped at 5 minutes. The
// stored error is a fixed classification; handler error text may hold
// provider detail and is only logged.
const recordFailureSQL = `
UPDATE outbox
SET dispatch_attempts = dispatch_attempts + 1,
    last_dispatch_error = 'handler_failure',
    next_attempt_at = now() + LEAST(
        interval '5 minutes',
        interval '5 seconds' * power(2, LEAST(dispatch_attempts, 6))
    ),
    quarantined_at = CASE WHEN dispatch_attempts + 1 >= $2 THEN now() ELSE NULL END,
    claimed_until = NULL,
    claim_token = NULL
WHERE id = $1
  AND claim_token = $3
  AND published_at IS NULL
RETURNING quarantined_at IS NOT NULL`

// RecordFailure schedules a retry for an event held by the claim token and
// reports whether the event is now quarantined because it reached
// maxAttempts.
func (r *PostgresReader) RecordFailure(ctx context.Context, token []byte, id idgen.UUID, maxAttempts int) (bool, error) {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	var quarantined bool
	err := r.db.QueryRow(ctx, recordFailureSQL, id, maxAttempts, token).Scan(&quarantined)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrClaimLost
	}
	if err != nil {
		return false, fmt.Errorf("record outbox failure: %w", err)
	}
	return quarantined, nil
}

// A deferred event waits the same backoff as a failure but spends no attempt.
const deferSQL = `
UPDATE outbox
SET next_attempt_at = now() + LEAST(
        interval '5 minutes',
        interval '5 seconds' * power(2, LEAST(dispatch_attempts, 6))
    ),
    claimed_until = NULL,
    claim_token = NULL
WHERE id = $1
  AND claim_token = $2
  AND published_at IS NULL`

// Defer moves the next attempt of an event held by the claim token out by the
// retry backoff without counting an attempt, so a deferred event does not hold
// up the events behind it.
func (r *PostgresReader) Defer(ctx context.Context, token []byte, id idgen.UUID) error {
	tag, err := r.db.Exec(ctx, deferSQL, id, token)
	if err != nil {
		return fmt.Errorf("defer outbox event: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}
