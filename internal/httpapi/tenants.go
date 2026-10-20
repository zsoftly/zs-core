package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	"github.com/zsoftly/zs-core/pkg/apikey"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/identity"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// decodeBody reads a JSON request body into out. Unknown fields, trailing
// data and a content type other than application/json are refused.
func decodeBody(r *http.Request, out any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return zserrors.New(zserrors.CodeUnsupportedMediaType, "Send the body as application/json.")
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return zserrors.Wrap(zserrors.CodeBadRequest, "", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return zserrors.New(zserrors.CodeValidation, "The request body is not valid for this route.")
	}
	if dec.More() {
		return zserrors.New(zserrors.CodeValidation, "The request body holds more than one JSON value.")
	}
	return nil
}

// inTx runs fn in a transaction and commits it.
func (s *server) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if s.db == nil {
		return zserrors.New(zserrors.CodeDatabaseUnavailable, "")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return nil
}

// mirror queues a Keycloak Organization update for the tenant in tx, after a
// change to the tenant or its members. The event commits with the change.
// The mirror relays of serve apply it with bounded concurrency and the
// sweeper corrects what they miss. Without the engines nothing runs them, so
// nothing is queued.
func (s *server) mirror(ctx context.Context, tx pgx.Tx, sc scope.Scope, actor events.Actor) error {
	if !s.tenants.Ready() {
		return nil
	}
	return tenant.EnqueueMirror(ctx, tx, sc, actor, RequestID(ctx))
}

func pageParams(q url.Values) (tenant.Page, error) {
	for key, values := range q {
		if len(values) > 1 {
			return tenant.Page{}, zserrors.New(zserrors.CodeValidation, "Query parameter "+strconv.Quote(key)+" is repeated.")
		}
	}
	var p tenant.Page
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > tenant.MaxLimit {
			return p, zserrors.New(zserrors.CodeInvalidLimit, "The limit must be an integer from 1 to "+strconv.Itoa(tenant.MaxLimit)+".")
		}
		p.Limit = n
	}
	p.Cursor = q.Get("cursor")
	return p, nil
}

