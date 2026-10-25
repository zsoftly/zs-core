package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/pkg/billing"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// meteringService returns the metering service. Recording needs only the
// database; reading Kill Bill's usage also needs billing.
func (s *server) meteringService() *metering.Service {
	if s.metering != nil {
		return s.metering
	}
	return &metering.Service{Now: s.now}
}

// tenantUsage is the common start of a usage route.
func (s *server) tenantUsage(w http.ResponseWriter, r *http.Request, permission string) (scope.Scope, tenant.Caller, bool) {
	ctx := r.Context()
	if err := scope.Require(ctx, permission); err != nil {
		s.writeError(w, r, err)
		return scope.Scope{}, tenant.Caller{}, false
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return scope.Scope{}, tenant.Caller{}, false
	}
	return sc, caller, true
}

func usageEvent(in gen.UsageEventCreate) metering.Event {
	ev := metering.Event{Meter: in.Meter, EventID: in.EventId, QuantityMicro: in.QuantityMicro, ResourceRef: deref(in.ResourceRef)}
	if in.OccurredAt != nil {
		ev.OccurredAt = *in.OccurredAt
	}
	if in.Dimensions != nil {
		ev.Dimensions = map[string]string(*in.Dimensions)
	}
	if in.Subscription != nil {
		ev.Subscription = *in.Subscription
	}
	return ev
}

func (s *server) recordUsage(w http.ResponseWriter, r *http.Request) {
	sc, caller, ok := s.tenantUsage(w, r, "usage:write")
	if !ok {
		return
	}
	var body gen.UsageEventCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	var (
		u        metering.Usage
		replayed bool
	)
	call := metering.Call{Actor: caller.Actor, CorrelationID: RequestID(r.Context())}
	err := s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		u, replayed, err = s.meteringService().RecordUsage(r.Context(), tx, sc, call, usageEvent(body))
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	s.writeAs(w, r, status, u, &gen.UsageEvent{})
}

func (s *server) recordUsageBatch(w http.ResponseWriter, r *http.Request) {
	sc, caller, ok := s.tenantUsage(w, r, "usage:write")
	if !ok {
		return
	}
	var body gen.UsageBatch
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	batch := make([]metering.Event, 0, len(body.Events))
	for _, ev := range body.Events {
		batch = append(batch, usageEvent(ev))
	}
	var (
		list    []metering.Usage
		replays []bool
	)
	call := metering.Call{Actor: caller.Actor, CorrelationID: RequestID(r.Context())}
	err := s.inTx(r.Context(), func(tx pgx.Tx) error {
		var err error
		list, replays, err = s.meteringService().RecordUsageBatch(r.Context(), tx, sc, call, batch)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]map[string]any, len(list))
	for i := range list {
		items[i] = map[string]any{"usage": list[i], "replayed": replays[i]}
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": items}, &gen.UsageBatchResult{})
}

// timeParam reads an RFC 3339 query parameter, zero when absent.
func timeParam(q url.Values, name string) (time.Time, error) {
	v := q.Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, zserrors.New(zserrors.CodeValidation, "The "+name+" must be an RFC 3339 time.")
	}
	return t, nil
}

// periodParams reads from and to as days, YYYY-MM-DD. Without them the
// period is the current month in UTC.
func periodParams(q url.Values, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	for name, target := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			d, err := time.Parse(time.DateOnly, v)
			if err != nil {
				return time.Time{}, time.Time{}, zserrors.New(zserrors.CodeValidation, "The "+name+" must be a day, YYYY-MM-DD.")
			}
			*target = d
		}
	}
	if !to.After(from) || to.Sub(from) > metering.MaxSummaryDays*24*time.Hour {
		return time.Time{}, time.Time{}, zserrors.New(zserrors.CodeValidation, "The period must be 1 to 366 days, with to after from.")
	}
	return from, to, nil
}

func repeated(q url.Values) error {
	for key, values := range q {
		if len(values) > 1 {
			return zserrors.New(zserrors.CodeValidation, "Query parameter "+strconv.Quote(key)+" is repeated.")
		}
	}
	return nil
}

