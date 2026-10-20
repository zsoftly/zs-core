// Package apikey issues and verifies tenant API keys.
//
// A key belongs to one tenant of one brand and acts as the membership that
// created it. Its secret is zs_<brand slug>_<32 random bytes in unpadded
// base64url>. The secret is shown once, when the key is created. Core stores
// only its SHA-256 digest, and a short prefix for display. A key expires
// after 90 days by default and after 365 days at most. Its permissions are
// fixed at creation and must be a subset of the creator's. At every use they
// are intersected with the creator's current permissions, so a key shrinks
// when its creator is demoted and stops when its creator leaves. Revocation
// takes effect at once. Core also revokes a key itself when its creator is
// removed or suspended and when its tenant is closed, so a key never comes
// back to life when its creator is invited again.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

const (
	// SecretPrefix starts every API key secret. A bearer credential with
	// this prefix is an API key; a JWT never starts with it.
	SecretPrefix = "zs_"
	// secretBytes is the entropy of a secret.
	secretBytes = 32
	// encodedSecretLength is secretBytes in unpadded base64url.
	encodedSecretLength = 43
	// displayChars is how many characters of the random part the prefix
	// keeps for display.
	displayChars = 6
	// DefaultTTL and MaxTTL bound the life of a key.
	DefaultTTL = 90 * 24 * time.Hour
	MaxTTL     = 365 * 24 * time.Hour
	// LastUsedGranularity limits how often last_used_at is written.
	LastUsedGranularity = time.Minute
)

// Key is a row of the api_keys table. It never holds the secret.
type Key struct {
	ID                    idgen.UUID
	BrandID               idgen.UUID
	TenantID              idgen.UUID
	CreatedByMembershipID idgen.UUID
	Name                  string
	Prefix                string
	Permissions           []string
	ExpiresAt             time.Time
	LastUsedAt            *time.Time
	RevokedAt             *time.Time
	RevokedReason         string
	CreatedAt             time.Time
}

// PublicID returns the key_ ID.
func (k Key) PublicID() string { return idgen.Format(idgen.APIKey, k.ID) }

// Created is a new key with its secret, which is never available again.
type Created struct {
	Key
	Secret string
}

var (
	errInvalid      = zserrors.New(zserrors.CodeAPIKeyInvalid, "")
	errOwnerRemoved = zserrors.New(zserrors.CodeAPIKeyOwnerRemoved, "")
	// ErrNotFound is the answer for a key that is not in the tenant.
	ErrNotFound = zserrors.New(zserrors.CodeNotFound, "The API key was not found.")
)

func digestOf(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

func unavailable(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
}

// CreateRequest describes a key. A zero ExpiresAt means DefaultTTL from now.
type CreateRequest struct {
	Name        string
	Permissions []string
	ExpiresAt   time.Time
}

// keyColumnsOf lists the columns scanKey reads, qualified with the table
// alias a, such as "k.", or unqualified when a is empty.
func keyColumnsOf(a string) string {
	return a + "id, " + a + "brand_id, " + a + "tenant_id, " + a + "created_by_membership_id, " + a + "name, " + a + "prefix, " +
		a + "permissions, " + a + "expires_at, " + a + "last_used_at, " + a + "revoked_at, coalesce(" + a + "revoked_reason, ''), " +
		a + "created_at"
}

var keyColumns = keyColumnsOf("")

func scanKey(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.BrandID, &k.TenantID, &k.CreatedByMembershipID, &k.Name, &k.Prefix, &k.Permissions,
		&k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt, &k.RevokedReason, &k.CreatedAt)
	if k.Permissions == nil {
		k.Permissions = []string{}
	}
	return k, err
}

