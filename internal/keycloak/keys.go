package keycloak

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zsoftly/zs-core/internal/upstream"
	"github.com/zsoftly/zs-core/pkg/identity"
)

// Key cache timings.
const (
	// DefaultKeyTTL is how long discovery and keys are used before a
	// refresh.
	DefaultKeyTTL = 10 * time.Minute
	// DefaultMaxStale is how long known keys keep working while Keycloak
	// cannot be reached for a refresh.
	DefaultMaxStale = time.Hour
	// DefaultMinRefresh limits refreshes triggered by an unknown key ID, so
	// tokens with made-up key IDs cannot make Core hammer Keycloak.
	DefaultMinRefresh = 30 * time.Second
)

// KeySet caches the OIDC discovery issuer and the RS256 signing keys of each
// realm. It implements identity.KeySource. Keys are fetched from the
// configured base URL, not from the jwks_uri in discovery, so the hostname
// Keycloak advertises to browsers never decides where Core fetches keys.
//
// No request waits on network I/O while it holds a lock. One refresh per
// realm runs at a time, in its own goroutine with its own timeout. While the
// cached keys are within maxStale they are served at once, and a refresh, if
// one is due, runs in the background. A request that needs the refresh,
// because it has no keys or names an unknown key ID, waits for it or for its
// own context, whichever ends first. Refreshes start at most once per
// minRefresh, whatever triggers them.
type KeySet struct {
	base         string
	http         *http.Client
	ttl          time.Duration
	maxStale     time.Duration
	minRefresh   time.Duration
	fetchTimeout time.Duration
	now          func() time.Time

	mu     sync.Mutex
	realms map[string]*realmKeys
}

type realmKeys struct {
	mu          sync.Mutex
	issuer      string
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
	lastErr     error
	inflight    chan struct{} // closed when the running refresh ends
}

// KeySetOptions tune a KeySet. Zero values take the defaults.
type KeySetOptions struct {
	HTTPClient *http.Client
	TTL        time.Duration
	MaxStale   time.Duration
	MinRefresh time.Duration
	// FetchTimeout bounds one refresh. Defaults to 10 seconds.
	FetchTimeout time.Duration
	Now          func() time.Time
}

// NewKeySet returns a cache for the Keycloak server at baseURL.
func NewKeySet(baseURL string, opts KeySetOptions) *KeySet {
	ks := &KeySet{
		base:         strings.TrimRight(baseURL, "/"),
		http:         opts.HTTPClient,
		ttl:          opts.TTL,
		maxStale:     opts.MaxStale,
		minRefresh:   opts.MinRefresh,
		fetchTimeout: opts.FetchTimeout,
		now:          opts.Now,
		realms:       map[string]*realmKeys{},
	}
	if ks.http == nil {
		ks.http = upstream.NewClient()
		ks.http.Timeout = 5 * time.Second
	}
	if ks.ttl <= 0 {
		ks.ttl = DefaultKeyTTL
	}
	if ks.maxStale <= 0 {
		ks.maxStale = DefaultMaxStale
	}
	if ks.minRefresh <= 0 {
		ks.minRefresh = DefaultMinRefresh
	}
	if ks.fetchTimeout <= 0 {
		ks.fetchTimeout = 10 * time.Second
	}
	if ks.now == nil {
		ks.now = time.Now
	}
	return ks
}

func (ks *KeySet) realm(name string) *realmKeys {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	rk, ok := ks.realms[name]
	if !ok {
		rk = &realmKeys{}
		ks.realms[name] = rk
	}
	return rk
}

// Issuer returns the issuer of the realm.
func (ks *KeySet) Issuer(ctx context.Context, realm string) (string, error) {
	issuer, _, err := ks.lookup(ctx, realm, "")
	return issuer, err
}

// PublicKey returns the signing key kid of the realm.
func (ks *KeySet) PublicKey(ctx context.Context, realm, kid string) (*rsa.PublicKey, error) {
	_, key, err := ks.lookup(ctx, realm, kid)
	return key, err
}

