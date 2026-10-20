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
		if _, reserved, err := store.Reserve(ctx, k, fp); err != nil || !reserved {
			t.Fatalf("Reserve(%+v) = %v, %v", k, reserved, err)
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
			_, reserved, err := store.Reserve(context.Background(), key, fp)
			if err != nil {
				t.Error(err)
			}
			if reserved {
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
	if _, reserved, err := store.Reserve(ctx, key, fp); err != nil || !reserved {
		t.Fatalf("reserve after release = %v %v", reserved, err)
	}

	now := time.Now().UTC().Add(PendingLeaseDuration + time.Minute)
	store.now = func() time.Time { return now }
	if _, reserved, _ := store.Reserve(ctx, key, mustFingerprint(t, "op", `{"a":2}`)); reserved {
		t.Fatal("different payload took over a stale lease")
	}
	if _, reserved, err := store.Reserve(ctx, key, fp); err != nil || !reserved {
		t.Fatalf("identical retry after lease expiry = %v %v", reserved, err)
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
	if _, reserved, err := NewPostgresStore(db.Pool).Reserve(ctx, key, fp); err != nil || !reserved {
		t.Fatalf("rolled-back reservation still blocks the key: %v %v", reserved, err)
	}

	err = pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		_, _, err := NewPostgresStore(tx).Reserve(ctx, pgKey(t), fp)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
