package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// listAuditEvents is a staff operator route until Phase 3b adds the tenant
// member path. With tenant_id it lists that tenant's events inside the
// caller's brand; without it, the brand's own events.
func (s *server) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	p, err := s.requireStaff(r.Context(), identity.StaffOperator)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	params, err := parseListAuditEventsParams(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	filter := audit.Filter{}
	if params.Limit != nil {
		filter.Limit = *params.Limit
	}
	if params.Cursor != nil {
		filter.Cursor = *params.Cursor
	}
	if params.ActorId != nil {
		filter.ActorID = *params.ActorId
	}
	if params.Action != nil {
		filter.Action = *params.Action
	}
	if params.OccurredAfter != nil {
		filter.OccurredAfter = *params.OccurredAfter
	}
	if params.OccurredBefore != nil {
		filter.OccurredBefore = *params.OccurredBefore
	}

	var page audit.Page
	if params.TenantId != nil {
		tenant, perr := idgen.ParseAs(idgen.Tenant, *params.TenantId)
		if perr != nil {
			s.writeError(w, r, scope.ErrInvalid)
			return
		}
		page, err = audit.Query(r.Context(), s.db, scope.Scope{Brand: p.Brand.ID, Tenant: tenant}, filter)
	} else {
		page, err = audit.QueryBrand(r.Context(), s.db, p.Brand.ID, filter)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body := gen.AuditEventPage{Data: make([]gen.AuditEvent, 0, len(page.Events))}
	if page.NextCursor != "" {
		next := page.NextCursor
		body.NextCursor = &next
	}
	for _, e := range page.Events {
		item := gen.AuditEvent{
			Id:            e.ID,
			BrandId:       e.BrandID,
			Actor:         gen.Actor{Type: gen.ActorType(e.Actor.Type)},
			Action:        e.Action,
			Outcome:       gen.AuditOutcome(e.Outcome),
			CorrelationId: e.CorrelationID,
			OccurredAt:    e.OccurredAt,
		}
		if e.Actor.ID != "" {
			id := e.Actor.ID
			item.Actor.Id = &id
		}
		if e.TenantID != "" {
			tenant := e.TenantID
			item.TenantId = &tenant
		}
		if e.Details != nil {
			details := e.Details
			item.Details = &details
		}
		if e.Reason != "" {
			reason := e.Reason
			item.Reason = &reason
		}
		body.Data = append(body.Data, item)
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

// parseListAuditEventsParams reads the query string into the generated
// parameter type. Repeated parameters are rejected rather than silently
// resolved.
func parseListAuditEventsParams(q url.Values) (gen.ListAuditEventsParams, error) {
	var p gen.ListAuditEventsParams
	for key, values := range q {
		if len(values) > 1 {
			return p, zserrors.New(zserrors.CodeValidation, "Query parameter "+strconv.Quote(key)+" is repeated.")
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > audit.MaxLimit {
			return p, zserrors.New(zserrors.CodeInvalidLimit, "The limit must be an integer from 1 to "+strconv.Itoa(audit.MaxLimit)+".")
		}
		p.Limit = &n
	}
	optional := func(name string) *string {
		if v := q.Get(name); v != "" {
			return &v
		}
		return nil
	}
	p.TenantId = optional("tenant_id")
	p.Cursor = optional("cursor")
	p.ActorId = optional("actor_id")
	p.Action = optional("action")
	for name, target := range map[string]**time.Time{
		"occurred_after":  &p.OccurredAfter,
		"occurred_before": &p.OccurredBefore,
	} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				return p, zserrors.New(zserrors.CodeValidation, name+" must be an RFC 3339 timestamp.")
			}
			*target = &t
		}
	}
	return p, nil
}
