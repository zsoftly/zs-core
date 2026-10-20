// Package identity verifies Keycloak access tokens, maps them to a brand and
// records the identities and staff grants of each brand.
//
// A token acts inside exactly one brand. The brand is chosen by the issuer
// (one Keycloak realm per brand) and confirmed by the audience and the
// authorized party, which must be clients the brand's manifest declares.
// Verification fails closed: an unknown realm, audience or client is
// token_audience_unknown, any other defect is token_invalid, and a signing
// key or brand lookup that cannot be completed is authentication_unavailable
// (503), never 401.
package identity

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Leeway is the clock skew accepted on exp, nbf and iat.
const Leeway = 60 * time.Second

// MaxTokenBytes caps the size of a bearer token.
const MaxTokenBytes = 16 << 10

// RecentMFAWindow is the age limit of RequireRecentMFA in the contract.
const RecentMFAWindow = 15 * time.Minute

// Kind is the kind of principal a token represents, decided by the client
// that obtained it.
type Kind string

const (
	// KindUser is a person signed in through the brand's customer client.
	KindUser Kind = "user"
	// KindStaff is a person signed in through the brand's staff client.
	// Staff authority still needs an active staff grant.
	KindStaff Kind = "staff"
	// KindService is a service account of one of the brand's service
	// clients.
	KindService Kind = "service"
)

// BrandConfig is what verification needs to know about a brand.
type BrandConfig struct {
	ID               idgen.UUID
	Slug             string
	Realm            string
	CustomerClientID string
	StaffClientID    string
	ServiceClientIDs []string
	// MFAACRValues are acr values that prove a second factor. Empty means
	// only amr is consulted.
	MFAACRValues []string
}

// PublicID returns the brand's public ID.
func (b BrandConfig) PublicID() string { return idgen.Format(idgen.Brand, b.ID) }

// kindOf returns the principal kind for an authorized party, or false when
// the client is not one of the brand's.
func (b BrandConfig) kindOf(clientID string) (Kind, bool) {
	switch {
	case clientID == "":
		return "", false
	case clientID == b.StaffClientID:
		return KindStaff, true
	case clientID == b.CustomerClientID:
		return KindUser, true
	}
	for _, c := range b.ServiceClientIDs {
		if c == clientID {
			return KindService, true
		}
	}
	return "", false
}

// BrandResolver finds the brand that owns a Keycloak realm.
type BrandResolver interface {
	// BrandByRealm returns ErrBrandUnknown when no active brand uses the
	// realm, and any other error when the lookup could not be completed.
	BrandByRealm(ctx context.Context, realm string) (BrandConfig, error)
}

// KeySource provides the issuer and the signing keys of a realm.
type KeySource interface {
	// Issuer returns the issuer the realm puts in its tokens.
	Issuer(ctx context.Context, realm string) (string, error)
	// PublicKey returns the RS256 key with the given key ID. It returns
	// ErrUnknownKey when the realm has no such key and ErrKeysUnavailable
	// (wrapped) when the keys cannot be fetched.
	PublicKey(ctx context.Context, realm, kid string) (*rsa.PublicKey, error)
}

// Sentinel errors for resolvers and key sources.
var (
	ErrBrandUnknown    = errors.New("no brand for this realm")
	ErrUnknownKey      = errors.New("unknown signing key")
	ErrKeysUnavailable = errors.New("signing keys unavailable")
)

// Principal is a verified token.
type Principal struct {
	Brand         BrandConfig
	Kind          Kind
	Issuer        string
	Subject       string
	ClientID      string
	Email         string
	EmailVerified bool
	Name          string
	Username      string
	// AuthTime is when the user last authenticated interactively. Zero when
	// the token does not say.
	AuthTime  time.Time
	MFA       bool
	ExpiresAt time.Time
}