func nextCursor(c string) *string {
	if c == "" {
		return nil
	}
	return &c
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func tenantBody(t tenant.Tenant) gen.Tenant {
	return gen.Tenant{
		Id: t.PublicID(), BrandId: idgen.Format(idgen.Brand, t.BrandID), Kind: gen.TenantKind(t.Kind),
		Slug: t.Slug, Name: t.Name, Status: gen.TenantStatus(t.Status), Activation: gen.TenantActivation(t.Activation),
		BillingCountry: optional(t.BillingCountry), CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(),
	}
}

func memberBody(m tenant.Membership) gen.Member {
	return gen.Member{
		Id: m.PublicID(), TenantId: idgen.Format(idgen.Tenant, m.TenantID), IdentityId: idgen.Format(idgen.Identity, m.IdentityID),
		Email: optional(m.Email), DisplayName: optional(m.DisplayName), Role: gen.Role(m.Role), Status: gen.MemberStatus(m.Status),
		CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC(),
	}
}

func teamBody(t tenant.Team) gen.Team {
	ids := make([]string, len(t.MembershipIDs))
	for i, id := range t.MembershipIDs {
		ids[i] = idgen.Format(idgen.Membership, id)
	}
	return gen.Team{
		Id: t.PublicID(), Name: t.Name, System: t.System, Locked: t.Locked, Permissions: t.Permissions,
		MembershipIds: ids, CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(),
	}
}

func invitationBody(i tenant.Invitation) gen.Invitation {
	return gen.Invitation{
		Id: i.PublicID(), TenantId: idgen.Format(idgen.Tenant, i.TenantID), Email: i.Email, Role: gen.Role(i.Role),
		Status: gen.InvitationStatus(i.Status), ExpiresAt: i.ExpiresAt.UTC(), AcceptedAt: utc(i.AcceptedAt),
		RevokedAt: utc(i.RevokedAt), CreatedAt: i.CreatedAt.UTC(),
	}
}

func apiKeyBody(k apikey.Key) gen.ApiKey {
	return gen.ApiKey{
		Id: k.PublicID(), TenantId: idgen.Format(idgen.Tenant, k.TenantID), Name: k.Name, Prefix: k.Prefix,
		Permissions: k.Permissions, CreatedByMembershipId: idgen.Format(idgen.Membership, k.CreatedByMembershipID),
		ExpiresAt: k.ExpiresAt.UTC(), LastUsedAt: utc(k.LastUsedAt), RevokedAt: utc(k.RevokedAt), CreatedAt: k.CreatedAt.UTC(),
	}
}

func activationBody(a tenant.Activation) gen.ActivationDecision {
	out := gen.ActivationDecision{
		State: gen.ActivationDecisionState(a.State), Reason: a.Reason, RequestId: a.RequestID,
		Actor: gen.Actor{Type: gen.ActorType(a.Actor.Type)}, DecidedAt: a.DecidedAt.UTC(),
	}
	if a.Actor.ID != "" {
		id := a.Actor.ID
		out.Actor.Id = &id
	}
	return out
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func pathID(r *http.Request, name string, prefix idgen.Prefix, notFound error) (idgen.UUID, error) {
	id, err := idgen.ParseAs(prefix, r.PathValue(name))
	if err != nil {
		return idgen.Nil, notFound
	}
	return id, nil
}

// creator returns the person who creates a tenant: a customer token with a
// verified email and its recorded identity.
func (s *server) creator(ctx context.Context) (identity.Principal, identity.Identity, error) {
	p, err := userPrincipal(ctx)
	if err != nil {
		return identity.Principal{}, identity.Identity{}, err
	}
	if err := identity.RequireVerifiedEmail(p); err != nil {
		return identity.Principal{}, identity.Identity{}, err
	}
	ident, ok := callerIdentity(ctx)
	if !ok {
		return identity.Principal{}, identity.Identity{}, zserrors.New(zserrors.CodeEmailNotVerified, "")
	}
	if !s.tenants.Ready() {
		return identity.Principal{}, identity.Identity{}, zserrors.New(zserrors.CodeNotReady, "Tenant creation needs the Keycloak and Kill Bill admin settings.")
	}
	return p, ident, nil
}

func (s *server) tenantDB() (tenant.DB, error) {
	if s.db == nil {
		return nil, zserrors.New(zserrors.CodeDatabaseUnavailable, "")
	}
	return s.db, nil
}

func (s *server) ensurePersonalTenant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, ident, err := s.creator(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	db, err := s.tenantDB()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	t, created, err := tenant.EnsurePersonal(ctx, db, s.tenants, tenant.CreateRequest{
		Brand: p.Brand.ID, Creator: ident.ID, Name: ident.DisplayName,
		Actor: events.Actor{Type: events.ActorUser, ID: ident.PublicID()}, CorrelationID: RequestID(ctx),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeJSON(w, r, status, tenantBody(t))
}

func (s *server) createTenant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, ident, err := s.creator(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.TenantCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	db, err := s.tenantDB()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	req := tenant.CreateRequest{
		Brand: p.Brand.ID, Creator: ident.ID, Name: body.Name, IdempotencyKey: body.IdempotencyKey,
		Actor: events.Actor{Type: events.ActorUser, ID: ident.PublicID()}, CorrelationID: RequestID(ctx),
	}
	if body.BillingCountry != nil {
		req.BillingCountry = *body.BillingCountry
	}
	t, err := tenant.CreateOrganization(ctx, db, s.tenants, req)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, tenantBody(t))
}

func (s *server) getTenant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "tenant:read"); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	t, err := tenant.Get(ctx, s.db, sc)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, tenantBody(t))
}

func (s *server) updateTenant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "tenant:update"); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.TenantUpdate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var t tenant.Tenant
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = tenant.Update(ctx, tx, sc, caller, tenant.Change{Name: body.Name, BillingCountry: body.BillingCountry})
		if err != nil {
			return err
		}
		return s.mirror(ctx, tx, sc, caller.Actor)
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, tenantBody(t))
}

func (s *server) listMembers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "member:read"); err != nil {
		s.writeError(w, r, err)
		return
	}
	page, err := pageParams(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	list, next, err := tenant.ListMembers(ctx, s.db, sc, page)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body := gen.MemberPage{Data: make([]gen.Member, 0, len(list)), NextCursor: nextCursor(next)}
	for _, m := range list {
		body.Data = append(body.Data, memberBody(m))
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

func (s *server) updateMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "member:update"); err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := pathID(r, "mem", idgen.Membership, tenant.ErrMemberNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.MemberUpdate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	change := tenant.MemberChange{}
	if body.Role != nil {
		role := tenant.Role(*body.Role)
		change.Role = &role
	}
	if body.Status != nil {
		status := tenant.MembershipStatus(*body.Status)
		change.Status = &status
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var m tenant.Membership
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		m, err = tenant.UpdateMember(ctx, tx, sc, caller, id, change)
		if err != nil {
			return err
		}
		return s.mirror(ctx, tx, sc, caller.Actor)
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, memberBody(m))
}

func (s *server) removeMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "member:remove"); err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := pathID(r, "mem", idgen.Membership, tenant.ErrMemberNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tenant.RemoveMember(ctx, tx, sc, caller, id); err != nil {
			return err
		}
		return s.mirror(ctx, tx, sc, caller.Actor)
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) transferOwnership(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, tenant.TransferPermission); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.OwnershipTransfer
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	to, err := idgen.ParseAs(idgen.Membership, body.MembershipId)
	if err != nil {
		s.writeError(w, r, tenant.ErrMemberNotFound)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var from, target tenant.Membership
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		from, target, err = tenant.TransferOwnership(ctx, tx, sc, caller, to)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, gen.OwnershipTransferResult{From: memberBody(from), To: memberBody(target)})
}