// Create issues a key for the caller's membership in the tenant of s. It
// needs apikey:create and recent MFA, so an API key can never create another.
// Every requested permission must be registered for the brand and held by
// the caller. The tenant must be active.
func Create(ctx context.Context, tx pgx.Tx, s scope.Scope, caller tenant.Caller, req CreateRequest) (Created, error) {
	if !caller.Has("apikey:create") {
		return Created{}, scope.ErrPermissionDenied
	}
	if !caller.RecentMFA {
		return Created{}, zserrors.New(zserrors.CodeRecentMFARequired, "")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > 100 || !utf8.ValidString(name) {
		return Created{}, zserrors.New(zserrors.CodeValidation, "The name must be 1 to 100 characters.")
	}
	if len(req.Permissions) == 0 || len(req.Permissions) > tenant.MaxLimit {
		return Created{}, zserrors.New(zserrors.CodeValidation, "Give 1 to 200 permissions.")
	}
	perms := tenant.Intersect(req.Permissions, req.Permissions)
	registered, err := tenant.Registered(ctx, tx, s.Brand)
	if err != nil {
		return Created{}, err
	}
	if err := tenant.CheckRegistered(perms, registered); err != nil {
		return Created{}, err
	}
	if !tenant.Subset(perms, caller.Permissions) {
		return Created{}, zserrors.New(zserrors.CodePermissionDenied, "An API key cannot hold a permission its creator does not hold.")
	}
	now := time.Now().UTC()
	expires := req.ExpiresAt.UTC()
	if req.ExpiresAt.IsZero() {
		expires = now.Add(DefaultTTL)
	}
	if !expires.After(now) || expires.After(now.Add(MaxTTL)) {
		return Created{}, zserrors.New(zserrors.CodeValidation, "expires_at must be in the future and at most 365 days away.")
	}
	args, err := s.Bind()
	if err != nil {
		return Created{}, err
	}
	var slug, status string
	if err := tx.QueryRow(ctx, `SELECT b.slug, t.status FROM tenants t JOIN brands b ON b.id = t.brand_id
WHERE t.brand_id = $1 AND t.id = $2 FOR UPDATE OF t`, args...).Scan(&slug, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Created{}, tenant.ErrNotFound
		}
		return Created{}, unavailable(err)
	}
	if status != string(tenant.StatusActive) {
		return Created{}, tenant.ErrNotFound
	}
	// The tenant row is locked: check the caller again on its membership as
	// it is now.
	caller, err = tenant.CurrentCaller(ctx, tx, s, caller)
	if err != nil {
		return Created{}, err
	}
	if !caller.Has("apikey:create") {
		return Created{}, scope.ErrPermissionDenied
	}
	if !tenant.Subset(perms, caller.Permissions) {
		return Created{}, zserrors.New(zserrors.CodePermissionDenied, "An API key cannot hold a permission its creator does not hold.")
	}
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Created{}, err
	}
	random := base64.RawURLEncoding.EncodeToString(raw)
	secret := SecretPrefix + slug + "_" + random
	prefix := SecretPrefix + slug + "_" + random[:displayChars]
	id, err := idgen.NewV7()
	if err != nil {
		return Created{}, err
	}
	args, err = s.Bind(id, caller.Membership.ID, name, digestOf(secret), prefix, perms, expires)
	if err != nil {
		return Created{}, err
	}
	k, err := scanKey(tx.QueryRow(ctx, `INSERT INTO api_keys
    (brand_id, tenant_id, id, created_by_membership_id, name, digest, prefix, permissions, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+keyColumns, args...))
	if err != nil {
		return Created{}, unavailable(err)
	}
	if _, err := audit.Write(ctx, tx, s, audit.Entry{
		Actor: caller.Actor, Action: "tenant.api_key.created", Outcome: audit.OutcomeSuccess, CorrelationID: caller.CorrelationID,
		Details: map[string]any{"api_key_id": k.PublicID(), "prefix": prefix, "permissions": perms, "expires_at": expires.Format(time.RFC3339)},
	}); err != nil {
		return Created{}, err
	}
	return Created{Key: k, Secret: secret}, nil
}

// List returns the keys of the tenant, revoked and expired ones included,
// oldest first. It needs apikey:read.
func List(ctx context.Context, q tenant.Querier, s scope.Scope, caller tenant.Caller, page tenant.Page) ([]Key, string, error) {
	if !caller.Has("apikey:read") {
		return nil, "", scope.ErrPermissionDenied
	}
	limit, after, err := page.After()
	if err != nil {
		return nil, "", err
	}
	args, err := s.Bind(after, limit+1)
	if err != nil {
		return nil, "", err
	}
	rows, err := q.Query(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE brand_id = $1 AND tenant_id = $2 AND id > $3 ORDER BY id LIMIT $4`, args...)
	if err != nil {
		return nil, "", unavailable(err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) { return scanKey(r) })
	if err != nil {
		return nil, "", unavailable(err)
	}
	if len(list) <= limit {
		return list, "", nil
	}
	list = list[:limit]
	return list, tenant.EncodeCursor(list[limit-1].ID), nil
}

