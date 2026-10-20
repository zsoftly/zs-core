//go:build integration

package idempotency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

func pgKey(t *testing.T) Key {
	return Key{Scope: postgrestest.NewScope(t), ActorID: "user:alice", Value: "order-" + idgen.MustNewV7().String()}
}

func TestPostgresStoreRunsOnceAndReplays(t *testing.T) {
	db := postgrestest.New(t)
	store := NewPostgresStore(db.Pool)
	ctx := context.Background()
	key := pgKey(t)
	fp := mustFingerprint(t, "op", `{"amount":100}`)
	var calls int32
	fn := func(context.Context) (Result, error) {
		atomic.AddInt32(&calls, 1)
		return Result{Status: 201, Body: []byte(`{"id":"ord_1"}`)}, nil
	}
	if _, replayed, err := Do(ctx, store, key, fp, fn); err != nil || replayed {
		t.Fatalf("first: %v %v", replayed, err)
	}
	res, replayed, err := Do(ctx, store, key, fp, fn)
	if err != nil || !replayed || res.Status != 201 || string(res.Body) != `{"id":"ord_1"}` {
		t.Fatalf("replay = %+v %v %v", res, replayed, err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}

	_, _, err = Do(ctx, store, key, mustFingerprint(t, "op", `{"amount":200}`), fn)
	if !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyConflict) {
		t.Fatalf("conflict err = %v", err)
	}
}

func TestPostgresStoreScopesKeys(t *testing.T) {
	db := postgrestest.New(t)
	store := NewPostgresStore(db.Pool)
	ctx := context.Background()
	base := pgKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	otherBrand, otherTenant, otherActor := base, base, base
	otherBrand.Scope.Brand = idgen.MustNewV7()
	otherTenant.Scope.Tenant = idgen.MustNewV7()
	otherActor.ActorID = "user:bob"
	for _, k := range []Key{base, otherBrand, otherTenant, otherActor} {
		if r, err := store.Reserve(ctx, k, fp); err != nil || !r.Reserved {
			t.Fatalf("Reserve(%+v) = %v, %v", k, r.Reserved, err)
		}
	}
}

func TestPostgresStoreConcurrentReserveHasOneWinner(t *testing.T) {
	db := postgrestest.New(t)
	store := NewPostgresStore(db.Pool)
	key := pgKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	var winners int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := store.Reserve(context.Background(), key, fp)
			if err != nil {
				t.Error(err)
			}
			if r.Reserved {
				atomic.AddInt32(&winners, 1)
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("winners = %d", winners)
	}
	_, _, err := Do(context.Background(), store, key, fp, func(context.Context) (Result, error) {
		t.Fatal("ran while pending")
		return Result{}, nil
	})
	if !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyInProgress) {
		t.Fatalf("err = %v", err)
	}
}