func (s *server) listTeams(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "team:read"); err != nil {
		s.writeError(w, r, err)
		return
	}
	page, err := pageParams(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	list, next, err := tenant.ListTeams(ctx, s.db, sc, page)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body := gen.TeamPage{Data: make([]gen.Team, 0, len(list)), NextCursor: nextCursor(next)}
	for _, t := range list {
		body.Data = append(body.Data, teamBody(t))
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

// teamWrite runs a team:manage operation and answers with the team.
func (s *server) teamWrite(w http.ResponseWriter, r *http.Request, status int, op func(context.Context, pgx.Tx, scope.Scope, tenant.Caller) (tenant.Team, error)) {
	ctx := r.Context()
	if err := scope.Require(ctx, "team:manage"); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var t tenant.Team
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		t, err = op(ctx, tx, sc, caller)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	s.writeJSON(w, r, status, teamBody(t))
}

func (s *server) createTeam(w http.ResponseWriter, r *http.Request) {
	var body gen.TeamCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.teamWrite(w, r, http.StatusCreated, func(ctx context.Context, tx pgx.Tx, sc scope.Scope, c tenant.Caller) (tenant.Team, error) {
		return tenant.CreateTeam(ctx, tx, sc, c, body.Name)
	})
}

func (s *server) updateTeam(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "tm", idgen.Team, tenant.ErrTeamNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.TeamUpdate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.teamWrite(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, sc scope.Scope, c tenant.Caller) (tenant.Team, error) {
		return tenant.RenameTeam(ctx, tx, sc, c, id, body.Name)
	})
}

func (s *server) deleteTeam(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "tm", idgen.Team, tenant.ErrTeamNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.teamWrite(w, r, http.StatusNoContent, func(ctx context.Context, tx pgx.Tx, sc scope.Scope, c tenant.Caller) (tenant.Team, error) {
		return tenant.Team{}, tenant.DeleteTeam(ctx, tx, sc, c, id)
	})
}

func (s *server) setTeamMembers(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "tm", idgen.Team, tenant.ErrTeamNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.TeamMembers
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	members := make([]idgen.UUID, 0, len(body.MembershipIds))
	for _, m := range body.MembershipIds {
		u, err := idgen.ParseAs(idgen.Membership, m)
		if err != nil {
			s.writeError(w, r, tenant.ErrMemberNotFound)
			return
		}
		members = append(members, u)
	}
	s.teamWrite(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, sc scope.Scope, c tenant.Caller) (tenant.Team, error) {
		return tenant.SetTeamMembers(ctx, tx, sc, c, id, members)
	})
}

func (s *server) setTeamGrants(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "tm", idgen.Team, tenant.ErrTeamNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.TeamGrants
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.teamWrite(w, r, http.StatusOK, func(ctx context.Context, tx pgx.Tx, sc scope.Scope, c tenant.Caller) (tenant.Team, error) {
		return tenant.SetTeamGrants(ctx, tx, sc, c, id, body.Permissions)
	})
}

func (s *server) listInvitations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "member:invite"); err != nil {
		s.writeError(w, r, err)
		return
	}
	page, err := pageParams(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, next, err := tenant.ListInvitations(ctx, s.db, sc, caller, page)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body := gen.InvitationPage{Data: make([]gen.Invitation, 0, len(list)), NextCursor: nextCursor(next)}
	for _, i := range list {
		body.Data = append(body.Data, invitationBody(i))
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

func (s *server) createInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "member:invite"); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.InvitationCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var (
		inv   tenant.Invitation
		token string
	)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		inv, token, err = tenant.CreateInvitation(ctx, tx, sc, caller, body.Email, tenant.Role(body.Role))
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, gen.InvitationCreated{Invitation: invitationBody(inv), Token: token})
}

