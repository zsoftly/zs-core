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
    created_at, lease_expires_at, expires_at, owner_token
) VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, $8, $9)
ON CONFLICT (brand_id, tenant_id, actor_id, idempotency_key) DO UPDATE SET
    fingerprint = EXCLUDED.fingerprint,
    status = 'pending',
    result_status = NULL,
    result_body = NULL,
    created_at = EXCLUDED.created_at,
    completed_at = NULL,
    lease_expires_at = EXCLUDED.lease_expires_at,
    expires_at = EXCLUDED.expires_at,
    owner_token = EXCLUDED.owner_token,
    failure_reason = NULL
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
func (s *PostgresStore) Reserve(ctx context.Context, key Key, fingerprint string) (Reservation, error) {
	args, err := key.Scope.Bind(key.ActorID, key.Value)
	if err != nil {
		return Reservation{}, err
	}
	owner, err := NewOwnerToken()
	if err != nil {
		return Reservation{}, err
	}
	now := s.now()
	lease := now.Add(PendingLeaseDuration)
	var marker int
	err = s.db.QueryRow(ctx, reserveSQL,
		append(args, fingerprint, now, lease, now.Add(RecordRetention), owner)...,
	).Scan(&marker)
	if err == nil {
		return Reservation{Reserved: true, OwnerToken: owner, LeaseExpiresAt: lease}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, fmt.Errorf("reserve idempotency key: %w", err)
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
		return Reservation{}, fmt.Errorf("read idempotency key: %w", err)
	}
	record.Status = Status(status)
	if resultStatus.Valid {
		record.Result.Status = int(resultStatus.Int32)
	}
	if leaseExpires.Valid {
		record.LeaseExpiresAt = leaseExpires.Time
	}
	return Reservation{Existing: record}, nil
}

const completeSQL = `
UPDATE idempotency_keys
SET status = 'completed', result_status = $6, result_body = $7,
    completed_at = $8, lease_expires_at = NULL, expires_at = $9
WHERE brand_id = $1 AND tenant_id = $2 AND actor_id = $3 AND idempotency_key = $4
  AND status = 'pending' AND owner_token = $5`

// Complete implements Store.
func (s *PostgresStore) Complete(ctx context.Context, key Key, owner []byte, result Result) error {
	if len(result.Body) > MaxResultBytes {
		return errorResultTooLarge
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
	tag, err := s.db.Exec(ctx, completeSQL, append(args, owner, int32(result.Status), body, now, now.Add(RecordRetention))...)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotOwner
	}
	return nil
}

const markUnstoredSQL = `
UPDATE idempotency_keys
SET status = 'completed_unstored', failure_reason = $6,
    completed_at = $7, lease_expires_at = NULL, expires_at = $8
WHERE brand_id = $1 AND tenant_id = $2 AND actor_id = $3 AND idempotency_key = $4
  AND status = 'pending' AND owner_token = $5`

// MarkUnstored implements Store.
func (s *PostgresStore) MarkUnstored(ctx context.Context, key Key, owner []byte, reason string) error {
	args, err := key.Scope.Bind(key.ActorID, key.Value)
	if err != nil {
		return err
	}
	if len(reason) > MaxFailureReasonLength {
		reason = reason[:MaxFailureReasonLength]
	}
	now := s.now()
	tag, err := s.db.Exec(ctx, markUnstoredSQL, append(args, owner, reason, now, now.Add(RecordRetention))...)
	if err != nil {
		return fmt.Errorf("mark idempotency key unstored: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotOwner
	}
	return nil
}

const releaseSQL = `
DELETE FROM idempotency_keys
WHERE brand_id = $1 AND tenant_id = $2 AND actor_id = $3 AND idempotency_key = $4
  AND status = 'pending' AND owner_token = $5`

// Release implements Store.
func (s *PostgresStore) Release(ctx context.Context, key Key, owner []byte) error {
	args, err := key.Scope.Bind(key.ActorID, key.Value)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, releaseSQL, append(args, owner)...); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}
