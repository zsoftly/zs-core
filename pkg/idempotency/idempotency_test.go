package idempotency

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

func testKey(t *testing.T) Key {
	t.Helper()
	s, err := scope.New(idgen.MustNewV7(), idgen.MustNewV7())
	if err != nil {
		t.Fatal(err)
	}
	return Key{Scope: s, ActorID: "user:alice", Value: "order-0001"}
}

func mustFingerprint(t *testing.T, op, body string) string {
	t.Helper()
	fp, err := Fingerprint(op, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestValidateKey(t *testing.T) {
	valid := []string{"abcdefgh", strings.Repeat("k", 255), "01JABC:retry-1_x.y"}
	for _, v := range valid {
		if err := ValidateKey(v); err != nil {
			t.Errorf("ValidateKey(%q) = %v", v, err)
		}
	}
	invalid := []string{"", "short", strings.Repeat("k", 256), "has space1", "tab\tkey12", "ünïcode-key"}
	for _, v := range invalid {
		if err := ValidateKey(v); !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyInvalid) {
			t.Errorf("ValidateKey(%q) = %v, want idempotency_key_invalid", v, err)
		}
	}
}

func TestKeyValidateRequiresScopeAndActor(t *testing.T) {
	k := testKey(t)
	k.Scope = scope.Scope{Brand: k.Scope.Brand}
	if err := k.Validate(); !zserrors.IsCode(err, zserrors.CodeScopeRequired) {
		t.Fatalf("missing tenant: %v", err)
	}
	k = testKey(t)
	k.ActorID = ""
	if err := k.Validate(); !zserrors.IsCode(err, zserrors.CodeValidation) {
		t.Fatalf("missing actor: %v", err)
	}
}

func TestFingerprintIsCanonical(t *testing.T) {
	a := mustFingerprint(t, "wallet.topup", `{"amount": 100, "currency":"CAD","idempotency_key":"k-00000001"}`)
	b := mustFingerprint(t, "wallet.topup", `{"idempotency_key":"k-00000001","currency":"CAD","amount":100}`)
	if a != b {
		t.Fatal("field order changed the fingerprint")
	}
	if a == mustFingerprint(t, "wallet.topup", `{"amount": 101, "currency":"CAD","idempotency_key":"k-00000001"}`) {
		t.Fatal("different amount produced the same fingerprint")
	}
	if a == mustFingerprint(t, "wallet.refund", `{"amount": 100, "currency":"CAD","idempotency_key":"k-00000001"}`) {
		t.Fatal("different operation produced the same fingerprint")
	}
	big1 := mustFingerprint(t, "op", `{"n": 9007199254740993}`)
	big2 := mustFingerprint(t, "op", `{"n": 9007199254740992}`)
	if big1 == big2 {
		t.Fatal("large integers lost precision")
	}
	if _, err := Fingerprint("op", []byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := Fingerprint("op", []byte(`{`)); !zserrors.IsCode(err, zserrors.CodeBadRequest) {
		t.Fatalf("invalid JSON error = %v", err)
	}
}

func TestDoRunsOnceAndReplays(t *testing.T) {
	store := NewMemoryStore()
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)
	var calls int32
	fn := func(context.Context) (Result, error) {
		atomic.AddInt32(&calls, 1)
		return Result{Status: 201, Body: []byte(`{"id":"ord_1"}`)}, nil
	}

	first, replayed, err := Do(context.Background(), store, key, fp, fn)
	if err != nil || replayed || first.Status != 201 {
		t.Fatalf("first = %+v %v %v", first, replayed, err)
	}
	second, replayed, err := Do(context.Background(), store, key, fp, fn)
	if err != nil || !replayed || string(second.Body) != string(first.Body) || second.Status != 201 {
		t.Fatalf("second = %+v %v %v", second, replayed, err)
	}
	if calls != 1 {
		t.Fatalf("command ran %d times", calls)
	}
}

func TestDoConflictOnDifferentPayload(t *testing.T) {
	store := NewMemoryStore()
	key := testKey(t)
	ok := func(context.Context) (Result, error) { return Result{Status: 200}, nil }
	if _, _, err := Do(context.Background(), store, key, mustFingerprint(t, "op", `{"a":1}`), ok); err != nil {
		t.Fatal(err)
	}
	_, _, err := Do(context.Background(), store, key, mustFingerprint(t, "op", `{"a":2}`), ok)
	if !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyConflict) {
		t.Fatalf("err = %v, want idempotency_key_conflict", err)
	}
	status, env := zserrors.ToEnvelope(err)
	if status != 409 || env.Error.Code != "idempotency_key_conflict" {
		t.Fatalf("envelope = %d %+v", status, env)
	}
}

