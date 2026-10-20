// Package idempotency makes state-changing commands safe to retry.
//
// A client sends an idempotency_key field in the request body. The key is
// scoped to the brand, the tenant and the actor, so two callers can never see
// each other's results. The first request reserves the key, runs the command
// once and stores the result. A retry with the same key and the same payload
// returns the stored result. A request with the same key and a different
// payload fails with idempotency_key_conflict. A retry that arrives while the
// first request still runs fails with idempotency_key_in_progress, which is
// retryable.
//
// Library callers that want the reservation and the command to commit
// together pass a pgx.Tx to NewPostgresStore and run the command in that
// same transaction.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/scope"
)

const (
	// FieldName is the request body field that carries the key.
	FieldName = "idempotency_key"
	// MinKeyLength and MaxKeyLength bound the key length in bytes.
	MinKeyLength = 8
	MaxKeyLength = 255
	// PendingLeaseDuration is how long a reservation blocks retries before an
	// identical retry may take it over.
	PendingLeaseDuration = 2 * time.Minute
	// RecordRetention is how long a completed result is replayed.
	RecordRetention = 24 * time.Hour
	// MaxResultBytes caps the stored result body.
	MaxResultBytes = 256 * 1024
)

// Status is the lifecycle state of a reserved key.
type Status string

const (
	StatusPending   Status = "pending"
	StatusCompleted Status = "completed"
)

// Key identifies one idempotent command: the brand and tenant scope, the
// actor and the client-supplied value.
type Key struct {
	Scope   scope.Scope
	ActorID string
	Value   string
}

// Validate checks the scope, the actor and the key value.
func (k Key) Validate() error {
	if err := k.Scope.Validate(); err != nil {
		return err
	}
	if k.ActorID == "" || len(k.ActorID) > MaxKeyLength {
		return zserrors.New(zserrors.CodeValidation, "An actor is required for idempotent commands.")
	}
	return ValidateKey(k.Value)
}

// ValidateKey checks that a key is 8 to 255 printable ASCII characters
// without spaces.
func ValidateKey(value string) error {
	if len(value) < MinKeyLength || len(value) > MaxKeyLength {
		return zserrors.New(zserrors.CodeIdempotencyKeyInvalid, "")
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return zserrors.New(zserrors.CodeIdempotencyKeyInvalid, "")
		}
	}
	return nil
}

// Fingerprint returns a digest of an operation name and its JSON payload.
// The payload is canonicalised first, so field order and whitespace do not
// change the fingerprint while any change of value does.
func Fingerprint(operation string, payload []byte) (string, error) {
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return "", zserrors.Wrap(zserrors.CodeBadRequest, "The request body is not valid JSON.", err)
	}
	h := sha256.New()
	h.Write([]byte(operation))
	h.Write([]byte{0})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func canonicalJSON(payload []byte) ([]byte, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return []byte("null"), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	return json.Marshal(value)
}

// Record is the stored state of one key.
type Record struct {
	Fingerprint    string
	Status         Status
	Result         Result
	CreatedAt      time.Time
	LeaseExpiresAt time.Time
	ExpiresAt      time.Time
}

// Result is the outcome of a command that is replayed on retry. Status is
// the HTTP status for HTTP callers and any caller-defined value for library
// callers.
type Result struct {
	Status int
	Body   []byte
}

// Store persists reservations. Reserve must be atomic: at most one caller wins
// a key, so concurrent retries cannot both run the command.
type Store interface {
	// Reserve takes the key for fingerprint. It returns reserved=true when the
	// caller now owns the key, or the existing record otherwise.
	Reserve(ctx context.Context, key Key, fingerprint string) (existing Record, reserved bool, err error)
	// Complete stores the result of a reserved key.
	Complete(ctx context.Context, key Key, result Result) error
	// Release drops a reservation so the command may be retried.
	Release(ctx context.Context, key Key) error
}

// Do runs fn at most once per key. A replay returns the stored result with
// replayed=true. When fn returns an error the reservation is released and the
// error is returned, so a failed command can be retried; a command that wants
// its failure replayed returns it as a Result instead.
func Do(ctx context.Context, store Store, key Key, fingerprint string, fn func(context.Context) (Result, error)) (Result, bool, error) {
	if err := key.Validate(); err != nil {
		return Result{}, false, err
	}
	existing, reserved, err := store.Reserve(ctx, key, fingerprint)
	if err != nil {
		return Result{}, false, unavailable(err)
	}
	if !reserved {
		switch {
		case existing.Fingerprint != fingerprint:
			return Result{}, false, zserrors.New(zserrors.CodeIdempotencyKeyConflict, "")
		case existing.Status != StatusCompleted:
			return Result{}, false, zserrors.New(zserrors.CodeIdempotencyKeyInProgress, "")
		default:
			return existing.Result, true, nil
		}
	}

	result, runErr := fn(ctx)
	if runErr != nil {
		if releaseErr := store.Release(ctx, key); releaseErr != nil {
			return Result{}, false, errors.Join(runErr, fmt.Errorf("release idempotency key: %w", releaseErr))
		}
		return Result{}, false, runErr
	}
	if len(result.Body) > MaxResultBytes {
		_ = store.Release(ctx, key)
		return Result{}, false, zserrors.New(zserrors.CodeInternal, "")
	}
	if err := store.Complete(ctx, key, result); err != nil {
		return Result{}, false, unavailable(err)
	}
	return result, false, nil
}

func unavailable(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
}
