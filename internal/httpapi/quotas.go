package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/quota"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// errFeature is the answer to a malformed feature in a path.
var errFeature = zserrors.New(zserrors.CodeValidation, "The feature must be 1 to 100 letters, digits, dots, colons, dashes or underscores.")

func (s *server) checkEntitlement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "tenant:read"); err != nil {
		s.writeError(w, r, err)
		return
	}
	feature := r.PathValue("feature")
	if !billing.ValidFeature(feature) {
		s.writeError(w, r, errFeature)
		return
	}
	sc, _ := scope.FromContext(ctx)
	// The check fails closed: without the billing settings it answers
	// allowed false, never an error.
	var e quota.Entitlement
	switch {
	case s.quota != nil:
		e = s.quota.CheckEntitlement(ctx, sc, feature)
	case s.billing.Ready():
		e = quota.Entitlement{Entitlement: s.billing.CheckEntitlement(ctx, sc, feature)}
	default:
		e = quota.Entitlement{Entitlement: billing.Entitlement{Reason: billing.ReasonBillingUnavailable}}
	}
	s.writeAs(w, r, http.StatusOK, e, &gen.Entitlement{})
}

// tenantQuota is the common start of a consume or release route.
func (s *server) tenantQuota(w http.ResponseWriter, r *http.Request) (*quota.Service, scope.Scope, quota.Call, string, bool) {
	ctx := r.Context()
	if err := scope.Require(ctx, "quota:consume"); err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, quota.Call{}, "", false
	}
	feature := r.PathValue("feature")
	if !billing.ValidFeature(feature) {
		s.writeError(w, r, errFeature)
		return nil, scope.Scope{}, quota.Call{}, "", false
	}
	if s.quota == nil {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotReady, "Quotas need the Kill Bill admin settings."))
		return nil, scope.Scope{}, quota.Call{}, "", false
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return nil, scope.Scope{}, quota.Call{}, "", false
	}
	return s.quota, sc, quota.Call{Actor: caller.Actor, CorrelationID: RequestID(ctx), Holder: callerHolder(caller)}, feature, true
}

func (s *server) consumeEntitlement(w http.ResponseWriter, r *http.Request) {
	svc, sc, call, feature, ok := s.tenantQuota(w, r)
	if !ok {
		return
	}
	var body gen.QuotaConsume
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	n := int64(1)
	if body.Amount != nil {
		n = *body.Amount
	}
	var e quota.Entitlement
	err := s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		e, err = svc.ConsumeFor(r.Context(), tx, sc, call, feature, body.Ref, n, leaseOf(body.LeaseSeconds))
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, e, &gen.Entitlement{})
}

func (s *server) releaseEntitlement(w http.ResponseWriter, r *http.Request) {
	svc, sc, call, feature, ok := s.tenantQuota(w, r)
	if !ok {
		return
	}
	var body gen.QuotaRelease
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.released(w, r, svc, sc, call, feature, body.Ref)
}

// released releases a ref's lease and answers what it returned and the
// entitlement after.
func (s *server) released(w http.ResponseWriter, r *http.Request, svc *quota.Service, sc scope.Scope, call quota.Call, feature, ref string) {
	var released int64
	err := s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		released, err = svc.Release(r.Context(), tx, sc, call, feature, ref)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	e := svc.CheckEntitlement(r.Context(), sc, feature)
	s.writeAs(w, r, http.StatusOK, map[string]any{"released": released, "entitlement": e}, &gen.QuotaReleased{})
}

func (s *server) extendEntitlement(w http.ResponseWriter, r *http.Request) {
	svc, sc, call, feature, ok := s.tenantQuota(w, r)
	if !ok {
		return
	}
	var body gen.QuotaExtend
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	var l quota.Lease
	err := s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		l, err = svc.Extend(r.Context(), tx, sc, call, feature, body.Ref, time.Duration(body.LeaseSeconds)*time.Second)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, l, &gen.QuotaLease{})
}

