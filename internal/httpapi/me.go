package httpapi

import (
	"net/http"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/pkg/identity"
)

// getMe returns the caller's identity. Memberships are added in Phase 3b;
// until then the list is always empty.
func (s *server) getMe(w http.ResponseWriter, r *http.Request) {
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
