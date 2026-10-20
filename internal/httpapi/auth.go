package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/zsoftly/zs-core/pkg/apikey"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

type identityKey struct{}

type apiKeyKey struct{}

type callerKey struct{}

// callerAPIKey returns the verified API key of the request, if the caller
// presented one.
func callerAPIKey(ctx context.Context) (apikey.Verified, bool) {
	v, ok := ctx.Value(apiKeyKey{}).(apikey.Verified)
	return v, ok
}

// tenantCaller returns the caller that scope resolution built for a tenant
// route.
func tenantCaller(ctx context.Context) (tenant.Caller, error) {
	c, ok := ctx.Value(callerKey{}).(tenant.Caller)
	if !ok {
		return tenant.Caller{}, scope.ErrPermissionDenied
	}
	return c, nil
}

// errTokenOnly is the answer to an API key on a route that needs a person's
// token.
var errTokenOnly = zserrors.New(zserrors.CodePermissionDenied, "API keys work on tenant routes only.")

// userPrincipal returns the caller when it is a person signed in through the
// customer client, which is the only principal that creates tenants and
// accepts invitations. An API key is permission_denied, and so is a staff or
// service token.
func userPrincipal(ctx context.Context) (identity.Principal, error) {
	if _, ok := callerAPIKey(ctx); ok {
		return identity.Principal{}, errTokenOnly
	}
	p, err := identity.PrincipalFromContext(ctx)
	if err != nil {
		return identity.Principal{}, err
	}
	if p.Kind != identity.KindUser {
		return identity.Principal{}, zserrors.New(zserrors.CodePermissionDenied, "This route needs a customer token.")
	}
	return p, nil
}

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
		if strings.HasPrefix(raw, apikey.SecretPrefix) {
			s.authenticateAPIKey(w, r, raw, next)
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

// authenticateAPIKey verifies an API key and puts a brand-level scope and
// the verified key on the context. The key's brand comes from its prefix and
// is confirmed by the stored key. A database that cannot be reached is 503
// authentication_unavailable.
func (s *server) authenticateAPIKey(w http.ResponseWriter, r *http.Request, raw string, next http.Handler) {
	ctx := r.Context()
	if s.db == nil {
		s.writeError(w, r, zserrors.New(zserrors.CodeAuthenticationUnavailable, ""))
		return
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", err))
		return
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	v, err := apikey.Verify(ctx, tx, raw)
	if err != nil {
		if zserrors.IsCode(err, zserrors.CodeAPIKeyInvalid) || zserrors.IsCode(err, zserrors.CodeAPIKeyOwnerRemoved) {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		}
		s.writeError(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", err))
		return
	}
	ctx = scope.WithBrand(ctx, v.Key.BrandID)
	ctx = context.WithValue(ctx, apiKeyKey{}, v)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// member resolves the tenant of a /v1/tenants/{id} route after authenticate
// and before the handler. The tenant must be in the caller's brand and not
// closed, and a write needs it active. A token caller needs an active
// membership of its identity; an API key must belong to the tenant. Anything
// else is 404 tenant_not_found, so a tenant's existence is never confirmed
// to outsiders. The handler then calls scope.Require for its permission.
func (s *server) member(next http.HandlerFunc) http.Handler {
	return s.memberOf(next, false)
}

// memberReducing is member for a write that only reduces what a tenant
// holds: settling, releasing or extending a reservation, and releasing a
// quota lease. A suspended tenant may make it, so its running work can
// finish and give back what it holds.
func (s *server) memberReducing(next http.HandlerFunc) http.Handler {
	return s.memberOf(next, true)
}

func (s *server) memberOf(next http.HandlerFunc, reducing bool) http.Handler {
	return s.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, sc, err := s.resolveTenant(r, reducing)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		ctx := scope.WithContext(r.Context(), sc)
		ctx = scope.WithPermissions(ctx, caller.Permissions)
		ctx = context.WithValue(ctx, callerKey{}, caller)
		next(w, r.WithContext(ctx))
	}))
}

// callerHolder names who owns the holds and leases a tenant caller makes:
// the API key, or the membership.
func callerHolder(c tenant.Caller) string {
	if !c.APIKeyID.IsNil() {
		return "apikey:" + idgen.Format(idgen.APIKey, c.APIKeyID)
	}
	return "membership:" + c.Membership.PublicID()
}

func (s *server) resolveTenant(r *http.Request, reducing bool) (tenant.Caller, scope.Scope, error) {
	ctx := r.Context()
	notFound := tenant.ErrNotFound
	brandID, err := scope.BrandFromContext(ctx)
	if err != nil {
		return tenant.Caller{}, scope.Scope{}, notFound
	}
	tenantID, err := idgen.ParseAs(idgen.Tenant, r.PathValue("id"))
	if err != nil {
		return tenant.Caller{}, scope.Scope{}, notFound
	}
	sc := scope.Scope{Brand: brandID, Tenant: tenantID}
	write := r.Method != http.MethodGet && r.Method != http.MethodHead && !reducing
	usable := func(status tenant.Status) bool {
		return status == tenant.StatusActive || (status == tenant.StatusSuspended && !write)
	}
	if s.db == nil {
		return tenant.Caller{}, sc, zserrors.New(zserrors.CodeAuthenticationUnavailable, "")
	}
	unavailable := func(err error) error {
		if zserrors.IsCode(err, zserrors.CodeTenantNotFound) || zserrors.IsCode(err, zserrors.CodeNotFound) {
			return notFound
		}
		return zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", err)
	}

	if key, ok := callerAPIKey(ctx); ok {
		if key.Scope() != sc || !usable(key.TenantStatus) {
			return tenant.Caller{}, sc, notFound
		}
		return tenant.Caller{
			Membership: key.Membership, Permissions: key.Permissions,
			Actor:         events.Actor{Type: events.ActorAPIKey, ID: key.Key.PublicID()},
			CorrelationID: RequestID(ctx), APIKeyID: key.Key.ID,
		}, sc, nil
	}
	p, err := identity.PrincipalFromContext(ctx)
	if err != nil {
		return tenant.Caller{}, sc, err
	}
	ident, ok := callerIdentity(ctx)
	if !ok || p.Kind != identity.KindUser {
		return tenant.Caller{}, sc, notFound
	}
	t, err := tenant.Get(ctx, s.db, sc)
	if err != nil {
		return tenant.Caller{}, sc, unavailable(err)
	}
	if !usable(t.Status) {
		return tenant.Caller{}, sc, notFound
	}
	m, err := tenant.MembershipOf(ctx, s.db, sc, ident.ID)
	if err != nil {
		return tenant.Caller{}, sc, unavailable(err)
	}
	if m.Status != tenant.MembershipActive {
		return tenant.Caller{}, sc, notFound
	}
	perms, m, err := tenant.Effective(ctx, s.db, sc, m.ID)
	if err != nil {
		return tenant.Caller{}, sc, unavailable(err)
	}
	return tenant.Caller{
		Membership: m, Permissions: perms,
		RecentMFA:     identity.RequireRecentMFA(p, identity.RecentMFAWindow, s.now()) == nil,
		Actor:         events.Actor{Type: events.ActorUser, ID: ident.PublicID()},
		CorrelationID: RequestID(ctx),
	}, sc, nil
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
// Keycloak roles are never consulted. When the brand's mfa policy is
// required or required_for_staff, a staff token with a grant but without
// evidence of a second factor is 403 mfa_required, however recent it is.
// Keycloak asks staff for the second factor too, and this check holds even
// when a login path skipped it.
func RequireStaff(ctx context.Context, db identity.Querier, want identity.StaffScope) (identity.Principal, error) {
	p, err := identity.PrincipalFromContext(ctx)
	if err != nil {
		if _, ok := callerAPIKey(ctx); ok {
			return identity.Principal{}, zserrors.New(zserrors.CodeNotFound, "")
		}
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
	if err := identity.RequireStaffMFA(p); err != nil {
		return identity.Principal{}, err
	}
	return p, nil
}

func (s *server) requireStaff(ctx context.Context, want identity.StaffScope) (identity.Principal, error) {
	if s.db == nil {
		return RequireStaff(ctx, nil, want)
	}
	return RequireStaff(ctx, s.db, want)
}
