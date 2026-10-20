package httpapi

import (
	"net/http"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// getMe returns the caller's identity and its active memberships. Only a
// customer token with a recorded identity has memberships. An API key gets
// permission_denied.
func (s *server) getMe(w http.ResponseWriter, r *http.Request) {
	if _, ok := callerAPIKey(r.Context()); ok {
		s.writeError(w, r, errTokenOnly)
		return
	}
	p, err := identity.PrincipalFromContext(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body := gen.Me{
		Identity: gen.Identity{
			BrandId:       p.Brand.PublicID(),
			Kind:          gen.IdentityKind(p.Kind),
			Issuer:        p.Issuer,
			Subject:       p.Subject,
			EmailVerified: p.EmailVerified,
			Mfa:           p.MFA,
		},
		Memberships: []gen.Membership{},
	}
	if ident, ok := callerIdentity(r.Context()); ok {
		id := ident.PublicID()
		body.Identity.Id = &id
		if p.Kind == identity.KindUser {
			list, err := tenant.ListForIdentity(r.Context(), s.db, p.Brand.ID, ident.ID)
			if err != nil {
				s.writeError(w, r, err)
				return
			}
			for _, im := range list {
				body.Memberships = append(body.Memberships, gen.Membership{
					Id: im.Membership.PublicID(), Role: gen.Role(im.Membership.Role),
					Status: gen.MembershipStatus(im.Membership.Status), Tenant: tenantBody(im.Tenant),
				})
			}
		}
	}
	if p.Email != "" {
		email := p.Email
		body.Identity.Email = &email
	}
	if p.Name != "" {
		name := p.Name
		body.Identity.DisplayName = &name
	}
	if !p.AuthTime.IsZero() {
		at := p.AuthTime
		body.Identity.AuthenticatedAt = &at
	}
	s.writeJSON(w, r, http.StatusOK, body)
}