func (s *server) listUsageEvents(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.tenantUsage(w, r, "usage:read")
	if !ok {
		return
	}
	q := r.URL.Query()
	page, err := pageParams(q)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var f metering.Filter
	f.Meter = q.Get("meter")
	if f.From, err = timeParam(q, "from"); err == nil {
		f.To, err = timeParam(q, "to")
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, next, err := metering.ListUsageEvents(r.Context(), s.db, sc, f, page.Limit, page.Cursor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": list, "next_cursor": nextCursor(next)}, &gen.UsageEventPage{})
}

func (s *server) getUsageSummary(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.tenantUsage(w, r, "usage:read")
	if !ok {
		return
	}
	q := r.URL.Query()
	if err := repeated(q); err != nil {
		s.writeError(w, r, err)
		return
	}
	from, to, err := periodParams(q, s.now())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sum, err := metering.GetUsageSummary(r.Context(), s.db, sc, q.Get("meter"), from, to)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, sum, &gen.UsageSummary{})
}

func (s *server) getBilledUsage(w http.ResponseWriter, r *http.Request) {
	sc, _, ok := s.tenantUsage(w, r, "usage:read")
	if !ok {
		return
	}
	q := r.URL.Query()
	if err := repeated(q); err != nil {
		s.writeError(w, r, err)
		return
	}
	from, to, err := periodParams(q, s.now())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.metering == nil || !s.metering.Billing.Ready() {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotReady, "Billed usage needs the Kill Bill admin settings."))
		return
	}
	list, err := s.metering.BilledUsage(r.Context(), sc, q.Get("subscription"), from, to)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if list == nil {
		list = []billing.BilledUsage{}
	}
	s.writeAs(w, r, http.StatusOK, map[string]any{"data": list}, &gen.BilledUsageList{})
}

// staffRollup is the start of a staff route on a tenant's usage rollup:
// the support grant, the tenant and a staff call with the reason.
func (s *server) staffRollup(w http.ResponseWriter, r *http.Request, reason string) (scope.Scope, metering.StaffCall, bool) {
	ctx := r.Context()
	p, err := s.requireStaff(ctx, identity.StaffSupport)
	if err != nil {
		s.writeError(w, r, err)
		return scope.Scope{}, metering.StaffCall{}, false
	}
	tenantID, err := pathID(r, "id", idgen.Tenant, tenant.ErrNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return scope.Scope{}, metering.StaffCall{}, false
	}
	if err := checkReasonLength(reason); err != nil {
		s.writeError(w, r, err)
		return scope.Scope{}, metering.StaffCall{}, false
	}
	actorID := "sub:" + p.Subject
	if ident, ok := callerIdentity(ctx); ok {
		actorID = ident.PublicID()
	}
	call := metering.StaffCall{Call: metering.Call{Actor: events.Actor{Type: events.ActorStaff, ID: actorID}, CorrelationID: RequestID(ctx)}, Reason: reason}
	return scope.Scope{Brand: p.Brand.ID, Tenant: tenantID}, call, true
}

func (s *server) reassignUsageRollup(w http.ResponseWriter, r *http.Request) {
	var body gen.UsageRollupReassign
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, call, ok := s.staffRollup(w, r, body.Reason)
	if !ok {
		return
	}
	ro, err := s.meteringService().ReassignRollup(r.Context(), sc, call, r.PathValue("rollup"), body.Subscription)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, ro, &gen.UsageRollup{})
}

func (s *server) discardUsageRollup(w http.ResponseWriter, r *http.Request) {
	var body gen.UsageRollupDiscard
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, call, ok := s.staffRollup(w, r, body.Reason)
	if !ok {
		return
	}
	ro, err := s.meteringService().DiscardRollup(r.Context(), sc, call, r.PathValue("rollup"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeAs(w, r, http.StatusOK, ro, &gen.UsageRollup{})
}

func (s *server) resolveUsageRollup(w http.ResponseWriter, r *http.Request) {
	var body gen.UsageRollupResolve
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, call, ok := s.staffRollup(w, r, body.Reason)
	if !ok {
		return
	}
	res, _, err := s.meteringService().ResolveUnconfirmedRollup(r.Context(), sc, call, r.PathValue("rollup"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, gen.UsageRollupResolution{
		RollupId: res.RollupID, Decision: gen.UsageRollupResolutionDecision(res.Decision), InvoiceId: res.InvoiceID, ResolvedBy: res.ResolvedBy, ResolvedAt: res.ResolvedAt,
	})
}
