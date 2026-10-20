package keycloak

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

func jwkOf(kid string, k *rsa.PublicKey, use, alg string) map[string]string {
	return map[string]string{
		"kid": kid, "kty": "RSA", "use": use, "alg": alg,
		"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
	}
}

// clock is a test clock safe to read from the refresh goroutine.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeKeycloak serves one realm. It can fail with a status or hang until
// released.
type fakeKeycloak struct {
	*httptest.Server
	key        *rsa.PrivateKey
	status     atomic.Int32 // 0 serves normally
	hang       atomic.Bool
	release    chan struct{}
	discoCalls atomic.Int32
	certCalls  atomic.Int32
	inflight   atomic.Int32
	maxFlight  atomic.Int32
}

const testRealm = "zs-reference"

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &fakeKeycloak{key: key, release: make(chan struct{})}
	gate := func(w http.ResponseWriter, r *http.Request) bool {
		n := f.inflight.Add(1)
		defer f.inflight.Add(-1)
		for {
			m := f.maxFlight.Load()
			if n <= m || f.maxFlight.CompareAndSwap(m, n) {
				break
			}
		}
		if f.hang.Load() {
			select {
			case <-f.release:
			case <-r.Context().Done():
			}
			http.Error(w, "released", http.StatusServiceUnavailable)
			return false
		}
		if s := f.status.Load(); s != 0 {
			http.Error(w, "failing", int(s))
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/"+testRealm+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		f.discoCalls.Add(1)
		if !gate(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "http://127.0.0.1:8180/realms/" + testRealm, "jwks_uri": "http://elsewhere.invalid/certs"})
	})
	mux.HandleFunc("/realms/"+testRealm+"/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		f.certCalls.Add(1)
		if !gate(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwkOf("sig", &key.PublicKey, "sig", "RS256"), jwkOf("enc", &enc.PublicKey, "enc", "RSA-OAEP")}})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(func() {
		if f.hang.Load() {
			close(f.release)
		}
		f.Close()
	})
	return f
}

func TestKeySet(t *testing.T) {
	kc := newFakeKeycloak(t)
	clk := &clock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	ks := NewKeySet(kc.URL, KeySetOptions{Now: clk.Now})
	ctx := context.Background()

	iss, err := ks.Issuer(ctx, testRealm)
	if err != nil || iss != "http://127.0.0.1:8180/realms/"+testRealm {
		t.Fatalf("issuer = %q, %v", iss, err)
	}
	if k, err := ks.PublicKey(ctx, testRealm, "sig"); err != nil || k.N.Cmp(kc.key.N) != 0 {
		t.Fatalf("sig key: %v", err)
	}
	if _, err := ks.PublicKey(ctx, testRealm, "enc"); !errors.Is(err, identity.ErrUnknownKey) {
		t.Fatalf("encryption key must not be usable: %v", err)
	}
	calls := kc.certCalls.Load()
	if _, err := ks.PublicKey(ctx, testRealm, "made-up"); !errors.Is(err, identity.ErrUnknownKey) {
		t.Fatalf("unknown kid: %v", err)
	}
	if kc.certCalls.Load() != calls {
		t.Fatal("an unknown kid within the minimum refresh interval refetched keys")
	}

	// Keycloak goes down after the TTL: known keys keep working until
	// maxStale, unknown ones are unavailable, not invalid.
	kc.status.Store(http.StatusServiceUnavailable)
	clk.Add(DefaultKeyTTL + time.Second)
	if _, err := ks.PublicKey(ctx, testRealm, "sig"); err != nil {
		t.Fatalf("stale key within maxStale: %v", err)
	}
	if _, err := ks.PublicKey(ctx, testRealm, "new-kid"); !errors.Is(err, identity.ErrKeysUnavailable) {
		t.Fatalf("unknown kid during outage: %v", err)
	}
	clk.Add(DefaultMaxStale)
	if _, err := ks.PublicKey(ctx, testRealm, "sig"); !errors.Is(err, identity.ErrKeysUnavailable) {
		t.Fatalf("beyond maxStale: %v", err)
	}

	unreachable := NewKeySet("http://127.0.0.1:1", KeySetOptions{})
	if _, err := unreachable.Issuer(ctx, testRealm); !errors.Is(err, identity.ErrKeysUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}

// A 404 or any other non-200 answer is a failed fetch, never an unknown key:
// cached keys keep working within maxStale, and without them the answer is
// unavailable.
func TestNon200IsAFetchFailure(t *testing.T) {
	kc := newFakeKeycloak(t)
	ctx := context.Background()
	if _, err := NewKeySet(kc.URL, KeySetOptions{}).PublicKey(ctx, "zs-missing", "sig"); !errors.Is(err, identity.ErrKeysUnavailable) || errors.Is(err, identity.ErrUnknownKey) {
		t.Fatalf("missing realm: %v", err)
	}
	for _, status := range []int32{http.StatusNotFound, http.StatusInternalServerError, http.StatusForbidden} {
		kc.status.Store(status)
		_, err := NewKeySet(kc.URL, KeySetOptions{}).PublicKey(ctx, testRealm, "sig")
		if !errors.Is(err, identity.ErrKeysUnavailable) || errors.Is(err, identity.ErrUnknownKey) {
			t.Fatalf("HTTP %d without cached keys: %v", status, err)
		}
	}

	kc.status.Store(0)
	clk := &clock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	ks := NewKeySet(kc.URL, KeySetOptions{Now: clk.Now})
	if _, err := ks.PublicKey(ctx, testRealm, "sig"); err != nil {
		t.Fatal(err)
	}
	kc.status.Store(http.StatusNotFound)
	clk.Add(DefaultKeyTTL + time.Second)
	if _, err := ks.PublicKey(ctx, testRealm, "sig"); err != nil {
		t.Fatalf("cached key during a 404: %v", err)
	}
	waitIdle(t, ks, testRealm)
	clk.Add(DefaultMinRefresh)
	if _, err := ks.PublicKey(ctx, testRealm, "other"); !errors.Is(err, identity.ErrKeysUnavailable) {
		t.Fatalf("unknown kid during a 404: %v", err)
	}
}

// waitIdle waits until no refresh of realm is running.
func waitIdle(t *testing.T, ks *KeySet, realm string) {
	t.Helper()
	rk := ks.realm(realm)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rk.mu.Lock()
		busy := rk.inflight != nil
		rk.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("refresh did not finish")
}

// Refresh attempts are throttled in the stale case too: a failing Keycloak
// sees at most one attempt per minRefresh however many requests arrive.
func TestStaleRefreshIsThrottled(t *testing.T) {
	kc := newFakeKeycloak(t)
	clk := &clock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	ks := NewKeySet(kc.URL, KeySetOptions{Now: clk.Now})
	ctx := context.Background()
	if _, err := ks.PublicKey(ctx, testRealm, "sig"); err != nil {
		t.Fatal(err)
	}
	kc.status.Store(http.StatusBadGateway)
	clk.Add(DefaultKeyTTL)
	before := kc.discoCalls.Load()
	for range 50 {
		if _, err := ks.PublicKey(ctx, testRealm, "sig"); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, ks, testRealm)
		clk.Add(time.Second)
	}
	// 50 requests over 50 seconds with a 30-second minimum interval: two
	// attempts.
	if got := kc.discoCalls.Load() - before; got != 2 {
		t.Fatalf("%d refresh attempts in 50 seconds, want 2", got)
	}
}

func signToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]any{"alg": "RS256", "kid": "sig", "typ": "JWT"}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type oneBrand struct{ cfg identity.BrandConfig }

func (b oneBrand) BrandByRealm(_ context.Context, realm string) (identity.BrandConfig, error) {
	if realm != b.cfg.Realm {
		return identity.BrandConfig{}, identity.ErrBrandUnknown
	}
	return b.cfg, nil
}

// A Keycloak that accepts connections and never answers does not hold up
// verification with cached keys, and a request that needs a refresh returns
// when its own context ends.
func TestHungKeycloakDoesNotBlockCachedVerification(t *testing.T) {
	kc := newFakeKeycloak(t)
	start := time.Now()
	clk := &clock{now: start}
	ks := NewKeySet(kc.URL, KeySetOptions{Now: clk.Now, FetchTimeout: 5 * time.Second})
	v := &identity.Verifier{
		Brands: oneBrand{identity.BrandConfig{ID: idgen.MustNewV7(), Slug: testRealm, Realm: testRealm, CustomerClientID: "zs-customer", StaffClientID: "zs-staff"}},
		Keys:   ks, Now: clk.Now,
	}
	tok := func() string {
		now := clk.Now()
		return signToken(t, kc.key, map[string]any{
			"iss": "http://127.0.0.1:8180/realms/" + testRealm, "sub": "s1", "aud": "zs-customer", "azp": "zs-customer",
			"typ": "Bearer", "exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(),
		})
	}
	ctx := context.Background()
	if _, err := v.Verify(ctx, tok()); err != nil {
		t.Fatalf("warm-up verify: %v", err)
	}

	kc.hang.Store(true)
	clk.Add(DefaultKeyTTL + time.Second) // stale: a refresh is due and hangs
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	began := time.Now()
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := v.Verify(ctx, tok())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	if elapsed := time.Since(began); elapsed > 100*time.Millisecond {
		t.Fatalf("20 verifications with cached keys took %v behind a hung Keycloak", elapsed)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("verify with cached keys: %v", err)
		}
	}
	if n := kc.maxFlight.Load(); n > 1 {
		t.Fatalf("%d refreshes ran at once", n)
	}

	// An unknown key needs the refresh; the caller's deadline bounds the
	// wait and the answer is unavailable, not invalid.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	began = time.Now()
	_, err := ks.PublicKey(short, testRealm, "rotated")
	if elapsed := time.Since(began); elapsed > 500*time.Millisecond {
		t.Fatalf("cancelled caller waited %v", elapsed)
	}
	if !errors.Is(err, identity.ErrKeysUnavailable) {
		t.Fatalf("unknown kid behind a hung Keycloak: %v", err)
	}
	if _, err := v.Verify(ctx, tok()); err != nil {
		t.Fatalf("cached verification after a cancelled caller: %v", err)
	}
	// The verifier maps the same failure to 503, never 401.
	unknown := signToken(t, kc.key, map[string]any{"iss": "http://127.0.0.1:8180/realms/" + testRealm})
	unknown = strings.Replace(unknown, base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"sig","typ":"JWT"}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"rotated","typ":"JWT"}`)), 1)
	short2, cancel2 := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel2()
	if _, err := v.Verify(short2, unknown); !zserrors.IsCode(err, zserrors.CodeAuthenticationUnavailable) {
		t.Fatalf("verify with an unknown key behind a hung Keycloak: %v", err)
	}
}
