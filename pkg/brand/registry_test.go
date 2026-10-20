package brand

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// fakeStore serves the brands to a registry. While hung, a load blocks until
// release is closed, whatever its context says, as a database that stopped
// answering would.
type fakeStore struct {
	mu      sync.Mutex
	records []Record
	hung    chan struct{}
	loads   atomic.Int32
}

func (f *fakeStore) load(ctx context.Context) ([]Record, error) {
	f.loads.Add(1)
	f.mu.Lock()
	hung, records := f.hung, f.records
	f.mu.Unlock()
	if hung != nil {
		<-hung
		return nil, errors.New("database stopped answering")
	}
	return records, nil
}

func (f *fakeStore) hang() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hung = make(chan struct{})
	return f.hung
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func appliedRecord(t *testing.T, realm string) Record {
	m := reference(t)
	m.Identity.Realm = realm
	return Record{ID: idgen.MustNewV7(), Slug: realm, Status: StatusActive, KeycloakRealm: realm, Manifest: m, AppliedAt: time.Now()}
}

func newTestRegistry(store *fakeStore, clock *fakeClock) *Registry {
	r := NewRegistry(nil, 30*time.Second, nil)
	r.load = store.load
	r.now = clock.Now
	return r
}

// eventually waits for cond, for background refreshes.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

// With no snapshot, a lookup waits for the single running refresh up to its
// own deadline and then fails; a hung store never stacks up loads.
func TestRegistryWithoutSnapshotFailsAtTheDeadline(t *testing.T) {
	store := &fakeStore{}
	release := store.hang()
	defer close(release)
	reg := newTestRegistry(store, &fakeClock{now: time.Unix(1_000_000, 0)})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := reg.BrandByRealm(ctx, "zs-reference")
			if err == nil || errors.Is(err, identity.ErrBrandUnknown) || !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("lookup without a snapshot = %v", err)
			}
			if time.Since(start) > 2*time.Second {
				t.Errorf("lookup outlived its deadline: %v", time.Since(start))
			}
		}()
	}
	wg.Wait()
	if n := store.loads.Load(); n != 1 {
		t.Fatalf("loads = %d, want 1", n)
	}
}

// With a snapshot, lookups never wait on the store: a hung refresh leaves
// them on the stale snapshot until it is TTL old, and only then do they fail.
func TestRegistryServesTheSnapshotWhileTheStoreHangs(t *testing.T) {
	store := &fakeStore{records: []Record{appliedRecord(t, "zs-reference")}}
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	reg := newTestRegistry(store, clock)
	ctx := context.Background()

	if cfg, err := reg.BrandByRealm(ctx, "zs-reference"); err != nil || cfg.Realm != "zs-reference" {
		t.Fatalf("first lookup = %+v, %v", cfg, err)
	}
	release := store.hang()
	defer close(release)

	// Half the TTL: due for a refresh, which hangs. Every lookup returns at
	// once from the snapshot and only one load runs.
	clock.Advance(16 * time.Second)
	for range 50 {
		lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
		start := time.Now()
		cfg, err := reg.BrandByRealm(lookupCtx, "zs-reference")
		cancel()
		if err != nil || cfg.Realm != "zs-reference" {
			t.Fatalf("stale lookup = %+v, %v", cfg, err)
		}
		if time.Since(start) > 100*time.Millisecond {
			t.Fatalf("lookup waited %v on a hung store", time.Since(start))
		}
	}
	// An unknown realm is answered from the snapshot too.
	if _, err := reg.BrandByRealm(ctx, "zs-other"); !errors.Is(err, identity.ErrBrandUnknown) {
		t.Fatalf("unknown realm = %v", err)
	}
	reg.Invalidate()
	if _, err := reg.BrandByRealm(ctx, "zs-reference"); err != nil {
		t.Fatalf("lookup after Invalidate = %v", err)
	}
	eventually(t, func() bool { return store.loads.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	if n := store.loads.Load(); n != 2 {
		t.Fatalf("loads = %d, want 2", n)
	}

	// Past the TTL there is no usable snapshot: the lookup waits for the hung
	// refresh up to its deadline and fails.
	clock.Advance(15 * time.Second)
	lookupCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := reg.BrandByRealm(lookupCtx, "zs-reference"); err == nil || errors.Is(err, identity.ErrBrandUnknown) {
		t.Fatalf("expired snapshot = %v", err)
	}
}

// A refresh that completes replaces the snapshot, and a failed one keeps it.
func TestRegistryRefreshReplacesTheSnapshot(t *testing.T) {
	store := &fakeStore{records: []Record{appliedRecord(t, "zs-reference")}}
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	reg := newTestRegistry(store, clock)
	ctx := context.Background()
	if _, err := reg.BrandByRealm(ctx, "zs-second"); !errors.Is(err, identity.ErrBrandUnknown) {
		t.Fatalf("zs-second before it exists = %v", err)
	}

	// A brand applied elsewhere: the unknown realm starts a refresh once the
	// snapshot is 5 seconds old, and a later lookup sees the brand.
	store.mu.Lock()
	store.records = append(store.records, appliedRecord(t, "zs-second"))
	store.mu.Unlock()
	if _, err := reg.BrandByRealm(ctx, "zs-second"); !errors.Is(err, identity.ErrBrandUnknown) {
		t.Fatalf("zs-second inside the minimum refresh interval = %v", err)
	}
	clock.Advance(6 * time.Second)
	_, _ = reg.BrandByRealm(ctx, "zs-second")
	eventually(t, func() bool {
		_, err := reg.BrandByRealm(ctx, "zs-second")
		return err == nil
	})

	// A disabled brand disappears after a refresh.
	store.mu.Lock()
	store.records[1].Status = StatusDisabled
	store.mu.Unlock()
	clock.Advance(16 * time.Second)
	_, _ = reg.BrandByRealm(ctx, "zs-second")
	eventually(t, func() bool {
		_, err := reg.BrandByRealm(ctx, "zs-second")
		return errors.Is(err, identity.ErrBrandUnknown)
	})
}

// A TTL shorter than the minimum refresh interval still refreshes on time:
// only refreshes for unknown realms are rate-limited.
func TestRegistryShortTTLRefreshesOnTime(t *testing.T) {
	store := &fakeStore{records: []Record{appliedRecord(t, "zs-reference")}}
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	reg := NewRegistry(nil, time.Second, nil)
	reg.load = store.load
	reg.now = clock.Now
	ctx := context.Background()
	for i := range 10 {
		clock.Advance(1100 * time.Millisecond)
		lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := reg.BrandByRealm(lookupCtx, "zs-reference")
		cancel()
		if err != nil {
			t.Fatalf("lookup %d after the snapshot expired = %v", i, err)
		}
	}
}

func TestVerificationConfigCarriesTheStaffMFARule(t *testing.T) {
	for policy, want := range map[string]bool{MFAOptional: false, MFARequired: true, MFARequiredForStaff: true} {
		m := &Manifest{}
		m.Identity.MFA = policy
		if got := VerificationConfig(Record{Manifest: m}).StaffMFARequired; got != want {
			t.Errorf("%s: StaffMFARequired = %v, want %v", policy, got, want)
		}
	}
}