func (s *server) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "member:invite"); err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := pathID(r, "inv", idgen.Invitation, tenant.ErrInvitationNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tenant.RevokeInvitation(ctx, tx, sc, caller, id)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := userPrincipal(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := identity.RequireVerifiedEmail(p); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := identity.RequireRecentAuth(p, identity.RecentMFAWindow, s.now()); err != nil {
		s.writeError(w, r, err)
		return
	}
	ident, ok := callerIdentity(ctx)
	if !ok {
		s.writeError(w, r, zserrors.New(zserrors.CodeEmailNotVerified, ""))
		return
	}
	var body gen.InvitationAccept
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	var (
		m       tenant.Membership
		t       tenant.Tenant
		refused error
	)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		inv, mem, err := tenant.AcceptInvitation(ctx, tx, tenant.Acceptor{
			Brand: p.Brand.ID, IdentityID: ident.ID, Email: p.Email, EmailVerified: p.EmailVerified,
			RecentAuth: true, CorrelationID: RequestID(ctx),
		}, body.Token)
		var recorded *tenant.RefusalRecorded
		if errors.As(err, &recorded) {
			// Commit the revocation and its audit event, then refuse.
			refused = err
			return nil
		}
		if err != nil {
			return err
		}
		m = mem
		t, err = tenant.Get(ctx, tx, scope.Scope{Brand: p.Brand.ID, Tenant: inv.TenantID})
		if err != nil {
			return err
		}
		return s.mirror(ctx, tx, t.Scope(), events.Actor{Type: events.ActorUser, ID: ident.PublicID()})
	})
	if err == nil {
		err = refused
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, gen.InvitationAccepted{Membership: memberBody(m), Tenant: tenantBody(t)})
}

func (s *server) getTenantActivation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "tenant:read"); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	state, decisions, err := tenant.ActivationOf(ctx, s.db, sc)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body := gen.ActivationState{State: gen.ActivationStateState(state), Decisions: make([]gen.ActivationDecision, 0, len(decisions))}
	for _, d := range decisions {
		body.Decisions = append(body.Decisions, activationBody(d))
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

// decideTenantActivation is a staff route: an active activation grant in the
// tenant's brand, or 404.
func (s *server) decideTenantActivation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.requireStaff(ctx, identity.StaffActivation)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	tenantID, err := pathID(r, "id", idgen.Tenant, tenant.ErrNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.ActivationDecisionRequest
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	actorID := "sub:" + p.Subject
	if ident, ok := callerIdentity(ctx); ok {
		actorID = ident.PublicID()
	}
	sc := scope.Scope{Brand: p.Brand.ID, Tenant: tenantID}
	var (
		a        tenant.Activation
		replayed bool
	)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		a, replayed, err = tenant.Decide(ctx, tx, sc, tenant.Decision{State: string(body.State), Reason: body.Reason, RequestID: body.RequestId},
			events.Actor{Type: events.ActorStaff, ID: actorID}, RequestID(ctx))
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
	s.writeJSON(w, r, status, activationBody(a))
}

func (s *server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "apikey:read"); err != nil {
		s.writeError(w, r, err)
		return
	}
	page, err := pageParams(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, next, err := apikey.List(ctx, s.db, sc, caller, page)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body := gen.ApiKeyPage{Data: make([]gen.ApiKey, 0, len(list)), NextCursor: nextCursor(next)}
	for _, k := range list {
		body.Data = append(body.Data, apiKeyBody(k))
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

func (s *server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "apikey:create"); err != nil {
		s.writeError(w, r, err)
		return
	}
	var body gen.ApiKeyCreate
	if err := decodeBody(r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	req := apikey.CreateRequest{Name: body.Name, Permissions: body.Permissions}
	if body.ExpiresAt != nil {
		req.ExpiresAt = *body.ExpiresAt
	}
	var created apikey.Created
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = apikey.Create(ctx, tx, sc, caller, req)
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, gen.ApiKeyCreated{ApiKey: apiKeyBody(created.Key), Secret: created.Secret})
}

func (s *server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "apikey:revoke"); err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := pathID(r, "key", idgen.APIKey, apikey.ErrNotFound)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	if len(q["reason"]) > 1 {
		s.writeError(w, r, zserrors.New(zserrors.CodeValidation, "Query parameter \"reason\" is repeated."))
		return
	}
	sc, _ := scope.FromContext(ctx)
	caller, err := tenantCaller(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := apikey.Revoke(ctx, tx, sc, caller, id, q.Get("reason"))
		return err
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listTenantAuditEvents is the member route of the audit trail: the events
// of one tenant, for a caller with audit:read in it.
func (s *server) listTenantAuditEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := scope.Require(ctx, "audit:read"); err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	if q.Has("tenant_id") {
		s.writeError(w, r, zserrors.New(zserrors.CodeValidation, "The tenant is in the path; tenant_id is not a parameter of this route."))
		return
	}
	params, err := parseListAuditEventsParams(q)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sc, _ := scope.FromContext(ctx)
	page, err := audit.Query(ctx, s.db, sc, auditFilter(params))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, auditPageBody(page))
}
