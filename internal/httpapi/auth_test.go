package httpapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

var (
	authKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	authBrand  = identity.BrandConfig{ID: idgen.MustNewV7(), Slug: "zs-reference", Realm: "zs-reference",
		CustomerClientID: "zs-customer", StaffClientID: "zs-staff"}
	authIssuer = "http://127.0.0.1:8180/realms/zs-reference"
)

type stubBrands struct{}

func (stubBrands) BrandByRealm(_ context.Context, realm string) (identity.BrandConfig, error) {
	if realm == authBrand.Realm {
		return authBrand, nil
	}
	return identity.BrandConfig{}, identity.ErrBrandUnknown
}

type stubKeys struct{ down bool }

func (k stubKeys) Issuer(context.Context, string) (string, error) {
	if k.down {
		return "", identity.ErrKeysUnavailable
	}
	return authIssuer, nil
}

func (k stubKeys) PublicKey(_ context.Context, _, kid string) (*rsa.PublicKey, error) {
	if k.down {
		return nil, identity.ErrKeysUnavailable
	}
	if kid != "k1" {
		return nil, identity.ErrUnknownKey
	}
	return &authKey.PublicKey, nil
}

// grantDB answers the staff grant lookup and fails everything else.
type grantDB struct {
	failingDB
	granted bool
	err     error
}

func (g grantDB) QueryRow(context.Context, string, ...any) pgx.Row { return boolRow{g.granted, g.err} }

type boolRow struct {
	v   bool
	err error
}

func (r boolRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*bool)) = r.v
	return nil
}

