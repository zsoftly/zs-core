package tenant

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// MembershipStatus is the state of a membership.
type MembershipStatus string

// Membership statuses. Only an active membership has permissions.
const (
	MembershipActive    MembershipStatus = "active"
	MembershipSuspended MembershipStatus = "suspended"
	MembershipRemoved   MembershipStatus = "removed"
)

// Membership is a row of the memberships table, with the identity's email
// and display name for lists.
type Membership struct {
	ID             idgen.UUID
	BrandID        idgen.UUID
	TenantID       idgen.UUID
	IdentityID     idgen.UUID
	KeycloakUserID string
	Email          string
	DisplayName    string
	Role           Role
	Status         MembershipStatus
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PublicID returns the mem_ ID.
func (m Membership) PublicID() string { return idgen.Format(idgen.Membership, m.ID) }

// ErrMemberNotFound is the answer for a membership that does not exist in
// the tenant or was removed.
var ErrMemberNotFound = zserrors.New(zserrors.CodeNotFound, "The member was not found.")

const membershipColumns = `m.id, m.brand_id, m.tenant_id, m.identity_id, coalesce(i.keycloak_user_id, ''),
       coalesce(i.email, ''), coalesce(i.display_name, ''), m.role, m.status, m.created_at, m.updated_at`

const membershipFrom = ` FROM memberships m JOIN identities i ON i.id = m.identity_id AND i.brand_id = m.brand_id `

func scanMembership(row pgx.Row) (Membership, error) {
	var m Membership
	var role, status string
	err := row.Scan(&m.ID, &m.BrandID, &m.TenantID, &m.IdentityID, &m.KeycloakUserID, &m.Email, &m.DisplayName,
		&role, &status, &m.CreatedAt, &m.UpdatedAt)
	m.Role, m.Status = Role(role), MembershipStatus(status)
	return m, err
}

func oneMembership(ctx context.Context, q Querier, where string, args []any) (Membership, error) {
	m, err := scanMembership(q.QueryRow(ctx, `SELECT `+membershipColumns+membershipFrom+`WHERE m.brand_id = $1 AND m.tenant_id = $2 AND `+where, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, ErrMemberNotFound
	}
	if err != nil {
		return Membership{}, unavailable(err)
	}
	return m, nil
}

// GetMembership returns a membership of the tenant in any status.
func GetMembership(ctx context.Context, q Querier, s scope.Scope, id idgen.UUID) (Membership, error) {
	args, err := s.Bind(id)
	if err != nil {
		return Membership{}, err
	}
	return oneMembership(ctx, q, "m.id = $3", args)
}

// MembershipOf returns the membership of an identity in the tenant: the one
// that is not removed when there is one, otherwise the latest removed one.
// A removed membership is history. Accepting a new invitation creates a new
// membership with a new ID.
func MembershipOf(ctx context.Context, q Querier, s scope.Scope, identityID idgen.UUID) (Membership, error) {
	args, err := s.Bind(identityID)
	if err != nil {
		return Membership{}, err
	}
	return oneMembership(ctx, q, "m.identity_id = $3 ORDER BY (m.status = 'removed'), m.created_at DESC, m.id DESC LIMIT 1", args)
}

// ListMembers returns the memberships of the tenant that are not removed,
// oldest first.
func ListMembers(ctx context.Context, q Querier, s scope.Scope, page Page) ([]Membership, string, error) {
	limit, after, err := page.After()
	if err != nil {
		return nil, "", err
	}
	args, err := s.Bind(after, limit+1)
	if err != nil {
		return nil, "", err
	}
	rows, err := q.Query(ctx, `SELECT `+membershipColumns+membershipFrom+`
WHERE m.brand_id = $1 AND m.tenant_id = $2 AND m.status <> 'removed' AND m.id > $3
ORDER BY m.id LIMIT $4`, args...)
	if err != nil {
		return nil, "", unavailable(err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Membership, error) { return scanMembership(r) })
	if err != nil {
		return nil, "", unavailable(err)
	}
	return pageOf(list, limit, func(m Membership) idgen.UUID { return m.ID })
}

func pageOf[T any](list []T, limit int, id func(T) idgen.UUID) ([]T, string, error) {
	if len(list) <= limit {
		return list, "", nil
	}
	list = list[:limit]
	return list, EncodeCursor(id(list[limit-1])), nil
}

// IdentityMembership is one tenant an identity belongs to, for GET /v1/me.
type IdentityMembership struct {
	Membership Membership
	Tenant     Tenant
}

// ListForIdentity returns the active memberships of an identity in tenants
// of the brand that are not closed, oldest first, up to MaxLimit.
func ListForIdentity(ctx context.Context, q Querier, brand, identityID idgen.UUID) ([]IdentityMembership, error) {
	rows, err := q.Query(ctx, `SELECT `+membershipColumns+`, `+tenantColumnsOf("t.")+membershipFrom+`
JOIN tenants t ON t.brand_id = m.brand_id AND t.id = m.tenant_id
WHERE m.brand_id = $1 AND m.identity_id = $2 AND m.status = 'active' AND t.status <> 'closed'
ORDER BY m.id LIMIT $3`, brand, identityID, MaxLimit)
	if err != nil {
		return nil, unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (IdentityMembership, error) {
		var im IdentityMembership
		var role, status, kind, tstatus string
		err := r.Scan(&im.Membership.ID, &im.Membership.BrandID, &im.Membership.TenantID, &im.Membership.IdentityID,
			&im.Membership.KeycloakUserID, &im.Membership.Email, &im.Membership.DisplayName, &role, &status,
			&im.Membership.CreatedAt, &im.Membership.UpdatedAt,
			&im.Tenant.ID, &im.Tenant.BrandID, &kind, &im.Tenant.Slug, &im.Tenant.Name, &tstatus, &im.Tenant.Activation,
			&im.Tenant.KeycloakOrganizationID, &im.Tenant.KillBillAccountID, &im.Tenant.BillingCountry,
			&im.Tenant.CreatedByIdentityID, &im.Tenant.CreatedAt, &im.Tenant.UpdatedAt)
		im.Membership.Role, im.Membership.Status = Role(role), MembershipStatus(status)
		im.Tenant.Kind, im.Tenant.Status = Kind(kind), Status(tstatus)
		return im, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// MemberChange is a change to a membership. Nil fields are left alone.
type MemberChange struct {
	Role   *Role
	Status *MembershipStatus
}

// UpdateMember changes the role or status of a membership. It needs
// member:update and recent MFA. The caller must hold every permission the
// target's current and new role imply, so an admin cannot touch an owner or
// hand out billing, and only an owner can make another owner. The last
// active owner cannot be demoted or suspended: last_owner. The caller's
// membership is read again under the tenant lock and every check runs on
// what it reads.
//
// Suspending a member revokes every API key it created, with reason
// creator_suspended. Reactivation does not bring them back. A role change or
// a suspension also revokes the member's pending invitations it can no
// longer send.
func UpdateMember(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, id idgen.UUID, change MemberChange) (Membership, error) {
	if err := caller.require("member:update"); err != nil {
		return Membership{}, err
	}
	if err := caller.requireMFA(); err != nil {
		return Membership{}, err
	}
	if change.Role == nil && change.Status == nil {
		return Membership{}, validation("Give a role or a status.")
	}
	if change.Role != nil && !change.Role.Valid() {
		return Membership{}, validation("The role must be owner, admin, billing_admin or member.")
	}
	if change.Status != nil && *change.Status != MembershipActive && *change.Status != MembershipSuspended {
		return Membership{}, validation("The status must be active or suspended. Remove a member with DELETE.")
	}
	_, caller, err := lockForCaller(ctx, tx, s, caller)
	if err != nil {
		return Membership{}, err
	}
	if err := caller.require("member:update"); err != nil {
		return Membership{}, err
	}
	target, err := GetMembership(ctx, tx, s, id)
	if err != nil {
		return Membership{}, err
	}
	if target.Status == MembershipRemoved {
		return Membership{}, ErrMemberNotFound
	}
	newRole, newStatus := target.Role, target.Status
	if change.Role != nil {
		newRole = *change.Role
	}
	if change.Status != nil {
		newStatus = *change.Status
	}
	if newRole == target.Role && newStatus == target.Status {
		return target, nil
	}
	if err := canManage(ctx, tx, s, caller, target.Role, newRole); err != nil {
		return Membership{}, err
	}
	if err := guardLastOwner(ctx, tx, s, target, newRole, newStatus); err != nil {
		return Membership{}, err
	}
	updated, err := setMembership(ctx, tx, s, target, newRole, newStatus)
	if err != nil {
		return Membership{}, err
	}
	details := map[string]any{"membership_id": target.PublicID()}
	if newRole != target.Role {
		details["role"] = map[string]any{"before": string(target.Role), "after": string(newRole)}
	}
	if newStatus != target.Status {
		details["status"] = map[string]any{"before": string(target.Status), "after": string(newStatus)}
	}
	if err := writeAudit(ctx, tx, s, caller, "tenant.member.updated", "", details); err != nil {
		return Membership{}, err
	}
	reason := InvitationRevokedInviterDemoted
	if newStatus == MembershipSuspended {
		reason = InvitationRevokedInviterSuspended
		if err := revokeKeys(ctx, tx, s, caller, target.ID, KeyRevokedCreatorSuspended); err != nil {
			return Membership{}, err
		}
	}
	if err := revokeStaleInvitations(ctx, tx, s, caller, target.ID, reason); err != nil {
		return Membership{}, err
	}
	return updated, nil
}

// RemoveMember sets a membership to removed and takes it out of every team.
// It needs member:remove and recent MFA, and the caller must hold every
// permission the target's role implies. The last active owner cannot be
// removed: last_owner. In the same transaction it revokes every API key the
// member created, with reason creator_removed, and every pending invitation
// the member sent. A removed membership stays removed: a new invitation
// creates a new membership, and the revoked keys never work again.
func RemoveMember(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, id idgen.UUID) (Membership, error) {
	if err := caller.require("member:remove"); err != nil {
		return Membership{}, err
	}
	if err := caller.requireMFA(); err != nil {
		return Membership{}, err
	}
	_, caller, err := lockForCaller(ctx, tx, s, caller)
	if err != nil {
		return Membership{}, err
	}
	if err := caller.require("member:remove"); err != nil {
		return Membership{}, err
	}
	target, err := GetMembership(ctx, tx, s, id)
	if err != nil {
		return Membership{}, err
	}
	if target.Status == MembershipRemoved {
		return Membership{}, ErrMemberNotFound
	}
	if err := canManage(ctx, tx, s, caller, target.Role, target.Role); err != nil {
		return Membership{}, err
	}
	if err := guardLastOwner(ctx, tx, s, target, target.Role, MembershipRemoved); err != nil {
		return Membership{}, err
	}
	removed, err := setMembership(ctx, tx, s, target, target.Role, MembershipRemoved)
	if err != nil {
		return Membership{}, err
	}
	if err := writeAudit(ctx, tx, s, caller, "tenant.member.removed", "", map[string]any{
		"membership_id": target.PublicID(), "role": string(target.Role),
	}); err != nil {
		return Membership{}, err
	}
	if err := revokeKeys(ctx, tx, s, caller, target.ID, KeyRevokedCreatorRemoved); err != nil {
		return Membership{}, err
	}
	if err := revokeStaleInvitations(ctx, tx, s, caller, target.ID, InvitationRevokedInviterRemoved); err != nil {
		return Membership{}, err
	}
	return removed, nil
}

// TransferOwnership makes an active admin the owner and the calling owner an
// admin, in one transaction. It needs the owner role, tenant:transfer and
// recent MFA.
func TransferOwnership(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, to idgen.UUID) (from, target Membership, err error) {
	if err := caller.require(TransferPermission); err != nil {
		return Membership{}, Membership{}, err
	}
	if err := caller.requireMFA(); err != nil {
		return Membership{}, Membership{}, err
	}
	_, caller, err = lockForCaller(ctx, tx, s, caller)
	if err != nil {
		return Membership{}, Membership{}, err
	}
	if err := caller.require(TransferPermission); err != nil {
		return Membership{}, Membership{}, err
	}
	from = caller.Membership
	if from.Role != RoleOwner || from.Status != MembershipActive {
		return Membership{}, Membership{}, scope.ErrPermissionDenied
	}
	target, err = GetMembership(ctx, tx, s, to)
	if err != nil {
		return Membership{}, Membership{}, err
	}
	if target.ID == from.ID || target.Status != MembershipActive || target.Role != RoleAdmin {
		return Membership{}, Membership{}, zserrors.New(zserrors.CodeConflict, "Ownership transfers only to another active admin.")
	}
	if target, err = setMembership(ctx, tx, s, target, RoleOwner, MembershipActive); err != nil {
		return Membership{}, Membership{}, err
	}
	if err := guardLastOwner(ctx, tx, s, from, RoleAdmin, MembershipActive); err != nil {
		return Membership{}, Membership{}, err
	}
	if from, err = setMembership(ctx, tx, s, from, RoleAdmin, MembershipActive); err != nil {
		return Membership{}, Membership{}, err
	}
	if err := writeAudit(ctx, tx, s, caller, "tenant.ownership.transferred", "", map[string]any{
		"from_membership_id": from.PublicID(), "to_membership_id": target.PublicID(),
	}); err != nil {
		return Membership{}, Membership{}, err
	}
	if err := revokeStaleInvitations(ctx, tx, s, caller, from.ID, InvitationRevokedInviterDemoted); err != nil {
		return Membership{}, Membership{}, err
	}
	return from, target, nil
}

// canManage refuses a change to a membership with role current, ending with
// role next, unless the caller holds every permission both roles imply. An
// owner role on either side also needs the caller to be an owner. The caller
// must come from CurrentCaller in the same transaction.
func canManage(ctx context.Context, q Querier, s scope.Scope, caller Caller, current, next Role) error {
	if (current == RoleOwner || next == RoleOwner) && caller.Membership.Role != RoleOwner {
		return scope.ErrPermissionDenied
	}
	registered, err := Registered(ctx, q, s.Brand)
	if err != nil {
		return err
	}
	if err := caller.requireAll(RoleImplies(current, registered)); err != nil {
		return err
	}
	return caller.requireAll(RoleImplies(next, registered))
}

// guardLastOwner returns last_owner when target is an active owner that would
// stop being one and no other active owner remains. The tenant row is locked.
func guardLastOwner(ctx context.Context, tx pgx.Tx, s scope.Scope, target Membership, newRole Role, newStatus MembershipStatus) error {
	if target.Role != RoleOwner || target.Status != MembershipActive {
		return nil
	}
	if newRole == RoleOwner && newStatus == MembershipActive {
		return nil
	}
	args, err := s.Bind(target.ID)
	if err != nil {
		return err
	}
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships
WHERE brand_id = $1 AND tenant_id = $2 AND role = 'owner' AND status = 'active' AND id <> $3`, args...).Scan(&others); err != nil {
		return unavailable(err)
	}
	if others == 0 {
		return zserrors.New(zserrors.CodeLastOwner, "")
	}
	return nil
}

// setMembership writes a role and status and keeps the system teams in step.
func setMembership(ctx context.Context, tx pgx.Tx, s scope.Scope, m Membership, role Role, status MembershipStatus) (Membership, error) {
	args, err := s.Bind(m.ID, string(role), string(status))
	if err != nil {
		return Membership{}, err
	}
	err = tx.QueryRow(ctx, `UPDATE memberships SET role = $4, status = $5, updated_at = now(),
       removed_at = CASE WHEN $5 = 'removed' THEN now() ELSE NULL END
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 RETURNING updated_at`, args...).Scan(&m.UpdatedAt)
	if err != nil {
		return Membership{}, unavailable(err)
	}
	m.Role, m.Status = role, status
	if err := syncSystemTeams(ctx, tx, s, m); err != nil {
		return Membership{}, err
	}
	return m, nil
}

// addMembership inserts a new active membership and puts it in its system
// teams. A removed membership of the same identity is never reactivated: it
// stays as history, with the API keys revoked when it was removed, and the
// new membership has a new ID.
func addMembership(ctx context.Context, tx pgx.Tx, s scope.Scope, identityID idgen.UUID, role Role) (Membership, error) {
	existing, err := MembershipOf(ctx, tx, s, identityID)
	switch {
	case err == nil && existing.Status != MembershipRemoved:
		return Membership{}, zserrors.New(zserrors.CodeAlreadyMember, "")
	case err != nil && !errors.Is(err, ErrMemberNotFound):
		return Membership{}, err
	}
	id, err := idgen.NewV7()
	if err != nil {
		return Membership{}, err
	}
	args, err := s.Bind(id, identityID, string(role))
	if err != nil {
		return Membership{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (brand_id, tenant_id, id, identity_id, role, status)
VALUES ($1, $2, $3, $4, $5, 'active')`, args...); err != nil {
		if isUniqueViolation(err) {
			return Membership{}, zserrors.New(zserrors.CodeAlreadyMember, "")
		}
		return Membership{}, unavailable(err)
	}
	m, err := GetMembership(ctx, tx, s, id)
	if err != nil {
		return Membership{}, err
	}
	if err := syncSystemTeams(ctx, tx, s, m); err != nil {
		return Membership{}, err
	}
	return m, nil
}
