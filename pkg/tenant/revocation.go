package tenant

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Reasons Core records when it revokes API keys on its own. They are stored
// in api_keys.revoked_reason and in the audit event.
const (
	KeyRevokedCreatorRemoved   = "creator_removed"
	KeyRevokedCreatorSuspended = "creator_suspended"
	KeyRevokedTenantClosed     = "tenant_closed"
)

// Reasons Core records when it revokes invitations on its own.
const (
	InvitationRevokedInviterRemoved   = "inviter_removed"
	InvitationRevokedInviterDemoted   = "inviter_demoted"
	InvitationRevokedInviterSuspended = "inviter_suspended"
)

// revokeKeys revokes every unrevoked, unexpired API key of the tenant that
// creator created, or of the whole tenant when creator is nil, in tx. Each key
// is audited as tenant.api_key.revoked with reason. A revoked key never works
// again, whatever happens to its creator later.
func revokeKeys(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, creator idgen.UUID, reason string) error {
	var by any
	if !creator.IsNil() {
		by = creator
	}
	args, err := s.Bind(by, reason)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `UPDATE api_keys SET revoked_at = now(), revoked_reason = $4
WHERE brand_id = $1 AND tenant_id = $2 AND revoked_at IS NULL AND expires_at > now()
  AND ($3::uuid IS NULL OR created_by_membership_id = $3)
RETURNING id, prefix, created_by_membership_id`, args...)
	if err != nil {
		return unavailable(err)
	}
	type revoked struct {
		id, membership idgen.UUID
		prefix         string
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (revoked, error) {
		var k revoked
		err := r.Scan(&k.id, &k.prefix, &k.membership)
		return k, err
	})
	if err != nil {
		return unavailable(err)
	}
	for _, k := range list {
		if err := writeAudit(ctx, tx, s, caller, "tenant.api_key.revoked", reason, map[string]any{
			"api_key_id": idgen.Format(idgen.APIKey, k.id), "prefix": k.prefix,
			"created_by_membership_id": idgen.Format(idgen.Membership, k.membership), "revoked_reason": reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

// revokeStaleInvitations revokes the pending invitations inviter sent that
// it no longer has the authority to send: all of them when the membership is
// not active, otherwise those whose role implies a permission the inviter no
// longer holds. Each is audited as tenant.invitation.revoked with reason.
// Acceptance checks the inviter's authority again, so this keeps the list of
// pending invitations honest rather than being the only guard.
func revokeStaleInvitations(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, inviter idgen.UUID, reason string) error {
	perms, m, err := Effective(ctx, tx, s, inviter)
	if err != nil {
		return err
	}
	registered, err := Registered(ctx, tx, s.Brand)
	if err != nil {
		return err
	}
	args, err := s.Bind(inviter)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, role FROM invitations
WHERE brand_id = $1 AND tenant_id = $2 AND invited_by_membership_id = $3 AND status = 'pending' AND expires_at > now()
FOR UPDATE`, args...)
	if err != nil {
		return unavailable(err)
	}
	type pending struct {
		id   idgen.UUID
		role Role
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pending, error) {
		var p pending
		var role string
		err := r.Scan(&p.id, &role)
		p.role = Role(role)
		return p, err
	})
	if err != nil {
		return unavailable(err)
	}
	authority := Caller{Membership: m, Permissions: perms}
	for _, p := range list {
		if m.Status == MembershipActive && canInvite(authority, p.role, registered) {
			continue
		}
		args, err := s.Bind(p.id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE invitations SET status = 'revoked', revoked_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, args...); err != nil {
			return unavailable(err)
		}
		if err := writeAudit(ctx, tx, s, caller, "tenant.invitation.revoked", reason, map[string]any{
			"invitation_id": idgen.Format(idgen.Invitation, p.id), "role": string(p.role),
			"invited_by_membership_id": idgen.Format(idgen.Membership, inviter), "revoked_reason": reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

// canInvite reports whether c may invite someone with role: it holds
// member:invite and every permission the role implies. The role is never
// owner.
func canInvite(c Caller, role Role, registered []string) bool {
	if role == RoleOwner || !c.Has("member:invite") {
		return false
	}
	return Subset(RoleImplies(role, registered), c.Permissions)
}
