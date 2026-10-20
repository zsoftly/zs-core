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

func TestDoRejectsOversizedResult(t *testing.T) {
	store := NewMemoryStore()
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{}`)
	_, _, err := Do(context.Background(), store, key, fp, func(context.Context) (Result, error) {
		return Result{Status: 200, Body: make([]byte, MaxResultBytes+1)}, nil
	})
	if !zserrors.IsCode(err, zserrors.CodeInternal) {
		t.Fatalf("err = %v", err)
	}
	if _, reserved, _ := store.Reserve(context.Background(), key, fp); !reserved {
		t.Fatal("oversized result left the key reserved")
	}
}

func TestStaleLeaseTakeoverOnlyForSamePayload(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	store.now = func() time.Time { return now }
	key := testKey(t)
	fp := mustFingerprint(t, "op", `{"a":1}`)
	if _, reserved, _ := store.Reserve(context.Background(), key, fp); !reserved {
		t.Fatal("first reserve failed")
	}
	now = now.Add(PendingLeaseDuration + time.Second)
	if _, reserved, _ := store.Reserve(context.Background(), key, mustFingerprint(t, "op", `{"a":2}`)); reserved {
		t.Fatal("different payload took over a stale lease")
	}
	if _, reserved, _ := store.Reserve(context.Background(), key, fp); !reserved {
		t.Fatal("identical retry could not take over a stale lease")
	}
	now = now.Add(RecordRetention + time.Second)
	if _, reserved, _ := store.Reserve(context.Background(), key, mustFingerprint(t, "op", `{"a":3}`)); !reserved {
		t.Fatal("expired record still blocks the key")
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
