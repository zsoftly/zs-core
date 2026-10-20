package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// DB is the subset of pgxpool.Pool and pgx.Tx the store needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PostgresStore is the durable Store backed by the idempotency_keys table.
type PostgresStore struct {
	db  DB
	now func() time.Time
}

// NewPostgresStore returns a store over a pool or a transaction.
func NewPostgresStore(db DB) *PostgresStore {
	return &PostgresStore{db: db, now: func() time.Time { return time.Now().UTC() }}
}

const reserveSQL = `
INSERT INTO idempotency_keys (
    brand_id, tenant_id, actor_id, idempotency_key, fingerprint, status,
    created_at, lease_expires_at, expires_at
) VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, $8)
ON CONFLICT (brand_id, tenant_id, actor_id, idempotency_key) DO UPDATE SET
    fingerprint = EXCLUDED.fingerprint,
    status = 'pending',
    result_status = NULL,
    result_body = NULL,
    created_at = EXCLUDED.created_at,
    completed_at = NULL,
    lease_expires_at = EXCLUDED.lease_expires_at,
    expires_at = EXCLUDED.expires_at
WHERE idempotency_keys.expires_at <= $6
   OR (idempotency_keys.status = 'pending'
       AND idempotency_keys.lease_expires_at <= $6
       AND idempotency_keys.fingerprint = EXCLUDED.fingerprint)
RETURNING 1`

const selectSQL = `
SELECT fingerprint, status, result_status, result_body, created_at, lease_expires_at, expires_at
FROM idempotency_keys
WHERE brand_id = $1 AND tenant_id = $2 AND actor_id = $3 AND idempotency_key = $4`

// Reserve implements Store.
func (s *PostgresStore) Reserve(ctx context.Context, key Key, fingerprint string) (Record, bool, error) {
	args, err := key.Scope.Bind(key.ActorID, key.Value)
	if err != nil {
		return Record{}, false, err
	}
	now := s.now()
	var marker int
	err = s.db.QueryRow(ctx, reserveSQL,
		append(args, fingerprint, now, now.Add(PendingLeaseDuration), now.Add(RecordRetention))...,
	).Scan(&marker)
	if err == nil {
		return Record{}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, fmt.Errorf("reserve idempotency key: %w", err)
	}

	var (
		record       Record
		status       string
		resultStatus pgtype.Int4
		leaseExpires pgtype.Timestamptz
	)
	if err := s.db.QueryRow(ctx, selectSQL, args...).Scan(
		&record.Fingerprint, &status, &resultStatus, &record.Result.Body,
		&record.CreatedAt, &leaseExpires, &record.ExpiresAt,
	); err != nil {
		return Record{}, false, fmt.Errorf("read idempotency key: %w", err)
	}
	record.Status = Status(status)
	if resultStatus.Valid {
		record.Result.Status = int(resultStatus.Int32)
	}
	if leaseExpires.Valid {
		record.LeaseExpiresAt = leaseExpires.Time
	}
	return record, false, nil
}

const completeSQL = `
UPDATE idempotency_keys
SET status = 'completed', result_status = $5, result_body = $6,
    completed_at = $7, lease_expires_at = NULL, expires_at = $8
WHERE brand_id = $1 AND tenant_id = $2 AND actor_id = $3 AND idempotency_key = $4
  AND status = 'pending'`

// Complete implements Store.
func (s *PostgresStore) Complete(ctx context.Context, key Key, result Result) error {
	if len(result.Body) > MaxResultBytes {
		return fmt.Errorf("idempotency result exceeds %d bytes", MaxResultBytes)
	}
	args, err := key.Scope.Bind(key.ActorID, key.Value)
	if err != nil {
		return err
	}
	now := s.now()
	body := result.Body
	if body == nil {
		body = []byte{}
	}
	tag, err := s.db.Exec(ctx, completeSQL, append(args, int32(result.Status), body, now, now.Add(RecordRetention))...)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("idempotency reservation is not pending")
	}
	return nil
}

const releaseSQL = `
DELETE FROM idempotency_keys
WHERE brand_id = $1 AND tenant_id = $2 AND actor_id = $3 AND idempotency_key = $4`

// Release implements Store.
func (s *PostgresStore) Release(ctx context.Context, key Key) error {
	args, err := key.Scope.Bind(key.ActorID, key.Value)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, releaseSQL, args...); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}