// staffReleaseEntitlement releases another holder's lease, for staff with
// the support grant and a reason.
func (s *server) staffReleaseEntitlement(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.requireStaff(ctx, identity.StaffSupport)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	tenantID, err := pathID(r, "id", idgen.Tenant, tenant.ErrNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	feature := r.PathValue("feature")
	if !billing.ValidFeature(feature) {
		s.writeError(w, r, errFeature)
		return
	}
	if s.quota == nil {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotReady, "Quotas need the Kill Bill admin settings."))
		return
	}
	var body gen.StaffQuotaRelease
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkReasonLength(body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	actorID := "sub:" + p.Subject
	if ident, ok := callerIdentity(ctx); ok {
		actorID = ident.PublicID()
	}
	call := quota.Call{Actor: events.Actor{Type: events.ActorStaff, ID: actorID}, Reason: strings.TrimSpace(body.Reason),
		CorrelationID: RequestID(ctx), Staff: true}
	s.released(w, r, s.quota, scope.Scope{Brand: p.Brand.ID, Tenant: tenantID}, call, feature, body.Ref)
}

// staffBrand is the start of a brand route for staff with want: the
// caller's brand from the path, and the staff actor.
func (s *server) staffBrand(w http.ResponseWriter, r *http.Request, want identity.StaffScope) (idgen.UUID, events.Actor, bool) {
	ctx := r.Context()
	p, err := s.requireStaff(ctx, want)
	if err != nil {
		s.writeError(w, r, err)
		return idgen.Nil, events.Actor{}, false
	}
	id, err := s.brandFromPath(r, p)
	if err != nil {
		s.writeError(w, r, err)
		return idgen.Nil, events.Actor{}, false
	}
	actorID := "sub:" + p.Subject
	if ident, ok := callerIdentity(ctx); ok {
		actorID = ident.PublicID()
	}
	return id, events.Actor{Type: events.ActorStaff, ID: actorID}, true
}

func meterBody(m brand.MeterRecord) map[string]any {
	return map[string]any{"id": m.PublicID(), "slug": m.Slug, "unit": m.Unit, "killbill_unit": m.KillBillUnit,
		"dimensions": m.Dimensions, "status": m.Status, "created_at": m.CreatedAt.UTC(), "updated_at": m.UpdatedAt.UTC()}
}

func quotaBody(q brand.QuotaRecord) map[string]any {
	out := map[string]any{"id": q.PublicID(), "feature": q.Feature, "plan": q.Plan, "limit": q.Limit, "window": q.Window,
		"created_at": q.CreatedAt.UTC(), "updated_at": q.UpdatedAt.UTC()}
	if q.Meter != "" {
		out["meter"] = q.Meter
	}
	return out
}

