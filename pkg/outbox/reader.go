package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// DB is the subset of pgxpool.Pool the reader needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PostgresReader reads the outbox table.
type PostgresReader struct {
	db DB
}

// NewPostgresReader returns a reader over db.
func NewPostgresReader(db DB) *PostgresReader {
	return &PostgresReader{db: db}
}

const selectUnpublishedSQL = `
SELECT envelope
FROM outbox
WHERE published_at IS NULL
  AND quarantined_at IS NULL
  AND next_attempt_at <= now()
ORDER BY occurred_at, id
LIMIT $1`

// Unpublished returns up to limit events that are due for delivery, oldest
// first.
func (r *PostgresReader) Unpublished(ctx context.Context, limit int) ([]events.Envelope, error) {
	rows, err := r.db.Query(ctx, selectUnpublishedSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	defer rows.Close()

	var envelopes []events.Envelope
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		var envelope events.Envelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, fmt.Errorf("decode outbox envelope: %w", err)
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes, rows.Err()
}

const markPublishedSQL = `UPDATE outbox SET published_at = now() WHERE id = ANY($1::uuid[]) AND published_at IS NULL`

// MarkPublished records successful delivery of the given events.
func (r *PostgresReader) MarkPublished(ctx context.Context, ids []idgen.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = id.String()
	}
	if _, err := r.db.Exec(ctx, markPublishedSQL, values); err != nil {
		return fmt.Errorf("mark outbox published: %w", err)
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
    quarantined_at = CASE WHEN dispatch_attempts + 1 >= $2 THEN now() ELSE NULL END
WHERE id = $1
  AND published_at IS NULL
RETURNING quarantined_at IS NOT NULL`

// RecordFailure schedules a retry for id and reports whether the event is now
// quarantined because it reached maxAttempts.
func (r *PostgresReader) RecordFailure(ctx context.Context, id idgen.UUID, maxAttempts int) (bool, error) {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	var quarantined bool
	if err := r.db.QueryRow(ctx, recordFailureSQL, id, maxAttempts).Scan(&quarantined); err != nil {
		return false, fmt.Errorf("record outbox failure: %w", err)
	}
	return quarantined, nil
}