// Verifier verifies bearer tokens.
type Verifier struct {
	Brands BrandResolver
	Keys   KeySource
	// Now defaults to time.Now.
	Now func() time.Time
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

var (
	errInvalid     = zserrors.New(zserrors.CodeTokenInvalid, "")
	errAudience    = zserrors.New(zserrors.CodeTokenAudienceUnknown, "")
	errUnavailable = func(cause error) error { return zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", cause) }
)

type header struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Typ  string   `json:"typ"`
	Crit []string `json:"crit"`
}

type claims struct {
	Issuer        string          `json:"iss"`
	Subject       string          `json:"sub"`
	Audience      audience        `json:"aud"`
	AZP           string          `json:"azp"`
	Typ           string          `json:"typ"`
	Exp           *json.Number    `json:"exp"`
	Nbf           *json.Number    `json:"nbf"`
	Iat           *json.Number    `json:"iat"`
	AuthTime      *json.Number    `json:"auth_time"`
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
	Username      string          `json:"preferred_username"`
	AMR           []string        `json:"amr"`
	ACR           string          `json:"acr"`
}

// audience accepts the string and the array form of aud.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// Verify checks raw, a compact JWS access token, and returns the principal.
//
// The order matters. The issuer is read before the signature is checked
// only to pick the realm and its keys; nothing else in the token is trusted
// until the signature verifies against a key of that realm and the issuer
// equals the one the realm publishes.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if raw == "" || len(raw) > MaxTokenBytes {
		return Principal{}, errInvalid
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Principal{}, errInvalid
	}
	var h header
	if err := decodeSegment(parts[0], &h); err != nil {
		return Principal{}, errInvalid
	}
	if h.Alg != "RS256" || h.Kid == "" || len(h.Crit) > 0 {
		return Principal{}, errInvalid
	}
	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Principal{}, errInvalid
	}
	realm, ok := realmOf(c.Issuer)
	if !ok {
		return Principal{}, errAudience
	}

	brand, err := v.Brands.BrandByRealm(ctx, realm)
	if errors.Is(err, ErrBrandUnknown) {
		return Principal{}, errAudience
	}
	if err != nil {
		return Principal{}, errUnavailable(err)
	}
	issuer, err := v.Keys.Issuer(ctx, realm)
	if err != nil {
		if errors.Is(err, ErrUnknownKey) {
			return Principal{}, errInvalid
		}
		return Principal{}, errUnavailable(err)
	}
	key, err := v.Keys.PublicKey(ctx, realm, h.Kid)
	if err != nil {
		if errors.Is(err, ErrUnknownKey) {
			return Principal{}, errInvalid
		}
		return Principal{}, errUnavailable(err)
	}
	if err := verifyRS256(key, parts[0]+"."+parts[1], parts[2]); err != nil {
		return Principal{}, errInvalid
	}

	// The signature is valid: the claims can now be trusted.
	if c.Issuer != issuer {
		return Principal{}, errInvalid
	}
	if c.Typ != "Bearer" || c.Subject == "" {
		return Principal{}, errInvalid
	}
	now := v.now()
	exp, ok := unixTime(c.Exp)
	if !ok || !now.Before(exp.Add(Leeway)) {
		return Principal{}, errInvalid
	}
	if c.Nbf != nil {
		nbf, ok := unixTime(c.Nbf)
		if !ok || now.Add(Leeway).Before(nbf) {
			return Principal{}, errInvalid
		}
	}
	if c.Iat != nil {
		iat, ok := unixTime(c.Iat)
		if !ok || now.Add(Leeway).Before(iat) {
			return Principal{}, errInvalid
		}
	}

	kind, ok := brand.kindOf(c.AZP)
	if !ok || !contains(c.Audience, c.AZP) {
		return Principal{}, errAudience
	}

	p := Principal{
		Brand:     brand,
		Kind:      kind,
		Issuer:    c.Issuer,
		Subject:   c.Subject,
		ClientID:  c.AZP,
		Email:     c.Email,
		Name:      c.Name,
		Username:  c.Username,
		ExpiresAt: exp,
		MFA:       mfaFrom(c.AMR, c.ACR, brand.MFAACRValues),
	}
	p.EmailVerified = bytes.Equal(bytes.TrimSpace(c.EmailVerified), []byte("true")) && c.Email != ""
	if c.AuthTime != nil {
		at, ok := unixTime(c.AuthTime)
		if !ok {
			return Principal{}, errInvalid
		}
		p.AuthTime = at
	}
	return p, nil
}

func decodeSegment(segment string, out any) error {
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

func verifyRS256(key *rsa.PublicKey, signingInput, signature string) error {
	if key == nil || key.N.BitLen() < 2048 {
		return errors.New("weak or missing key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(signingInput))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig)
}

// realmOf extracts the realm from a Keycloak issuer of the form
// <base>/realms/<realm>.
func realmOf(issuer string) (string, bool) {
	i := strings.LastIndex(issuer, "/realms/")
	if i <= 0 {
		return "", false
	}
	realm := issuer[i+len("/realms/"):]
	if realm == "" || strings.ContainsAny(realm, "/?#") {
		return "", false
	}
	return realm, true
}

func unixTime(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	v, err := n.Int64()
	if err != nil || v <= 0 {
		f, ferr := n.Float64()
		if ferr != nil || f <= 0 {
			return time.Time{}, false
		}
		v = int64(f)
	}
	return time.Unix(v, 0).UTC(), true
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// mfaMethods are the RFC 8176 amr values that prove a second factor or a
// phishing-resistant key. Keycloak emits them through the AMR protocol mapper
// from the reference values Core sets on the brand's authentication flows.
var mfaMethods = map[string]bool{"mfa": true, "otp": true, "hwk": true, "swk": true, "sc": true}

func mfaFrom(amr []string, acr string, mfaACR []string) bool {
	for _, m := range amr {
		if mfaMethods[strings.ToLower(m)] {
			return true
		}
	}
	acr = strings.TrimSpace(acr)
	for _, v := range mfaACR {
		if acr != "" && acr == v {
			return true
		}
	}
	return false
}

// RequireRecentMFA returns recent_mfa_required unless p authenticated with a
// second factor within window of now. A zero window means RecentMFAWindow. An
// auth_time in the future beyond the leeway is refused.
func RequireRecentMFA(p Principal, window time.Duration, now time.Time) error {
	if window <= 0 {
		window = RecentMFAWindow
	}
	if !p.MFA || p.AuthTime.IsZero() || now.Sub(p.AuthTime) > window || p.AuthTime.After(now.Add(Leeway)) {
		return zserrors.New(zserrors.CodeRecentMFARequired, "")
	}
	return nil
}

// RequireVerifiedEmail returns email_not_verified unless the token carries a
// verified email.
func RequireVerifiedEmail(p Principal) error {
	if !p.EmailVerified {
		return zserrors.New(zserrors.CodeEmailNotVerified, "")
	}
	return nil
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the principal in ctx, or unauthenticated.
func PrincipalFromContext(ctx context.Context) (Principal, error) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	if !ok || p.Subject == "" || p.Brand.ID.IsNil() {
		return Principal{}, zserrors.New(zserrors.CodeUnauthenticated, "")
	}
	return p, nil
}

// String renders a principal for logs without personal data.
func (p Principal) String() string {
	return fmt.Sprintf("%s:%s@%s", p.Kind, p.Subject, p.Brand.Slug)
}
