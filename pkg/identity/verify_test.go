package identity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

const (
	testIssuer  = "http://127.0.0.1:8180/realms/zs-reference"
	otherIssuer = "http://127.0.0.1:8180/realms/zs-second"
	testKid     = "kid-1"
)

var (
	testKey, _  = rsa.GenerateKey(rand.Reader, 2048)
	otherKey, _ = rsa.GenerateKey(rand.Reader, 2048)
	weakKey, _  = rsa.GenerateKey(rand.Reader, 1024)
	testNow     = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	brandRef    = BrandConfig{ID: idgen.MustNewV7(), Slug: "zs-reference", Realm: "zs-reference",
		CustomerClientID: "zs-customer", StaffClientID: "zs-staff", ServiceClientIDs: []string{"zs-billing"},
		MFAACRValues: []string{"gold"}}
	brandSecond = BrandConfig{ID: idgen.MustNewV7(), Slug: "zs-second", Realm: "zs-second",
		CustomerClientID: "zs-second-customer", StaffClientID: "zs-second-staff"}
)

type fakeBrands struct{ err error }

func (f fakeBrands) BrandByRealm(_ context.Context, realm string) (BrandConfig, error) {
	if f.err != nil {
		return BrandConfig{}, f.err
	}
	switch realm {
	case brandRef.Realm:
		return brandRef, nil
	case brandSecond.Realm:
		return brandSecond, nil
	}
	return BrandConfig{}, ErrBrandUnknown
}

type fakeKeys struct {
	keys map[string]*rsa.PublicKey
	err  error
}

func (f fakeKeys) Issuer(_ context.Context, realm string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "http://127.0.0.1:8180/realms/" + realm, nil
}

func (f fakeKeys) PublicKey(_ context.Context, realm, kid string) (*rsa.PublicKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	k, ok := f.keys[realm+"/"+kid]
	if !ok {
		return nil, ErrUnknownKey
	}
	return k, nil
}

func newVerifier() *Verifier {
	return &Verifier{
		Brands: fakeBrands{},
		Keys: fakeKeys{keys: map[string]*rsa.PublicKey{
			"zs-reference/" + testKid: &testKey.PublicKey,
			"zs-second/" + testKid:    &otherKey.PublicKey,
			"zs-reference/weak":       &weakKey.PublicKey,
		}},
		Now: func() time.Time { return testNow },
	}
}

func baseClaims() map[string]any {
	return map[string]any{
		"iss":                testIssuer,
		"sub":                "5633a1df-1c81-4f09-b3d0-35c7761a60e8",
		"aud":                []string{"zs-customer", "account"},
		"azp":                "zs-customer",
		"typ":                "Bearer",
		"exp":                testNow.Add(5 * time.Minute).Unix(),
		"iat":                testNow.Add(-time.Minute).Unix(),
		"auth_time":          testNow.Add(-2 * time.Minute).Unix(),
		"email":              "person@example.test",
		"email_verified":     true,
		"name":               "Test Person",
		"preferred_username": "person",
		"amr":                []string{"pwd"},
		"acr":                "1",
	}
}

