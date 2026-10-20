package brand

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zsoftly/zs-core/pkg/identity"
)

// Registry caches the applied brands for token verification. It implements
// identity.BrandResolver. Only active brands with a completed apply are
// known; their verification settings come from the manifest stored at the
// last apply, which is what Keycloak was converged to.
//
// Lookups read an immutable snapshot of the brands and never wait on the
// database while a usable snapshot exists. One refresher at a time loads a
// new snapshot in the background and swaps it in under a short lock. A
// snapshot is due for a refresh at half its TTL, after Invalidate, or when a
// lookup names an unknown realm, and it is served until it is TTL old. Only
// a lookup that finds no usable snapshot waits, for the running refresh or
// its own deadline, whichever comes first, and fails when neither yields a
// snapshot. Token verification reports that as authentication_unavailable.
type Registry struct {
	load       func(ctx context.Context) ([]Record, error)
	log        *slog.Logger
	ttl        time.Duration
	minRefresh time.Duration
	timeout    time.Duration
	now        func() time.Time

	mu          sync.Mutex
	snap        *registrySnapshot // nil until the first load succeeds
	invalidated bool
	refreshing  chan struct{} // closed when the running refresh ends
	lastStart   time.Time
	lastErr     error
}

// registrySnapshot is never modified once published.
type registrySnapshot struct {
	byRealm  map[string]identity.BrandConfig
	loadedAt time.Time
}

// errNoBrands is returned when no snapshot is usable and no refresh
// produced one.
var errNoBrands = errors.New("brand registry: the applied brands could not be loaded")

// NewRegistry returns a registry whose snapshots are served for at most ttl
// and refreshed after ttl/2. An unknown realm triggers a refresh no more than
// once per 5 seconds, so a brand applied by another instance is picked up
// quickly. log receives the brands whose stored manifest cannot be decoded
// and the refreshes that fail; it may be nil.
func NewRegistry(db Querier, ttl time.Duration, log *slog.Logger) *Registry {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Registry{
		load:       func(ctx context.Context) ([]Record, error) { return List(ctx, db, log) },
		log:        log,
		ttl:        ttl,
		minRefresh: 5 * time.Second,
		timeout:    10 * time.Second,
		now:        time.Now,
	}
}

// Invalidate makes the next lookup start a refresh. The current snapshot is
// served until the refresh replaces it or it expires.
func (r *Registry) Invalidate() {
	r.mu.Lock()
	r.invalidated = true
	r.mu.Unlock()
}

// BrandByRealm returns the brand that owns realm.
func (r *Registry) BrandByRealm(ctx context.Context, realm string) (identity.BrandConfig, error) {
	now := r.now()
	r.mu.Lock()
	snap := r.usable(now)
	var (
		cfg identity.BrandConfig
		ok  bool
	)
	if snap != nil {
		cfg, ok = snap.byRealm[realm]
	}
	var wait chan struct{}
	switch {
	case snap == nil || r.invalidated || now.Sub(snap.loadedAt) >= r.ttl/2:
		wait = r.startRefreshLocked(now, false)
	case !ok && now.Sub(snap.loadedAt) >= r.minRefresh:
		wait = r.startRefreshLocked(now, true)
	}
	lastErr := r.lastErr
	r.mu.Unlock()

	if snap != nil {
		if !ok {
			return identity.BrandConfig{}, identity.ErrBrandUnknown
		}
		return cfg, nil
	}
	if wait == nil {
		return identity.BrandConfig{}, noBrandsError(lastErr)
	}
	select {
	case <-wait:
	case <-ctx.Done():
		return identity.BrandConfig{}, fmt.Errorf("%w: %w", errNoBrands, ctx.Err())
	}
	r.mu.Lock()
	snap = r.usable(r.now())
	lastErr = r.lastErr
	r.mu.Unlock()
	if snap == nil {
		return identity.BrandConfig{}, noBrandsError(lastErr)
	}
	if cfg, ok = snap.byRealm[realm]; !ok {
		return identity.BrandConfig{}, identity.ErrBrandUnknown
	}
	return cfg, nil
}

func noBrandsError(cause error) error {
	if cause == nil {
		return errNoBrands
	}
	return fmt.Errorf("%w: %w", errNoBrands, cause)
}

// usable returns the snapshot when it is younger than the TTL. r.mu is held.
func (r *Registry) usable(now time.Time) *registrySnapshot {
	if r.snap == nil || now.Sub(r.snap.loadedAt) >= r.ttl {
		return nil
	}
	return r.snap
}

// startRefreshLocked starts a refresh unless one is running, and returns the
// channel that closes when the running refresh ends. A limited refresh, one
// an unknown realm asks for, does not start within minRefresh of the last
// one and returns nil instead. r.mu is held.
func (r *Registry) startRefreshLocked(now time.Time, limited bool) chan struct{} {
	if r.refreshing != nil {
		return r.refreshing
	}
	if limited && !r.lastStart.IsZero() && now.Sub(r.lastStart) < r.minRefresh {
		return nil
	}
	done := make(chan struct{})
	r.refreshing = done
	r.invalidated = false
	r.lastStart = now
	go r.refresh(now, done)
	return done
}

// refresh loads the brands and publishes a new snapshot. A failed load keeps
// the previous snapshot. The snapshot is dated from the start of the load.
func (r *Registry) refresh(started time.Time, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	records, err := r.load(ctx)
	var next *registrySnapshot
	if err == nil {
		next = &registrySnapshot{byRealm: make(map[string]identity.BrandConfig, len(records)), loadedAt: started}
		for _, rec := range records {
			if rec.Status != StatusActive || rec.AppliedAt.IsZero() || rec.Manifest == nil {
				continue
			}
			next.byRealm[rec.KeycloakRealm] = VerificationConfig(rec)
		}
	} else if r.log != nil {
		r.log.Warn("brand registry refresh failed; serving the previous snapshot until it expires", "error", err)
	}
	r.mu.Lock()
	if next != nil {
		r.snap = next
	}
	r.lastErr = err
	r.refreshing = nil
	r.mu.Unlock()
	close(done)
}

// VerificationConfig derives the token verification settings of a brand.
func VerificationConfig(rec Record) identity.BrandConfig {
	m := rec.Manifest
	cfg := identity.BrandConfig{
		ID:               rec.ID,
		Slug:             rec.Slug,
		Realm:            rec.KeycloakRealm,
		CustomerClientID: m.Identity.CustomerClient.ClientID,
		StaffClientID:    m.Identity.StaffClient.ClientID,
		MFAACRValues:     append([]string(nil), m.Identity.MFAACRValues...),
		StaffMFARequired: m.Identity.MFA == MFARequired || m.Identity.MFA == MFARequiredForStaff,
	}
	for _, sc := range m.Identity.ServiceClients {
		cfg.ServiceClientIDs = append(cfg.ServiceClientIDs, sc.ClientID)
	}
	return cfg
}