// lookup returns the issuer and, when kid is set, the key. It never holds
// the realm lock across network I/O.
func (ks *KeySet) lookup(ctx context.Context, realm, kid string) (string, *rsa.PublicKey, error) {
	rk := ks.realm(realm)
	waited := false
	for {
		rk.mu.Lock()
		now := ks.now()
		have := !rk.fetchedAt.IsZero()
		age := now.Sub(rk.fetchedAt)
		key, known := rk.keys[kid]
		missing := kid != "" && !known
		usable := have && age < ks.maxStale && !missing
		due := now.Sub(rk.lastAttempt) >= ks.minRefresh

		// Cached data answers the request: fresh, or stale but within
		// maxStale. A stale entry refreshes in the background.
		if usable {
			if age >= ks.ttl && due && rk.inflight == nil {
				ks.startRefresh(realm, rk, now)
			}
			issuer := rk.issuer
			rk.mu.Unlock()
			return issuer, key, nil
		}
		// The request needs a refresh: no keys, keys beyond maxStale, or an
		// unknown key ID. Join the running refresh, or start one when due.
		if rk.inflight == nil && due && !waited {
			ks.startRefresh(realm, rk, now)
		}
		if done := rk.inflight; done != nil && !waited {
			rk.mu.Unlock()
			select {
			case <-done:
				waited = true
				continue
			case <-ctx.Done():
				return "", nil, fmt.Errorf("%w: %v", identity.ErrKeysUnavailable, ctx.Err())
			}
		}
		// No refresh is possible now. Answer from what the last one left.
		lastErr := rk.lastErr
		freshEnough := have && age < ks.maxStale
		rk.mu.Unlock()
		switch {
		case lastErr == nil && freshEnough && missing:
			// Keycloak answered and has no such key.
			return "", nil, identity.ErrUnknownKey
		case lastErr != nil:
			// A key Core has never seen cannot be judged while Keycloak is
			// unreachable: that is unavailable, not invalid.
			return "", nil, lastErr
		default:
			return "", nil, fmt.Errorf("%w: keys expired and a refresh is not due", identity.ErrKeysUnavailable)
		}
	}
}

// startRefresh runs one fetch in the background. rk.mu must be held. The
// fetch has its own timeout and is not tied to any request, so a request
// that gives up never cancels the refresh other requests wait for.
func (ks *KeySet) startRefresh(realm string, rk *realmKeys, now time.Time) {
	done := make(chan struct{})
	rk.inflight = done
	rk.lastAttempt = now
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ks.fetchTimeout)
		issuer, keys, err := ks.fetch(ctx, realm)
		cancel()
		rk.mu.Lock()
		if err == nil {
			rk.issuer, rk.keys, rk.fetchedAt, rk.lastErr = issuer, keys, ks.now(), nil
		} else {
			rk.lastErr = err
		}
		rk.inflight = nil
		rk.mu.Unlock()
		close(done)
	}()
}

func (ks *KeySet) fetch(ctx context.Context, realm string) (string, map[string]*rsa.PublicKey, error) {
	base := ks.base + "/realms/" + url.PathEscape(realm)
	var disco struct {
		Issuer string `json:"issuer"`
	}
	if err := ks.getJSON(ctx, base+"/.well-known/openid-configuration", &disco); err != nil {
		return "", nil, err
	}
	if disco.Issuer == "" {
		return "", nil, fmt.Errorf("%w: discovery has no issuer", identity.ErrKeysUnavailable)
	}
	var jwks struct {
		Keys []jwk `json:"keys"`
	}
	if err := ks.getJSON(ctx, base+"/protocol/openid-connect/certs", &jwks); err != nil {
		return "", nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		pub, err := k.rsa()
		if err != nil || pub.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = pub
	}
	return disco.Issuer, keys, nil
}

func (ks *KeySet) getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := upstream.Do(ks.http, req)
	if err != nil {
		return fmt.Errorf("%w: %v", identity.ErrKeysUnavailable, err)
	}
	// Any status other than 200, including 404 for a realm that is missing,
	// is a failed fetch: known keys keep working within maxStale and
	// anything else is authentication_unavailable.
	if resp.Status != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", identity.ErrKeysUnavailable, resp.Status)
	}
	if err := upstream.DecodeJSON(resp.Body, out); err != nil {
		return fmt.Errorf("%w: %v", identity.ErrKeysUnavailable, err)
	}
	return nil
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k jwk) rsa() (*rsa.PublicKey, error) {
	n, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil || len(n) == 0 {
		return nil, errors.New("invalid modulus")
	}
	e, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil || len(e) == 0 || len(e) > 4 {
		return nil, errors.New("invalid exponent")
	}
	exp := 0
	for _, b := range e {
		exp = exp<<8 | int(b)
	}
	if exp < 3 || exp%2 == 0 {
		return nil, errors.New("invalid exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, nil
}