func TestPostgresStoreReleaseAndLeaseExpiry(t *testing.T) {
	db := postgrestest.New(t)
	store := NewPostgresStore(db.Pool)
	ctx := context.Background()
	key := pgKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)

	boom := errors.New("provider unavailable")
	if _, _, err := Do(ctx, store, key, fp, func(context.Context) (Result, error) { return Result{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if r, err := store.Reserve(ctx, key, fp); err != nil || !r.Reserved {
		t.Fatalf("reserve after release = %v %v", r.Reserved, err)
	}

	now := time.Now().UTC().Add(PendingLeaseDuration + time.Minute)
	store.now = func() time.Time { return now }
	if r, _ := store.Reserve(ctx, key, mustFingerprint(t, "op", `{"a":2}`)); r.Reserved {
		t.Fatal("different payload took over a stale lease")
	}
	if r, err := store.Reserve(ctx, key, fp); err != nil || !r.Reserved {
		t.Fatalf("identical retry after lease expiry = %v %v", r.Reserved, err)
	}
}

func TestPostgresStoreInsideCallerTransaction(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	key := pgKey(t)
	fp := mustFingerprint(t, "op", `{}`)

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Do(ctx, NewPostgresStore(tx), key, fp, func(context.Context) (Result, error) {
		return Result{Status: 200, Body: []byte(`{}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := NewPostgresStore(db.Pool).Reserve(ctx, key, fp); err != nil || !r.Reserved {
		t.Fatalf("rolled-back reservation still blocks the key: %v %v", r.Reserved, err)
	}

	err = pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, err := NewPostgresStore(tx).Reserve(ctx, pgKey(t), fp)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// laterStore is a second instance whose clock is past the lease of the first.
func laterStore(db DB) *PostgresStore {
	s := NewPostgresStore(db)
	s.now = func() time.Time { return time.Now().UTC().Add(PendingLeaseDuration + time.Minute) }
	return s
}

func TestPostgresLeaseTakeoverKeepsTheNewOwnersResult(t *testing.T) {
	db := postgrestest.New(t)
	storeA, storeB := NewPostgresStore(db.Pool), laterStore(db.Pool)
	ctx := context.Background()
	key := pgKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)

	_, _, errA := Do(ctx, storeA, key, fp, func(context.Context) (Result, error) {
		resB, replayed, errB := Do(ctx, storeB, key, fp, func(context.Context) (Result, error) {
			return Result{Status: 201, Body: []byte(`"b"`)}, nil
		})
		if errB != nil || replayed || string(resB.Body) != `"b"` {
			t.Errorf("B = %+v %v %v", resB, replayed, errB)
		}
		return Result{Status: 201, Body: []byte(`"a"`)}, nil
	})
	if !zserrors.IsCode(errA, zserrors.CodeIdempotencyKeyInProgress) {
		t.Fatalf("A after losing the lease = %v, want idempotency_key_in_progress", errA)
	}
	res, replayed, err := Do(ctx, storeA, key, fp, func(context.Context) (Result, error) {
		t.Fatal("command ran a third time")
		return Result{}, nil
	})
	if err != nil || !replayed || string(res.Body) != `"b"` {
		t.Fatalf("replay = %+v %v %v, want B's result", res, replayed, err)
	}
}

func TestPostgresReleaseAfterTakeoverKeepsTheNewReservation(t *testing.T) {
	db := postgrestest.New(t)
	storeA, storeB := NewPostgresStore(db.Pool), laterStore(db.Pool)
	ctx := context.Background()
	key := pgKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)

	var b Reservation
	boom := errors.New("provider down")
	_, _, errA := Do(ctx, storeA, key, fp, func(context.Context) (Result, error) {
		var err error
		if b, err = storeB.Reserve(ctx, key, fp); err != nil || !b.Reserved {
			t.Fatalf("B could not take over: %+v %v", b, err)
		}
		return Result{}, boom
	})
	if !errors.Is(errA, boom) {
		t.Fatalf("A = %v", errA)
	}
	if r, err := storeA.Reserve(ctx, key, fp); err != nil || r.Reserved || r.Existing.Status != StatusPending {
		t.Fatalf("A's release removed B's reservation: %+v %v", r, err)
	}
	if err := storeB.Complete(ctx, key, b.OwnerToken, Result{Status: 200, Body: []byte(`{}`)}); err != nil {
		t.Fatalf("B complete = %v", err)
	}
	if err := storeA.Complete(ctx, key, []byte("0123456789abcdef"), Result{Status: 200}); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("complete with a foreign owner token = %v, want ErrNotOwner", err)
	}
}

// failOnceComplete fails the first Complete, as a lost connection would.
type failOnceComplete struct {
	*PostgresStore
	failed atomic.Bool
}

func (s *failOnceComplete) Complete(ctx context.Context, key Key, owner []byte, result Result) error {
	if s.failed.CompareAndSwap(false, true) {
		return errors.New("connection reset by peer")
	}
	return s.PostgresStore.Complete(ctx, key, owner, result)
}

func TestPostgresCompleteFailureMarksUnstored(t *testing.T) {
	db := postgrestest.New(t)
	store := &failOnceComplete{PostgresStore: NewPostgresStore(db.Pool)}
	ctx := context.Background()
	key := pgKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	var calls int32
	fn := func(context.Context) (Result, error) {
		atomic.AddInt32(&calls, 1)
		return Result{Status: 201, Body: []byte(`{}`)}, nil
	}
	if _, _, err := Do(ctx, store, key, fp, fn); !zserrors.IsCode(err, zserrors.CodeDatabaseUnavailable) {
		t.Fatalf("first = %v", err)
	}
	var status, reason string
	if err := db.Pool.QueryRow(ctx,
		`SELECT status, failure_reason FROM idempotency_keys WHERE brand_id = $1 AND tenant_id = $2 AND actor_id = $3 AND idempotency_key = $4`,
		key.Scope.Brand, key.Scope.Tenant, key.ActorID, key.Value).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != string(StatusCompletedUnstored) || reason == "" {
		t.Fatalf("row = %s %q", status, reason)
	}
	// Even after the lease would have ended, the retry does not run again.
	if _, _, err := Do(ctx, laterStore(db.Pool), key, fp, fn); !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyInProgress) {
		t.Fatalf("retry = %v, want idempotency_key_in_progress", err)
	}
	if calls != 1 {
		t.Fatalf("command ran %d times", calls)
	}
}

func TestPostgresTooLargeResultReplaysWithoutRunning(t *testing.T) {
	db := postgrestest.New(t)
	store := NewPostgresStore(db.Pool)
	ctx := context.Background()
	key := pgKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	var calls int32
	fn := func(context.Context) (Result, error) {
		atomic.AddInt32(&calls, 1)
		return Result{Status: 200, Body: make([]byte, MaxResultBytes+1)}, nil
	}
	if _, _, err := Do(ctx, store, key, fp, fn); !zserrors.IsCode(err, zserrors.CodeIdempotencyResultTooLarge) {
		t.Fatalf("first = %v", err)
	}
	if _, replayed, err := Do(ctx, store, key, fp, fn); !zserrors.IsCode(err, zserrors.CodeIdempotencyResultTooLarge) || !replayed {
		t.Fatalf("replay = %v %v", replayed, err)
	}
	if calls != 1 {
		t.Fatalf("command ran %d times", calls)
	}
}
