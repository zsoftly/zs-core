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
// Every reservation carries a random owner token. Complete, MarkUnstored and
// Release match it, so a request whose lease was taken over by an identical
// retry can neither overwrite nor delete the retry's reservation. The command
// runs under a context whose deadline is the end of the lease: a command that
// honours its context stops before an identical retry can take the key over.
// Leases are compared against the clock of the process that reserves, so the
// clocks of Core instances must agree to well within PendingLeaseDuration.
//
// When the command succeeded but its result could not be stored, the key is
// marked completed_unstored and replays idempotency_key_in_progress until it
// expires, so the command does not run a second time. A result larger than
// MaxResultBytes is stored as a marker that replays
// idempotency_result_too_large.
//
// Library callers that want the reservation and the command to commit
// together pass a pgx.Tx to NewPostgresStore and run the command in that
// same transaction.
package idempotency

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// OwnerTokenBytes is the length of the random owner token of a
	// reservation.
	OwnerTokenBytes = 16
	// MaxFailureReasonLength caps the stored reason of a completed_unstored
	// key.
	MaxFailureReasonLength = 1000
	// bookkeepingTimeout bounds Complete, MarkUnstored and Release. They run
	// even when the caller's context is already cancelled, so a client that
	// disconnects does not leave a finished command unrecorded.
	bookkeepingTimeout = 10 * time.Second
)

// Status is the lifecycle state of a reserved key.
type Status string

const (
	StatusPending   Status = "pending"
	StatusCompleted Status = "completed"
	// StatusCompletedUnstored means the command ran but its result could not
	// be stored. The key replays idempotency_key_in_progress until it
	// expires.
	StatusCompletedUnstored Status = "completed_unstored"
)

// ErrNotOwner is returned by Store.Complete and Store.MarkUnstored when the
// reservation is no longer held by the given owner token: it was taken over
// by an identical retry after the lease ended, or it expired.
var ErrNotOwner = errors.New("idempotency reservation is not held by this owner")

var errorResultTooLarge = fmt.Errorf("idempotency result exceeds %d bytes", MaxResultBytes)

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

// Reservation is the outcome of Store.Reserve.
type Reservation struct {
	// Reserved is true when the caller now owns the key.
	Reserved bool
	// OwnerToken identifies the owner of a won key. Complete, MarkUnstored
	// and Release take it.
	OwnerToken []byte
	// LeaseExpiresAt is the end of the lease of a won key. After it an
	// identical retry may take the key over.
	LeaseExpiresAt time.Time
	// Existing is the record that blocks the key when Reserved is false.
	Existing Record
}

// Store persists reservations. Reserve must be atomic: at most one caller wins
// a key, so concurrent retries cannot both run the command.
type Store interface {
	// Reserve takes the key for fingerprint with a new owner token, or
	// returns the existing record that blocks it.
	Reserve(ctx context.Context, key Key, fingerprint string) (Reservation, error)
	// Complete stores the result of a pending key held by owner. It returns
	// ErrNotOwner when owner no longer holds the key.
	Complete(ctx context.Context, key Key, owner []byte, result Result) error
	// MarkUnstored records that the command of a pending key held by owner
	// ran but its result could not be stored. It returns ErrNotOwner when
	// owner no longer holds the key.
	MarkUnstored(ctx context.Context, key Key, owner []byte, reason string) error
	// Release drops a pending reservation held by owner so the command may be
	// retried. It does nothing when owner no longer holds the key.
	Release(ctx context.Context, key Key, owner []byte) error
}

// NewOwnerToken returns a random owner token for a reservation.
func NewOwnerToken() ([]byte, error) {
	token := make([]byte, OwnerTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("generate idempotency owner token: %w", err)
	}
	return token, nil
}

// tooLargeResult is the marker stored in place of a result larger than
// MaxResultBytes. It is the error envelope the replay returns.
var tooLargeResult = func() Result {
	status, envelope := zserrors.ToEnvelope(zserrors.New(zserrors.CodeIdempotencyResultTooLarge, ""))
	body, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	return Result{Status: status, Body: body}
}()

func isTooLargeMarker(r Result) bool {
	return r.Status == tooLargeResult.Status && bytes.Equal(r.Body, tooLargeResult.Body)
}

// Do runs fn at most once per key. A replay returns the stored result with
// replayed=true. When fn returns an error the reservation is released and the
// error is returned, so a failed command can be retried; a command that wants
// its failure replayed returns it as a Result instead.
//
// fn runs under a context that ends with the lease. When fn succeeds but its
// result cannot be stored, the key is marked completed_unstored so a retry
// gets idempotency_key_in_progress instead of running fn again.
func Do(ctx context.Context, store Store, key Key, fingerprint string, fn func(context.Context) (Result, error)) (Result, bool, error) {
	if err := key.Validate(); err != nil {
		return Result{}, false, err
	}
	reservation, err := store.Reserve(ctx, key, fingerprint)
	if err != nil {
		return Result{}, false, unavailable(err)
	}
	if !reservation.Reserved {
		existing := reservation.Existing
		switch {
		case existing.Fingerprint != fingerprint:
			return Result{}, false, zserrors.New(zserrors.CodeIdempotencyKeyConflict, "")
		case existing.Status != StatusCompleted:
			return Result{}, false, zserrors.New(zserrors.CodeIdempotencyKeyInProgress, "")
		case isTooLargeMarker(existing.Result):
			return Result{}, true, zserrors.New(zserrors.CodeIdempotencyResultTooLarge, "")
		default:
			return existing.Result, true, nil
		}
	}
	owner := reservation.OwnerToken

	runCtx, cancelRun := context.WithDeadline(ctx, reservation.LeaseExpiresAt)
	result, runErr := fn(runCtx)
	cancelRun()

	bookCtx, cancelBook := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancelBook()

	if runErr != nil {
		if releaseErr := store.Release(bookCtx, key, owner); releaseErr != nil {
			return Result{}, false, errors.Join(runErr, fmt.Errorf("release idempotency key: %w", releaseErr))
		}
		return Result{}, false, runErr
	}

	stored, tooLarge := result, false
	if len(result.Body) > MaxResultBytes {
		stored, tooLarge = tooLargeResult, true
	}
	if err := store.Complete(bookCtx, key, owner, stored); err != nil {
		if errors.Is(err, ErrNotOwner) {
			// An identical retry took the key over after the lease ended and
			// owns its result now.
			return Result{}, false, zserrors.Wrap(zserrors.CodeIdempotencyKeyInProgress, "", err)
		}
		if markErr := store.MarkUnstored(bookCtx, key, owner, failureReason(err)); markErr != nil {
			return Result{}, false, unavailable(errors.Join(err, fmt.Errorf("mark idempotency key unstored: %w", markErr)))
		}
		return Result{}, false, unavailable(err)
	}
	if tooLarge {
		return Result{}, false, zserrors.New(zserrors.CodeIdempotencyResultTooLarge, "")
	}
	return result, false, nil
}

func failureReason(err error) string {
	reason := "complete failed: " + err.Error()
	if len(reason) > MaxFailureReasonLength {
		reason = reason[:MaxFailureReasonLength]
	}
	return strings.ToValidUTF8(reason, "")
}

func unavailable(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
}
