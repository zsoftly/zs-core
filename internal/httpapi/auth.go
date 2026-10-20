package httpapi

import (
	"context"
	"net/http"
	"strings"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/scope"
)

type identityKey struct{}

// callerIdentity returns the identity recorded for the request, if any. It
// is absent for a principal without a verified email.
func callerIdentity(ctx context.Context) (identity.Identity, bool) {
	i, ok := ctx.Value(identityKey{}).(identity.Identity)
	return i, ok
}

// bearerToken returns the token of an "Authorization: Bearer <token>"
// header, or false.
func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// authenticate resolves the scope of a /v1 request from its bearer token:
// the brand from the issuer and audience, the principal from the token. It
// puts a brand-level scope (brand set, tenant unset) on the context, and
// records the identity on first verified contact. It fails closed: no token
// is 401, a bad token is 401, and a verification that cannot be completed is
// 503 authentication_unavailable.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			s.writeError(w, r, zserrors.New(zserrors.CodeUnauthenticated, ""))
			return
		}
		if s.verifier == nil {
			s.writeError(w, r, zserrors.New(zserrors.CodeAuthenticationUnavailable, "Token verification is not configured."))
			return
		}
		p, err := s.verifier.Verify(r.Context(), raw)
		if err != nil {
			if zserrors.IsCode(err, zserrors.CodeTokenInvalid) || zserrors.IsCode(err, zserrors.CodeTokenAudienceUnknown) {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			}
			s.writeError(w, r, err)
			return
		}
		ctx := identity.WithPrincipal(r.Context(), p)
		ctx = scope.WithBrand(ctx, p.Brand.ID)
		if p.EmailVerified {
			ident, err := s.recordIdentity(ctx, p)
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			ctx = context.WithValue(ctx, identityKey{}, ident)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *server) recordIdentity(ctx context.Context, p identity.Principal) (identity.Identity, error) {
	unavailable := func(err error) error {
		return zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", err)
	}
	if s.db == nil {
		return identity.Identity{}, zserrors.New(zserrors.CodeAuthenticationUnavailable, "")
	}
	// Read first: most requests come from an identity that is already
	// recorded, unchanged and seen recently, and need no write.
	existing, err := identity.Get(ctx, s.db, p.Brand.ID, p.Issuer, p.Subject)
	switch {
	case err == nil && !identity.NeedsUpsert(existing, p, s.now()):
		return existing, nil
	case err != nil && !zserrors.IsCode(err, zserrors.CodeNotFound):
		return identity.Identity{}, unavailable(err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return identity.Identity{}, unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	ident, _, err := identity.Upsert(ctx, tx, p, RequestID(ctx))
	if err != nil {
		return identity.Identity{}, unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return identity.Identity{}, unavailable(err)
	}
	return ident, nil
}

// RequireStaff returns the caller when it is a staff principal of the brand
// in scope with an active grant for want. It needs the context authenticate
// builds. Anything else is 404, so a route's existence is not confirmed to
// callers without authority. Grants are looked up by the token's brand,
// issuer and subject only; a grant in another brand never counts, and
// Keycloak roles are never consulted.
func RequireStaff(ctx context.Context, db identity.Querier, want identity.StaffScope) (identity.Principal, error) {
	p, err := identity.PrincipalFromContext(ctx)
	if err != nil {
		return identity.Principal{}, err
	}
	brandID, err := scope.BrandFromContext(ctx)
	if err != nil || brandID != p.Brand.ID || p.Kind != identity.KindStaff {
		return identity.Principal{}, zserrors.New(zserrors.CodeNotFound, "")
	}
	if db == nil {
		return identity.Principal{}, zserrors.New(zserrors.CodeAuthenticationUnavailable, "")
	}
	ok, err := identity.HasStaffGrant(ctx, db, p, want)
	if err != nil {
		return identity.Principal{}, err
	}
	if !ok {
		return identity.Principal{}, zserrors.New(zserrors.CodeNotFound, "")
	}
	return p, nil
}

func (s *server) requireStaff(ctx context.Context, want identity.StaffScope) (identity.Principal, error) {
	if s.db == nil {
		return RequireStaff(ctx, nil, want)
	}
	return RequireStaff(ctx, s.db, want)
}