func (s *server) writeMeters(w http.ResponseWriter, r *http.Request, list []brand.MeterRecord) {
	data := make([]map[string]any, 0, len(list))
	for _, m := range list {
		data = append(data, meterBody(m))
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": data}, &gen.MeterList{})
}

func (s *server) writeQuotas(w http.ResponseWriter, r *http.Request, list []brand.QuotaRecord) {
	data := make([]map[string]any, 0, len(list))
	for _, q := range list {
		data = append(data, quotaBody(q))
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": data}, &gen.QuotaList{})
}

func (s *server) listMeters(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.staffBrand(w, r, identity.StaffOperator)
	if !ok {
		return
	}
	list, err := brand.ListMeters(r.Context(), s.db, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeMeters(w, r, list)
}

func (s *server) listQuotas(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.staffBrand(w, r, identity.StaffOperator)
	if !ok {
		return
	}
	list, err := brand.ListQuotas(r.Context(), s.db, id, "")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeQuotas(w, r, list)
}

// catalogueFacts reads the usage units and plans of the brand's catalogue
// in force from Kill Bill.
func (s *server) catalogueFacts(ctx context.Context, brandID idgen.UUID) (brand.CatalogueFacts, error) {
	svc, err := s.billingService()
	if err != nil {
		return brand.CatalogueFacts{}, err
	}
	c, err := svc.Catalogue(ctx, brandID)
	if err != nil {
		return brand.CatalogueFacts{}, err
	}
	facts := brand.CatalogueFacts{Units: c.Units}
	for _, p := range c.Products {
		for _, pl := range p.Plans {
			facts.Plans = append(facts.Plans, pl.Name)
		}
	}
	return facts, nil
}

func (s *server) replaceMeters(w http.ResponseWriter, r *http.Request) {
	id, actor, ok := s.staffBrand(w, r, identity.StaffOperator)
	if !ok {
		return
	}
	var body gen.MeterReplace
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	reason := deref(body.Reason)
	if err := checkReasonLength(reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	facts, err := s.catalogueFacts(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	want := make([]brand.Meter, 0, len(body.Meters))
	for _, m := range body.Meters {
		mt := brand.Meter{Slug: m.Slug, Unit: string(m.Unit), KillBillUnit: m.KillbillUnit}
		if m.Dimensions != nil {
			mt.Dimensions = *m.Dimensions
		}
		want = append(want, mt)
	}
	var list []brand.MeterRecord
	err = s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		list, _, err = brand.ReplaceMeters(r.Context(), tx, id, want, facts, actor, strings.TrimSpace(reason), RequestID(r.Context()))
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeMeters(w, r, list)
}

func (s *server) replaceQuotas(w http.ResponseWriter, r *http.Request) {
	id, actor, ok := s.staffBrand(w, r, identity.StaffOperator)
	if !ok {
		return
	}
	var body gen.QuotaReplace
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	reason := deref(body.Reason)
	if err := checkReasonLength(reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	facts, err := s.catalogueFacts(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	want := make([]brand.Entitlement, 0, len(body.Quotas))
	for _, q := range body.Quotas {
		want = append(want, brand.Entitlement{Feature: q.Feature, Plan: q.Plan, Limit: q.Limit, Window: string(q.Window), Meter: deref(q.Meter)})
	}
	var list []brand.QuotaRecord
	err = s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		list, _, err = brand.ReplaceQuotas(r.Context(), tx, id, want, facts, actor, strings.TrimSpace(reason), RequestID(r.Context()))
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeQuotas(w, r, list)
}

func (s *server) setQuotaOverride(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.requireStaff(ctx, identity.StaffSupport)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	tenantID, err := pathID(r, "id", idgen.Tenant, tenant.ErrNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.quota == nil {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotReady, "Quotas need the Kill Bill admin settings."))
		return
	}
	var body gen.QuotaOverrideCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkReasonLength(body.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	actorID := "sub:" + p.Subject
	if ident, ok := callerIdentity(ctx); ok {
		actorID = ident.PublicID()
	}
	call := quota.Call{Actor: events.Actor{Type: events.ActorStaff, ID: actorID}, Reason: body.Reason, CorrelationID: RequestID(ctx)}
	sc := scope.Scope{Brand: p.Brand.ID, Tenant: tenantID}
	var o quota.Override
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		req := quota.OverrideRequest{Feature: body.Feature, Limit: body.Limit, Meter: deref(body.Meter), ExpiresAt: body.ExpiresAt}
		if body.Window != nil {
			req.Window = string(*body.Window)
		}
		o, err = s.quota.SetOverride(ctx, tx, sc, call, req)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := map[string]any{"id": o.ID, "tenant_id": sc.TenantID(), "feature": o.Feature, "limit": o.Limit,
		"reason": o.Reason, "expires_at": o.ExpiresAt, "created_at": o.CreatedAt}
	if o.Window != "" {
		out["window"] = o.Window
	}
	if o.Meter != "" {
		out["meter"] = o.Meter
	}
	s.writeAs(w, r, http.StatusCreated, out, &gen.QuotaOverride{})
}