// Revoke revokes a key of the tenant at once. It needs apikey:revoke. A key
// of another tenant is not_found. Revoking a revoked key returns it
// unchanged.
func Revoke(ctx context.Context, tx pgx.Tx, s scope.Scope, caller tenant.Caller, id idgen.UUID, reason string) (Key, error) {
	if !caller.Has("apikey:revoke") {
		return Key{}, scope.ErrPermissionDenied
	}
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > 1000 {
		return Key{}, zserrors.New(zserrors.CodeValidation, "The reason is too long.")
	}
	args, err := s.Bind(id)
	if err != nil {
		return Key{}, err
	}
	k, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 FOR UPDATE`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, unavailable(err)
	}
	// Check the caller again on its membership as it is now.
	caller, err = tenant.CurrentCaller(ctx, tx, s, caller)
	if err != nil {
		return Key{}, err
	}
	if !caller.Has("apikey:revoke") {
		return Key{}, scope.ErrPermissionDenied
	}
	if k.RevokedAt != nil {
		return k, nil
	}
	args, err = s.Bind(id, nullable(reason))
	if err != nil {
		return Key{}, err
	}
	k, err = scanKey(tx.QueryRow(ctx, `UPDATE api_keys SET revoked_at = now(), revoked_reason = $4
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING `+keyColumns, args...))
	if err != nil {
		return Key{}, unavailable(err)
	}
	if _, err := audit.Write(ctx, tx, s, audit.Entry{
		Actor: caller.Actor, Action: "tenant.api_key.revoked", Reason: reason, Outcome: audit.OutcomeSuccess,
		CorrelationID: caller.CorrelationID, Details: map[string]any{"api_key_id": k.PublicID(), "prefix": k.Prefix},
	}); err != nil {
		return Key{}, err
	}
	return k, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Verified is a key that passed Verify.
type Verified struct {
	Key          Key
	BrandSlug    string
	TenantStatus tenant.Status
	Membership   tenant.Membership
	// Permissions are the key's permissions intersected with the creator's
	// current effective permissions.
	Permissions []string
}

// Scope returns the brand and tenant of the key.
func (v Verified) Scope() scope.Scope {
	return scope.Scope{Brand: v.Key.BrandID, Tenant: v.Key.TenantID}
}

// ParseSecret splits a secret into the brand slug and the random part. It
// reports false for anything that is not an API key secret.
func ParseSecret(secret string) (slug string, ok bool) {
	rest, ok := strings.CutPrefix(secret, SecretPrefix)
	if !ok {
		return "", false
	}
	slug, random, ok := strings.Cut(rest, "_")
	if !ok || !secrets.ValidSlug(slug) || len(random) != encodedSecretLength {
		return "", false
	}
	if _, err := base64.RawURLEncoding.DecodeString(random); err != nil {
		return "", false
	}
	return slug, true
}

// Verify checks an API key secret and returns the key, its brand, tenant and
// creating membership, and its effective permissions. A malformed, unknown,
// expired or revoked key, or one of a brand that is not active, is
// api_key_invalid. A key whose creator is no longer an active member is
// api_key_owner_removed. last_used_at is written at most once a minute, in
// tx. The tenant's status is returned for scope resolution, which refuses
// writes to a suspended tenant and everything on a closed one.
func Verify(ctx context.Context, tx pgx.Tx, secret string) (Verified, error) {
	slug, ok := ParseSecret(secret)
	if !ok {
		return Verified{}, errInvalid
	}
	digest := digestOf(secret)
	var (
		v      Verified
		stored []byte
		status string
	)
	row := tx.QueryRow(ctx, `SELECT `+keyColumnsOf("k.")+`, k.digest, b.slug, t.status
FROM api_keys k
JOIN brands b ON b.id = k.brand_id
JOIN tenants t ON t.brand_id = k.brand_id AND t.id = k.tenant_id
WHERE k.digest = $1 AND b.slug = $2 AND b.status = 'active' AND b.applied_at IS NOT NULL`, digest, slug)
	err := row.Scan(&v.Key.ID, &v.Key.BrandID, &v.Key.TenantID, &v.Key.CreatedByMembershipID, &v.Key.Name, &v.Key.Prefix,
		&v.Key.Permissions, &v.Key.ExpiresAt, &v.Key.LastUsedAt, &v.Key.RevokedAt, &v.Key.RevokedReason, &v.Key.CreatedAt,
		&stored, &v.BrandSlug, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verified{}, errInvalid
	}
	if err != nil {
		return Verified{}, zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", err)
	}
	if subtle.ConstantTimeCompare(stored, digest) != 1 || v.Key.RevokedAt != nil || !time.Now().Before(v.Key.ExpiresAt) {
		return Verified{}, errInvalid
	}
	v.TenantStatus = tenant.Status(status)
	perms, m, err := tenant.Effective(ctx, tx, v.Scope(), v.Key.CreatedByMembershipID)
	if zserrors.IsCode(err, zserrors.CodeNotFound) {
		return Verified{}, errOwnerRemoved
	}
	if err != nil {
		return Verified{}, zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", err)
	}
	if m.Status != tenant.MembershipActive {
		return Verified{}, errOwnerRemoved
	}
	v.Membership = m
	v.Permissions = tenant.Intersect(v.Key.Permissions, perms)
	args, err := v.Scope().Bind(v.Key.ID, int(LastUsedGranularity.Seconds()))
	if err != nil {
		return Verified{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE api_keys SET last_used_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3
  AND (last_used_at IS NULL OR last_used_at <= now() - make_interval(secs => $4))`, args...); err != nil {
		return Verified{}, zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", err)
	}
	return v, nil
}