func TestDoKeysAreScopedByBrandTenantAndActor(t *testing.T) {
	store := NewMemoryStore()
	base := testKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)
	var calls int32
	fn := func(context.Context) (Result, error) { atomic.AddInt32(&calls, 1); return Result{Status: 200}, nil }

	otherBrand := base
	otherBrand.Scope.Brand = idgen.MustNewV7()
	otherTenant := base
	otherTenant.Scope.Tenant = idgen.MustNewV7()
	otherActor := base
	otherActor.ActorID = "user:bob"
	for _, k := range []Key{base, otherBrand, otherTenant, otherActor} {
		if _, replayed, err := Do(context.Background(), store, k, fp, fn); err != nil || replayed {
			t.Fatalf("Do(%+v) = %v %v", k, replayed, err)
		}
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4 independent executions", calls)
	}
}

func TestDoInProgress(t *testing.T) {
	store := NewMemoryStore()
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	started := make(chan struct{})
	finish := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = Do(context.Background(), store, key, fp, func(context.Context) (Result, error) {
			close(started)
			<-finish
			return Result{Status: 200}, nil
		})
	}()
	<-started
	_, _, err := Do(context.Background(), store, key, fp, func(context.Context) (Result, error) {
		t.Fatal("second execution ran")
		return Result{}, nil
	})
	close(finish)
	wg.Wait()
	if !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyInProgress) {
		t.Fatalf("err = %v, want in progress", err)
	}
	if _, env := zserrors.ToEnvelope(err); !env.Error.Retryable {
		t.Fatal("in progress must be retryable")
	}
}

