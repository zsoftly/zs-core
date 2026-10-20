package httpapi

import (
	"context"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// brandFromPath returns the brand in the path when it is the caller's own
// brand. Any other ID, well formed or not, is 404.
func (s *server) brandFromPath(r *http.Request, p identity.Principal) (idgen.UUID, error) {
	id, err := idgen.ParseAs(idgen.Brand, r.PathValue("id"))
	if err != nil || id != p.Brand.ID {
		return idgen.Nil, zserrors.New(zserrors.CodeNotFound, "")
	}
	return id, nil
}

func (s *server) getBrand(w http.ResponseWriter, r *http.Request) {
	p, err := s.requireStaff(r.Context(), identity.StaffOperator)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := s.brandFromPath(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rec, err := brand.Get(r.Context(), s.db, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, brandBody(rec))
}

func brandBody(rec brand.Record) gen.Brand {
	b := gen.Brand{
		Id:             rec.PublicID(),
		Slug:           rec.Slug,
		Name:           rec.Name,
		Status:         gen.BrandStatus(rec.Status),
		KeycloakRealm:  rec.KeycloakRealm,
		KillbillApiKey: rec.KillBillAPIKey,
		ManifestDigest: rec.ManifestDigest,
		CreatedAt:      rec.CreatedAt.UTC(),
		UpdatedAt:      rec.UpdatedAt.UTC(),
	}
	if rec.KillBillTenantID != "" {
		t := rec.KillBillTenantID
		b.KillbillTenantId = &t
	}
	if !rec.AppliedAt.IsZero() {
		at := rec.AppliedAt
		b.AppliedAt = &at
	}
	return b
}

// ApplyTimeout bounds one POST /v1/brands/{id}/apply. The route is long
// running: it may change many objects in Keycloak and Kill Bill.
const ApplyTimeout = 10 * time.Minute

// applyContext returns the context an apply runs under: ApplyTimeout long and
// not cancelled when the client disconnects, so an apply that has started
// changing the engines finishes the change it is on and audits it. The audit
// trail is the record of an apply whose client went away.
func applyContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), ApplyTimeout)
}

// extendWriteDeadline lets a long-running handler answer after the server's
// write timeout. It reports false when the writer does not support deadlines.
func extendWriteDeadline(w http.ResponseWriter, d time.Duration) bool {
	return http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d)) == nil
}

// applyGate is the HTTP check of an apply that changes a sensitive field: the
// operator must give a reason and have passed MFA within the last 15 minutes.
func applyGate(p identity.Principal, reason string, now func() time.Time) func([]string) error {
	return func([]string) error {
		if err := identity.RequireRecentMFA(p, identity.RecentMFAWindow, now()); err != nil {
			return zserrors.New(zserrors.CodeRecentMFARequired, "This apply changes authentication or payment settings: sign in again with multi-factor authentication within the last 15 minutes.")
		}
		if strings.TrimSpace(reason) == "" {
			return zserrors.New(zserrors.CodeReasonRequired, "This apply changes authentication or payment settings and requires a reason.")
		}
		return nil
	}
}

// brandFiles opens ZS_BRANDS_DIR/<slug> as the root of an apply's file
// references. It returns nil when there is no such directory, so a manifest
// that references a file is refused.
func (s *server) brandFiles(slug string) (*os.Root, error) {
	if s.brandsDir == "" {
		return nil, nil
	}
	root, err := os.OpenRoot(filepath.Join(s.brandsDir, slug))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return root, nil
}

// applyBrand converges the caller's brand to the manifest in the body.
func (s *server) applyBrand(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.requireStaff(ctx, identity.StaffOperator)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := s.brandFromPath(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && mediaType != "application/yaml") {
		s.writeError(w, r, zserrors.New(zserrors.CodeUnsupportedMediaType, "Send the manifest as application/json or application/yaml."))
		return
	}
	q := r.URL.Query()
	if len(q["reason"]) > 1 {
		s.writeError(w, r, zserrors.New(zserrors.CodeValidation, "Query parameter \"reason\" is repeated."))
		return
	}
	reason := q.Get("reason")
	if len(reason) > 1000 {
		s.writeError(w, r, zserrors.New(zserrors.CodeValidation, "The reason is too long."))
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeBadRequest, "", err))
		return
	}
	m, err := brand.Parse(data)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	deps := s.brandDeps
	deps.DB = s.db
	deps.Files, deps.FilesPrefix = nil, p.Brand.Slug+"/"
	root, err := s.brandFiles(p.Brand.Slug)
	if err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeInternal, "", err))
		return
	}
	if root != nil {
		defer root.Close()
		deps.Files = root.FS()
	}
	actorID := "sub:" + p.Subject
	if ident, ok := callerIdentity(ctx); ok {
		actorID = ident.PublicID()
	}
	extendWriteDeadline(w, ApplyTimeout+time.Minute)
	applyCtx, cancel := applyContext(r)
	defer cancel()
	diff, err := brand.Apply(applyCtx, deps, m, brand.Options{
		Actor:                  events.Actor{Type: events.ActorStaff, ID: actorID},
		Reason:                 reason,
		CorrelationID:          RequestID(ctx),
		CreatedBy:              "staff:" + p.Subject,
		ExpectBrand:            id,
		RequireRealmSSL:        true,
		ConfirmSensitiveChange: applyGate(p, reason, s.now),
	})
	if s.brands != nil && diff.Applied {
		s.brands.Invalidate()
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, diffBody(diff))
}

func diffBody(d brand.Diff) gen.BrandDiff {
	out := gen.BrandDiff{
		Applied:        d.Applied,
		Slug:           d.Slug,
		ManifestDigest: d.ManifestDigest,
		Changes:        make([]gen.BrandChange, 0, len(d.Changes)),
	}
	if d.BrandID != "" {
		id := d.BrandID
		out.BrandId = &id
	}
	if d.ManifestDigestBefore != "" {
		before := d.ManifestDigestBefore
		out.ManifestDigestBefore = &before
	}
	for _, ch := range d.Changes {
		c := gen.BrandChange{
			Target: gen.BrandChangeTarget(ch.Target),
			Name:   ch.Name,
			Action: gen.BrandChangeAction(ch.Action),
			Fields: make([]gen.BrandFieldChange, 0, len(ch.Fields)),
		}
		for _, f := range ch.Fields {
			fc := gen.BrandFieldChange{Field: f.Field}
			if f.Redacted {
				redacted := true
				fc.Redacted = &redacted
			} else {
				fc.Before, fc.After = f.Before, f.After
			}
			c.Fields = append(c.Fields, fc)
		}
		out.Changes = append(out.Changes, c)
	}
	return out
}