func signToken(t *testing.T, azp string, mutate func(map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"iss": authIssuer, "sub": "user-1", "aud": azp, "azp": azp, "typ": "Bearer",
		"exp": time.Now().Add(5 * time.Minute).Unix(), "email": "p@example.test", "email_verified": false,
	}
	if mutate != nil {
		mutate(claims)
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	input := enc(map[string]any{"alg": "RS256", "kid": "k1"}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, authKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func authServer(db DB, keysDown bool) http.Handler {
	return New(Options{DB: db, Verifier: &identity.Verifier{Brands: stubBrands{}, Keys: stubKeys{down: keysDown}}})
}

func call(t *testing.T, h http.Handler, method, path, token string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := do(t, h, req)
	if rec.Code >= 400 {
		code, _ := decodeEnvelope(t, rec)
		return rec, code
	}
	return rec, ""
}

func errName(err error) string {
	var e *zserrors.Error
	if !errors.As(err, &e) {
		return "untyped"
	}
	return zserrors.Lookup(e.Code).Name
}

func TestV1RequiresBearerToken(t *testing.T) {
	h := authServer(grantDB{}, false)
	for _, header := range []string{"", "Basic dXNlcjpwYXNz", "Bearer", "Bearer a b", "bearer"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := do(t, h, req)
		if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusUnauthorized || code != "unauthenticated" {
			t.Errorf("%q: %d %s", header, rec.Code, rec.Body)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%q: no WWW-Authenticate", header)
		}
	}
	// The Phase 1 headers grant nothing.
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil)
	req.Header.Set("X-ZS-Brand", idgen.Format(idgen.Brand, authBrand.ID))
	req.Header.Set("X-ZS-Tenant", idgen.Format(idgen.Tenant, idgen.MustNewV7()))
	if rec := do(t, h, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("trusted headers: %d", rec.Code)
	}
}

func TestTokenErrors(t *testing.T) {
	h := authServer(grantDB{}, false)
	if _, code := call(t, h, http.MethodGet, "/v1/me", "not.a.token"); code != "token_invalid" {
		t.Errorf("garbage token: %s", code)
	}
	foreign := signToken(t, "zs-customer", func(c map[string]any) { c["iss"] = "http://127.0.0.1:8180/realms/other" })
	if _, code := call(t, h, http.MethodGet, "/v1/me", foreign); code != "token_audience_unknown" {
		t.Errorf("other realm: %s", code)
	}
	if rec, code := call(t, authServer(grantDB{}, true), http.MethodGet, "/v1/me", signToken(t, "zs-customer", nil)); rec.Code != http.StatusServiceUnavailable || code != "authentication_unavailable" {
		t.Errorf("JWKS down: %d %s", rec.Code, code)
	}
	noVerifier := New(Options{})
	if rec, code := call(t, noVerifier, http.MethodGet, "/v1/me", signToken(t, "zs-customer", nil)); rec.Code != http.StatusServiceUnavailable || code != "authentication_unavailable" {
		t.Errorf("no verifier: %d %s", rec.Code, code)
	}
	// A verified email needs the identity row; a database outage is 503.
	verified := signToken(t, "zs-customer", func(c map[string]any) { c["email_verified"] = true })
	down := authServer(grantDB{err: errors.New("connection refused")}, false)
	if rec, code := call(t, down, http.MethodGet, "/v1/me", verified); rec.Code != http.StatusServiceUnavailable || code != "authentication_unavailable" {
		t.Errorf("database down: %d %s", rec.Code, code)
	}
}

func TestMeWithoutVerifiedEmail(t *testing.T) {
	rec, _ := call(t, authServer(grantDB{}, false), http.MethodGet, "/v1/me", signToken(t, "zs-customer", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Identity    map[string]any `json:"identity"`
		Memberships []any          `json:"memberships"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Identity["id"] != nil || body.Identity["kind"] != "user" || body.Identity["subject"] != "user-1" ||
		body.Identity["brand_id"] != idgen.Format(idgen.Brand, authBrand.ID) || body.Memberships == nil || len(body.Memberships) != 0 {
		t.Fatalf("body = %s", rec.Body)
	}
}

func TestStaffRoutes(t *testing.T) {
	brandPath := "/v1/brands/" + idgen.Format(idgen.Brand, authBrand.ID)
	staff := signToken(t, "zs-staff", nil)
	customer := signToken(t, "zs-customer", nil)

	// A customer token never reaches a staff route, grant or not.
	if rec, code := call(t, authServer(grantDB{granted: true}, false), http.MethodGet, brandPath, customer); rec.Code != http.StatusNotFound || code != "not_found" {
		t.Errorf("customer: %d %s", rec.Code, code)
	}
	// A staff token without a grant: 404.
	for _, path := range []string{brandPath, "/v1/audit/events"} {
		if rec, code := call(t, authServer(grantDB{granted: false}, false), http.MethodGet, path, staff); rec.Code != http.StatusNotFound || code != "not_found" {
			t.Errorf("%s without grant: %d %s", path, rec.Code, code)
		}
	}
	// Grant lookup failing: 503, never a pass.
	if rec, code := call(t, authServer(grantDB{err: errors.New("db down")}, false), http.MethodGet, brandPath, staff); rec.Code != http.StatusServiceUnavailable || code != "authentication_unavailable" {
		t.Errorf("grant lookup down: %d %s", rec.Code, code)
	}
	// With a grant, another brand's ID is still 404.
	other := "/v1/brands/" + idgen.Format(idgen.Brand, idgen.MustNewV7())
	if rec, code := call(t, authServer(grantDB{granted: true}, false), http.MethodGet, other, staff); rec.Code != http.StatusNotFound || code != "not_found" {
		t.Errorf("other brand: %d %s", rec.Code, code)
	}
	if rec, code := call(t, authServer(grantDB{granted: true}, false), http.MethodGet, "/v1/brands/brd_nope", staff); rec.Code != http.StatusNotFound || code != "not_found" {
		t.Errorf("malformed brand id: %d %s", rec.Code, code)
	}
}

func TestApplyBrandRequiresManifestMediaType(t *testing.T) {
	h := authServer(grantDB{granted: true}, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/brands/"+idgen.Format(idgen.Brand, authBrand.ID)+"/apply", strings.NewReader("slug: x"))
	req.Header.Set("Authorization", "Bearer "+signToken(t, "zs-staff", nil))
	req.Header.Set("Content-Type", "text/plain")
	rec := do(t, h, req)
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusUnsupportedMediaType || code != "unsupported_media_type" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	req.Header.Set("Content-Type", "application/yaml")
	req.Body = http.NoBody
	rec = do(t, h, req)
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusBadRequest || code != "brand_manifest_invalid" {
		t.Fatalf("empty manifest: %d %s", rec.Code, rec.Body)
	}
}

// identityDB returns one stored identity and refuses transactions, so a
// request that writes fails.
type identityDB struct {
	failingDB
	ident identity.Identity
}

func (d identityDB) QueryRow(context.Context, string, ...any) pgx.Row { return identityRow{d.ident} }

type identityRow struct{ i identity.Identity }

func (r identityRow) Scan(dest ...any) error {
	kc, email, name := r.i.KeycloakUserID, r.i.Email, r.i.DisplayName
	*(dest[0].(*idgen.UUID)) = r.i.ID
	*(dest[1].(*idgen.UUID)) = r.i.BrandID
	*(dest[2].(*string)) = r.i.Issuer
	*(dest[3].(*string)) = r.i.Subject
	*(dest[4].(**string)) = &kc
	*(dest[5].(**string)) = &email
	*(dest[6].(*bool)) = r.i.EmailVerified
	*(dest[7].(**string)) = &name
	*(dest[8].(*time.Time)) = r.i.LastSeenAt
	*(dest[9].(*time.Time)) = r.i.CreatedAt
	return nil
}

// A known identity that has not changed and was seen recently is read, not
// written: the request needs no transaction.
func TestRecordIdentityReadsFirst(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	p := identity.Principal{Brand: identity.BrandConfig{ID: idgen.MustNewV7()}, Kind: identity.KindUser, Issuer: authIssuer,
		Subject: "user-1", Email: "p@example.test", EmailVerified: true, Name: "P"}
	stored := identity.Identity{ID: idgen.MustNewV7(), BrandID: p.Brand.ID, Issuer: p.Issuer, Subject: p.Subject,
		Email: p.Email, EmailVerified: true, DisplayName: "P", LastSeenAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour)}
	s := &server{db: identityDB{ident: stored}, now: func() time.Time { return now }}
	got, err := s.recordIdentity(context.Background(), p)
	if err != nil || got.ID != stored.ID {
		t.Fatalf("unchanged identity: %+v, %v", got, err)
	}
	// A changed email needs the write; the refused transaction shows it was
	// attempted.
	changed := p
	changed.Email = "new@example.test"
	if _, err := s.recordIdentity(context.Background(), changed); errName(err) != "authentication_unavailable" {
		t.Fatalf("changed identity did not write: %v", err)
	}
	s.now = func() time.Time { return now.Add(identity.LastSeenGranularity) }
	if _, err := s.recordIdentity(context.Background(), p); errName(err) != "authentication_unavailable" {
		t.Fatalf("identity seen five minutes ago did not write: %v", err)
	}
}