func TestDoReleasesOnError(t *testing.T) {
	store := NewMemoryStore()
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	boom := errors.New("provider down")
	if _, _, err := Do(context.Background(), store, key, fp, func(context.Context) (Result, error) { return Result{}, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	res, replayed, err := Do(context.Background(), store, key, fp, func(context.Context) (Result, error) { return Result{Status: 200}, nil })
	if err != nil || replayed || res.Status != 200 {
		t.Fatalf("retry after failure = %+v %v %v", res, replayed, err)
	}
}

func TestDoStoresTooLargeMarkerAndDoesNotRunAgain(t *testing.T) {
	store := NewMemoryStore()
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	var calls int32
	fn := func(context.Context) (Result, error) {
		atomic.AddInt32(&calls, 1)
		return Result{Status: 200, Body: make([]byte, MaxResultBytes+1)}, nil
	}
	_, replayed, err := Do(context.Background(), store, key, fp, fn)
	if !zserrors.IsCode(err, zserrors.CodeIdempotencyResultTooLarge) || replayed {
		t.Fatalf("first = %v %v", replayed, err)
	}
	_, replayed, err = Do(context.Background(), store, key, fp, fn)
	if !zserrors.IsCode(err, zserrors.CodeIdempotencyResultTooLarge) || !replayed {
		t.Fatalf("replay = %v %v", replayed, err)
	}
	if calls != 1 {
		t.Fatalf("command ran %d times", calls)
	}
	status, env := zserrors.ToEnvelope(err)
	if status != 500 || env.Error.Code != "idempotency_result_too_large" || env.Error.Retryable {
		t.Fatalf("envelope = %d %+v", status, env)
	}
}

func TestStaleLeaseTakeoverOnlyForSamePayload(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)
	if r, _ := store.Reserve(context.Background(), key, fp); !r.Reserved {
		t.Fatal("first reserve failed")
	}
	now = now.Add(PendingLeaseDuration + time.Second)
	if r, _ := store.Reserve(context.Background(), key, mustFingerprint(t, "op", `{"a":2}`)); r.Reserved {
		t.Fatal("different payload took over a stale lease")
	}
	if r, _ := store.Reserve(context.Background(), key, fp); !r.Reserved {
		t.Fatal("identical retry could not take over a stale lease")
	}
	now = now.Add(RecordRetention + time.Second)
	if r, _ := store.Reserve(context.Background(), key, mustFingerprint(t, "op", `{"a":3}`)); !r.Reserved {
		t.Fatal("expired record still blocks the key")
	}
}

// clock lets a test move the time of a MemoryStore from inside a command.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestDoLeaseTakeoverKeepsTheNewOwnersResult(t *testing.T) {
	store := NewMemoryStore()
	c := &clock{now: time.Now().UTC()}
	store.now = c.Now
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)
	ctx := context.Background()

	_, _, errA := Do(ctx, store, key, fp, func(context.Context) (Result, error) {
		// A outlives its lease; the identical retry B takes the key over,
		// runs and stores its result.
		c.Add(PendingLeaseDuration + time.Second)
		resB, replayed, errB := Do(ctx, store, key, fp, func(context.Context) (Result, error) {
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
	res, replayed, err := Do(ctx, store, key, fp, func(context.Context) (Result, error) {
		t.Fatal("command ran a third time")
		return Result{}, nil
	})
	if err != nil || !replayed || string(res.Body) != `"b"` {
		t.Fatalf("replay = %+v %v %v, want B's result", res, replayed, err)
	}
}

func TestDoReleaseAfterTakeoverKeepsTheNewReservation(t *testing.T) {
	store := NewMemoryStore()
	c := &clock{now: time.Now().UTC()}
	store.now = c.Now
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)
	ctx := context.Background()

	var b Reservation
	boom := errors.New("provider down")
	_, _, errA := Do(ctx, store, key, fp, func(context.Context) (Result, error) {
		c.Add(PendingLeaseDuration + time.Second)
		var err error
		if b, err = store.Reserve(ctx, key, fp); err != nil || !b.Reserved {
			t.Fatalf("B could not take over: %+v %v", b, err)
		}
		return Result{}, boom
	})
	if !errors.Is(errA, boom) {
		t.Fatalf("A = %v", errA)
	}
	if r, _ := store.Reserve(ctx, key, fp); r.Reserved || r.Existing.Status != StatusPending {
		t.Fatalf("A's release removed B's reservation: %+v", r)
	}
	if err := store.Complete(ctx, key, b.OwnerToken, Result{Status: 200, Body: []byte(`{}`)}); err != nil {
		t.Fatalf("B complete = %v", err)
	}
}

func TestDoBoundsTheCommandByTheLease(t *testing.T) {
	store := NewMemoryStore()
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	before := time.Now()
	_, _, err := Do(context.Background(), store, key, fp, func(ctx context.Context) (Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("command context has no deadline")
		}
		if d := deadline.Sub(before); d <= 0 || d > PendingLeaseDuration+time.Second {
			t.Fatalf("command deadline is %v after start, want the lease of %v", d, PendingLeaseDuration)
		}
		return Result{Status: 200}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	short := &shortLeaseStore{MemoryStore: NewMemoryStore(), lease: 20 * time.Millisecond}
	key2 := testKey(t)
	_, _, err = Do(context.Background(), short, key2, fp, func(ctx context.Context) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("command past its lease = %v, want deadline exceeded", err)
	}
	if r, _ := short.MemoryStore.Reserve(context.Background(), key2, fp); !r.Reserved {
		t.Fatal("reservation not released after the command hit its lease")
	}
}

// shortLeaseStore reports a short lease so a test can watch the command
// context end.
type shortLeaseStore struct {
	*MemoryStore
	lease time.Duration
}

func (s *shortLeaseStore) Reserve(ctx context.Context, key Key, fp string) (Reservation, error) {
	r, err := s.MemoryStore.Reserve(ctx, key, fp)
	if r.Reserved {
		r.LeaseExpiresAt = time.Now().Add(s.lease)
	}
	return r, err
}

// failingCompleteStore fails Complete a set number of times.
type failingCompleteStore struct {
	Store
	failures int32
	marked   int32
}

func (s *failingCompleteStore) Complete(ctx context.Context, key Key, owner []byte, result Result) error {
	if atomic.AddInt32(&s.failures, -1) >= 0 {
		return errors.New("connection reset")
	}
	return s.Store.Complete(ctx, key, owner, result)
}

func (s *failingCompleteStore) MarkUnstored(ctx context.Context, key Key, owner []byte, reason string) error {
	atomic.AddInt32(&s.marked, 1)
	return s.Store.MarkUnstored(ctx, key, owner, reason)
}

func TestDoCompleteFailureDoesNotRunTheCommandAgain(t *testing.T) {
	store := &failingCompleteStore{Store: NewMemoryStore(), failures: 1}
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	var calls int32
	fn := func(context.Context) (Result, error) {
		atomic.AddInt32(&calls, 1)
		return Result{Status: 201}, nil
	}
	_, _, err := Do(context.Background(), store, key, fp, fn)
	if !zserrors.IsCode(err, zserrors.CodeDatabaseUnavailable) || store.marked != 1 {
		t.Fatalf("first = %v, marked %d", err, store.marked)
	}
	_, _, err = Do(context.Background(), store, key, fp, fn)
	if !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyInProgress) {
		t.Fatalf("retry = %v, want idempotency_key_in_progress", err)
	}
	if calls != 1 {
		t.Fatalf("command ran %d times", calls)
	}
}

func TestDoBookkeepingSurvivesCancelledCaller(t *testing.T) {
	store := NewMemoryStore()
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	if _, _, err := Do(ctx, store, key, fp, func(context.Context) (Result, error) {
		cancel()
		return Result{Status: 200, Body: []byte(`{}`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := Do(context.Background(), store, key, fp, func(context.Context) (Result, error) {
		t.Fatal("command ran again")
		return Result{}, nil
	}); err != nil || !replayed {
		t.Fatalf("replay = %v %v", replayed, err)
	}
}

func TestDoRejectsInvalidKeyBeforeStore(t *testing.T) {
	key := testKey(t)
	key.Value = "short"
	_, _, err := Do(context.Background(), NewMemoryStore(), key, "fp", func(context.Context) (Result, error) {
		t.Fatal("command ran")
		return Result{}, nil
	})
	if !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyInvalid) {
		t.Fatalf("err = %v", err)
	}
}