func sign(t *testing.T, header map[string]any, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(header) + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func token(t *testing.T, mutate func(h, c map[string]any)) string {
	h := map[string]any{"alg": "RS256", "typ": "JWT", "kid": testKid}
	c := baseClaims()
	if mutate != nil {
		mutate(h, c)
	}
	return sign(t, h, c, testKey)
}

func code(err error) string {
	var e *zserrors.Error
	if !errors.As(err, &e) {
		return "untyped"
	}
	return zserrors.Lookup(e.Code).Name
}

func TestVerifyAcceptsValidCustomerToken(t *testing.T) {
	p, err := newVerifier().Verify(context.Background(), token(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.Brand.ID != brandRef.ID || p.Kind != KindUser || p.Issuer != testIssuer || p.ClientID != "zs-customer" ||
		!p.EmailVerified || p.Email != "person@example.test" || p.MFA || p.AuthTime.IsZero() {
		t.Fatalf("principal = %+v", p)
	}
}

func TestVerifyKinds(t *testing.T) {
	v := newVerifier()
	for client, want := range map[string]Kind{"zs-customer": KindUser, "zs-staff": KindStaff, "zs-billing": KindService} {
		p, err := v.Verify(context.Background(), token(t, func(_, c map[string]any) {
			c["azp"], c["aud"] = client, client // single-string audience form
		}))
		if err != nil || p.Kind != want {
			t.Errorf("%s: kind %q, %v", client, p.Kind, err)
		}
	}
}

func TestVerifyRejections(t *testing.T) {
	v := newVerifier()
	cases := []struct {
		name   string
		mutate func(h, c map[string]any)
		raw    string
		want   string
	}{
		{name: "empty", raw: "", want: "token_invalid"},
		{name: "not a jwt", raw: "abc.def", want: "token_invalid"},
		{name: "garbage segments", raw: "a.b.c", want: "token_invalid"},
		{name: "oversized", raw: strings.Repeat("a", MaxTokenBytes+1), want: "token_invalid"},
		{name: "alg none", mutate: func(h, _ map[string]any) { h["alg"] = "none" }, want: "token_invalid"},
		{name: "alg HS256", mutate: func(h, _ map[string]any) { h["alg"] = "HS256" }, want: "token_invalid"},
		{name: "alg RS512", mutate: func(h, _ map[string]any) { h["alg"] = "RS512" }, want: "token_invalid"},
		{name: "no kid", mutate: func(h, _ map[string]any) { delete(h, "kid") }, want: "token_invalid"},
		{name: "unknown kid", mutate: func(h, _ map[string]any) { h["kid"] = "rotated-away" }, want: "token_invalid"},
		{name: "weak key", mutate: func(h, _ map[string]any) { h["kid"] = "weak" }, want: "token_invalid"},
		{name: "crit header", mutate: func(h, _ map[string]any) { h["crit"] = []string{"exp"} }, want: "token_invalid"},
		{name: "unknown realm", mutate: func(_, c map[string]any) { c["iss"] = "http://127.0.0.1:8180/realms/zs-unknown" }, want: "token_audience_unknown"},
		{name: "issuer not a realm", mutate: func(_, c map[string]any) { c["iss"] = "https://idp.example.test" }, want: "token_audience_unknown"},
		{name: "issuer other host", mutate: func(_, c map[string]any) { c["iss"] = "https://evil.example.test/realms/zs-reference" }, want: "token_invalid"},
		{name: "typ ID", mutate: func(_, c map[string]any) { c["typ"] = "ID" }, want: "token_invalid"},
		{name: "typ missing", mutate: func(_, c map[string]any) { delete(c, "typ") }, want: "token_invalid"},
		{name: "no subject", mutate: func(_, c map[string]any) { delete(c, "sub") }, want: "token_invalid"},
		{name: "no exp", mutate: func(_, c map[string]any) { delete(c, "exp") }, want: "token_invalid"},
		{name: "expired beyond leeway", mutate: func(_, c map[string]any) { c["exp"] = testNow.Add(-61 * time.Second).Unix() }, want: "token_invalid"},
		{name: "nbf beyond leeway", mutate: func(_, c map[string]any) { c["nbf"] = testNow.Add(61 * time.Second).Unix() }, want: "token_invalid"},
		{name: "iat in the future", mutate: func(_, c map[string]any) { c["iat"] = testNow.Add(10 * time.Minute).Unix() }, want: "token_invalid"},
		{name: "exp not a number", mutate: func(_, c map[string]any) { c["exp"] = "tomorrow" }, want: "token_invalid"},
		{name: "auth_time not a number", mutate: func(_, c map[string]any) { c["auth_time"] = "x" }, want: "token_invalid"},
		{name: "azp missing", mutate: func(_, c map[string]any) { delete(c, "azp") }, want: "token_audience_unknown"},
		{name: "azp other client", mutate: func(_, c map[string]any) { c["azp"] = "account-console" }, want: "token_audience_unknown"},
		{name: "aud lacks azp", mutate: func(_, c map[string]any) { c["aud"] = []string{"account"} }, want: "token_audience_unknown"},
		{name: "aud missing", mutate: func(_, c map[string]any) { delete(c, "aud") }, want: "token_audience_unknown"},
		{name: "client of another brand", mutate: func(_, c map[string]any) {
			c["azp"], c["aud"] = "zs-second-customer", []string{"zs-second-customer"}
		}, want: "token_audience_unknown"},
	}
	for _, tc := range cases {
		raw := tc.raw
		if tc.mutate != nil {
			raw = token(t, tc.mutate)
		}
		_, err := v.Verify(context.Background(), raw)
		if got := code(err); got != tc.want {
			t.Errorf("%s: got %s (%v), want %s", tc.name, got, err, tc.want)
		}
	}
}

func TestVerifyLeewayAccepted(t *testing.T) {
	v := newVerifier()
	raw := token(t, func(_, c map[string]any) {
		c["exp"] = testNow.Add(-59 * time.Second).Unix()
		c["nbf"] = testNow.Add(59 * time.Second).Unix()
	})
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("within leeway: %v", err)
	}
}

func TestVerifyRejectsTamperedAndForeignSignatures(t *testing.T) {
	v := newVerifier()
	good := token(t, nil)
	parts := strings.Split(good, ".")
	c := baseClaims()
	c["sub"] = "someone-else"
	payload, _ := json.Marshal(c)
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]
	if _, err := v.Verify(context.Background(), tampered); code(err) != "token_invalid" {
		t.Fatalf("tampered payload: %v", err)
	}
	// Signed by the zs-second realm key but claiming the zs-reference issuer.
	foreign := sign(t, map[string]any{"alg": "RS256", "kid": testKid}, baseClaims(), otherKey)
	if _, err := v.Verify(context.Background(), foreign); code(err) != "token_invalid" {
		t.Fatalf("foreign key: %v", err)
	}
}

func TestVerifyMapsAudienceToBrand(t *testing.T) {
	v := newVerifier()
	second := sign(t, map[string]any{"alg": "RS256", "kid": testKid}, map[string]any{
		"iss": otherIssuer, "sub": "s", "aud": "zs-second-staff", "azp": "zs-second-staff", "typ": "Bearer",
		"exp": testNow.Add(time.Minute).Unix(),
	}, otherKey)
	p, err := v.Verify(context.Background(), second)
	if err != nil || p.Brand.ID != brandSecond.ID || p.Kind != KindStaff {
		t.Fatalf("zs-second staff token: %+v %v", p, err)
	}
	// The same client IDs under the other realm do not map.
	cross := sign(t, map[string]any{"alg": "RS256", "kid": testKid}, map[string]any{
		"iss": otherIssuer, "sub": "s", "aud": "zs-staff", "azp": "zs-staff", "typ": "Bearer",
		"exp": testNow.Add(time.Minute).Unix(),
	}, otherKey)
	if _, err := v.Verify(context.Background(), cross); code(err) != "token_audience_unknown" {
		t.Fatalf("zs-reference client in zs-second realm: %v", err)
	}
}

func TestVerifyUnavailableIsNever401(t *testing.T) {
	raw := token(t, nil)
	keysDown := newVerifier()
	keysDown.Keys = fakeKeys{err: errors.Join(ErrKeysUnavailable, errors.New("dial tcp: connection refused"))}
	if _, err := keysDown.Verify(context.Background(), raw); code(err) != "authentication_unavailable" {
		t.Fatalf("JWKS down: %v", err)
	}
	dbDown := newVerifier()
	dbDown.Brands = fakeBrands{err: errors.New("database is down")}
	if _, err := dbDown.Verify(context.Background(), raw); code(err) != "authentication_unavailable" {
		t.Fatalf("brand lookup down: %v", err)
	}
	status, _ := zserrors.ToEnvelope(zserrors.New(zserrors.CodeAuthenticationUnavailable, ""))
	if status != 503 {
		t.Fatalf("authentication_unavailable status = %d", status)
	}
}

func TestEmailVerifiedNeedsBooleanTrue(t *testing.T) {
	v := newVerifier()
	for name, value := range map[string]any{"string": "true", "false": false, "missing": nil} {
		p, err := v.Verify(context.Background(), token(t, func(_, c map[string]any) {
			if value == nil {
				delete(c, "email_verified")
			} else {
				c["email_verified"] = value
			}
		}))
		if err != nil || p.EmailVerified {
			t.Errorf("%s: verified=%v err=%v", name, p.EmailVerified, err)
		}
		if RequireVerifiedEmail(p) == nil {
			t.Errorf("%s: RequireVerifiedEmail passed", name)
		}
	}
}

func TestMFAFromAMRAndACR(t *testing.T) {
	v := newVerifier()
	cases := []struct {
		amr  []string
		acr  string
		want bool
	}{
		{[]string{"pwd"}, "1", false},
		{[]string{"pwd", "otp"}, "1", true},
		{[]string{"hwk"}, "", true},
		{[]string{"MFA"}, "", true},
		{nil, "gold", true}, // configured acr for zs-reference
		{nil, "2", false},   // not configured
		{[]string{}, "", false},
	}
	for _, tc := range cases {
		p, err := v.Verify(context.Background(), token(t, func(_, c map[string]any) { c["amr"], c["acr"] = tc.amr, tc.acr }))
		if err != nil || p.MFA != tc.want {
			t.Errorf("amr=%v acr=%q: mfa=%v err=%v", tc.amr, tc.acr, p.MFA, err)
		}
	}
}

func TestRequireRecentMFA(t *testing.T) {
	base := Principal{MFA: true, AuthTime: testNow.Add(-5 * time.Minute)}
	if err := RequireRecentMFA(base, 0, testNow); err != nil {
		t.Fatalf("recent MFA refused: %v", err)
	}
	cases := map[string]Principal{
		"no mfa":        {MFA: false, AuthTime: testNow.Add(-time.Minute)},
		"no auth_time":  {MFA: true},
		"too old":       {MFA: true, AuthTime: testNow.Add(-16 * time.Minute)},
		"far in future": {MFA: true, AuthTime: testNow.Add(2 * time.Minute)},
	}
	for name, p := range cases {
		if err := RequireRecentMFA(p, 15*time.Minute, testNow); code(err) != "recent_mfa_required" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := RequireRecentMFA(Principal{MFA: true, AuthTime: testNow.Add(-15 * time.Minute)}, 15*time.Minute, testNow); err != nil {
		t.Errorf("exactly at the window: %v", err)
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, err := PrincipalFromContext(context.Background()); code(err) != "unauthenticated" {
		t.Fatalf("empty context: %v", err)
	}
	p := Principal{Brand: brandRef, Subject: "s"}
	got, err := PrincipalFromContext(WithPrincipal(context.Background(), p))
	if err != nil || got.Subject != "s" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}

// Documents a known limit. Keycloak puts otp in amr for the whole SSO
// session once the user passed OTP, and a re-authentication inside that
// session without a second factor can refresh auth_time. Core treats
// auth_time as the time of the second factor, so such a token passes
// RequireRecentMFA. Brands that need stronger proof list a step-up acr value
// in mfa_acr_values, which the realm issues only after a second factor.
func TestRecentMFAReadsAuthTimeAsTheSecondFactorTime(t *testing.T) {
	v := newVerifier()
	// OTP passed an hour ago; a password-only re-authentication two minutes
	// ago refreshed auth_time while amr still carries otp.
	p, err := v.Verify(context.Background(), token(t, func(_, c map[string]any) {
		c["amr"], c["acr"] = []string{"pwd", "otp"}, "1"
		c["auth_time"] = testNow.Add(-2 * time.Minute).Unix()
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireRecentMFA(p, RecentMFAWindow, testNow); err != nil {
		t.Fatalf("current behaviour changed: %v", err)
	}
	// With a step-up acr and no otp in amr, only that acr proves the factor.
	p, err = v.Verify(context.Background(), token(t, func(_, c map[string]any) { c["amr"], c["acr"] = []string{"pwd"}, "1" }))
	if err != nil || p.MFA {
		t.Fatalf("pwd with acr 1: mfa=%v err=%v", p.MFA, err)
	}
	p, err = v.Verify(context.Background(), token(t, func(_, c map[string]any) { c["amr"], c["acr"] = []string{"pwd"}, "gold" }))
	if err != nil || !p.MFA {
		t.Fatalf("pwd with step-up acr: mfa=%v err=%v", p.MFA, err)
	}
}
