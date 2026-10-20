package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// InvitationTTL is how long an invitation can be accepted.
const InvitationTTL = 7 * 24 * time.Hour

// invitationTokenBytes is the entropy of an invitation token.
const invitationTokenBytes = 32

// Invitation statuses. Expired is computed from expires_at and never stored.
const (
	InvitationPending  = "pending"
	InvitationAccepted = "accepted"
	InvitationRevoked  = "revoked"
	InvitationExpired  = "expired"
)

// Invitation is a row of the invitations table. The token is never stored;
// only its SHA-256 digest is.
type Invitation struct {
	ID                    idgen.UUID
	TenantID              idgen.UUID
	Email                 string
	Role                  Role
	Status                string
	InvitedByMembershipID idgen.UUID
	ExpiresAt             time.Time
	AcceptedAt            *time.Time
	RevokedAt             *time.Time
	CreatedAt             time.Time
}

// PublicID returns the inv_ ID.
func (i Invitation) PublicID() string { return idgen.Format(idgen.Invitation, i.ID) }

// ErrInvitationNotFound is the answer for an invitation that does not exist,
// has expired, was revoked or was accepted.
var ErrInvitationNotFound = zserrors.New(zserrors.CodeInvitationNotFound, "")

func invitationDigest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

const invitationColumns = `id, tenant_id, email, role, status, invited_by_membership_id, expires_at, accepted_at, revoked_at, created_at`

func scanInvitation(row pgx.Row, now time.Time) (Invitation, error) {
	var inv Invitation
	var role string
	err := row.Scan(&inv.ID, &inv.TenantID, &inv.Email, &role, &inv.Status, &inv.InvitedByMembershipID,
		&inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt, &inv.CreatedAt)
	inv.Role = Role(role)
	if inv.Status == InvitationPending && !now.Before(inv.ExpiresAt) {
		inv.Status = InvitationExpired
	}
	return inv, err
}

func validEmail(email string) bool {
	local, domain, ok := strings.Cut(email, "@")
	return ok && local != "" && domain != "" && !strings.ContainsAny(email, " \t\r\n<>,;") &&
		len(email) <= 320 && !strings.Contains(domain, "@")
}

// CreateInvitation invites an email with a non-owner role. It needs
// member:invite, and the caller must hold every permission the role implies.
// The clear token is returned once and never stored. Personal tenants take
// no invitations: tenant_kind_unsupported.
func CreateInvitation(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, email string, role Role) (Invitation, string, error) {
	if err := caller.require("member:invite"); err != nil {
		return Invitation{}, "", err
	}
	email = strings.TrimSpace(email)
	if !validEmail(email) {
		return Invitation{}, "", validation("The email address is invalid.")
	}
	if !role.Valid() || role == RoleOwner {
		return Invitation{}, "", validation("The role must be admin, billing_admin or member. Ownership moves only by transfer.")
	}
	t, caller, err := lockForCaller(ctx, tx, s, caller)
	if err != nil {
		return Invitation{}, "", err
	}
	if err := caller.require("member:invite"); err != nil {
		return Invitation{}, "", err
	}
	if t.Kind != KindOrganization {
		return Invitation{}, "", zserrors.New(zserrors.CodeTenantKindUnsupported, "A personal tenant has one member and takes no invitations.")
	}
	if err := canManage(ctx, tx, s, caller, role, role); err != nil {
		return Invitation{}, "", err
	}
	raw := make([]byte, invitationTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Invitation{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	id, err := idgen.NewV7()
	if err != nil {
		return Invitation{}, "", err
	}
	now := time.Now().UTC()
	args, err := s.Bind(id, email, string(role), invitationDigest(token), caller.Membership.ID, now.Add(InvitationTTL))
	if err != nil {
		return Invitation{}, "", err
	}
	inv, err := scanInvitation(tx.QueryRow(ctx, `INSERT INTO invitations
    (brand_id, tenant_id, id, email, role, token_digest, invited_by_membership_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+invitationColumns, args...), now)
	if err != nil {
		return Invitation{}, "", unavailable(err)
	}
	if err := writeAudit(ctx, tx, s, caller, "tenant.invitation.created", "", map[string]any{
		"invitation_id": inv.PublicID(), "role": string(role),
	}); err != nil {
		return Invitation{}, "", err
	}
	return inv, token, nil
}

// ListInvitations returns the invitations of the tenant, oldest first. It
// needs member:invite.
func ListInvitations(ctx context.Context, q Querier, s scope.Scope, caller Caller, page Page) ([]Invitation, string, error) {
	if err := caller.require("member:invite"); err != nil {
		return nil, "", err
	}
	limit, after, err := page.After()
	if err != nil {
		return nil, "", err
	}
	args, err := s.Bind(after, limit+1)
	if err != nil {
		return nil, "", err
	}
	rows, err := q.Query(ctx, `SELECT `+invitationColumns+` FROM invitations
WHERE brand_id = $1 AND tenant_id = $2 AND id > $3 ORDER BY id LIMIT $4`, args...)
	if err != nil {
		return nil, "", unavailable(err)
	}
	now := time.Now().UTC()
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invitation, error) { return scanInvitation(r, now) })
	if err != nil {
		return nil, "", unavailable(err)
	}
	return pageOf(list, limit, func(i Invitation) idgen.UUID { return i.ID })
}

// RevokeInvitation revokes a pending invitation. It needs member:invite.
// Anything else is invitation_not_found.
func RevokeInvitation(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, id idgen.UUID) (Invitation, error) {
	if err := caller.require("member:invite"); err != nil {
		return Invitation{}, err
	}
	_, caller, err := lockForCaller(ctx, tx, s, caller)
	if err != nil {
		return Invitation{}, err
	}
	if err := caller.require("member:invite"); err != nil {
		return Invitation{}, err
	}
	args, err := s.Bind(id)
	if err != nil {
		return Invitation{}, err
	}
	now := time.Now().UTC()
	inv, err := scanInvitation(tx.QueryRow(ctx, `UPDATE invitations SET status = 'revoked', revoked_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' AND expires_at > now()
RETURNING `+invitationColumns, args...), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, unavailable(err)
	}
	if err := writeAudit(ctx, tx, s, caller, "tenant.invitation.revoked", "", map[string]any{"invitation_id": inv.PublicID()}); err != nil {
		return Invitation{}, err
	}
	return inv, nil
}

// Acceptor is the person who accepts an invitation: a recorded identity of
// the brand with a verified email, authenticated within the last 15
// minutes. The caller of AcceptInvitation establishes all three.
type Acceptor struct {
	Brand         idgen.UUID
	IdentityID    idgen.UUID
	Email         string
	EmailVerified bool
	RecentAuth    bool
	CorrelationID string
}

// RefusalRecorded wraps the error of an acceptance that AcceptInvitation
// refused and recorded in its transaction: the invitation is revoked and the
// refusal audited. Commit the transaction to keep the record, then return
// the error, which is still invitation_not_found.
type RefusalRecorded struct{ Err error }

func (r *RefusalRecorded) Error() string { return r.Err.Error() }

func (r *RefusalRecorded) Unwrap() error { return r.Err }

// AcceptInvitation adds the acceptor to the tenant of the invitation with
// its role. The invitation must belong to the acceptor's brand, be pending
// and unexpired, and its tenant must be active, or the answer is
// invitation_not_found. The acceptor's verified email must equal the invited
// address, compared case-insensitively, or the answer is
// invitation_email_mismatch.
//
// The inviter must still have the authority to send the invitation: an
// active membership with member:invite and every permission the role
// implies, read under the tenant lock. Otherwise the invitation is revoked,
// the refusal is audited as tenant.invitation.refused_stale_inviter, and the
// answer is invitation_not_found wrapped in RefusalRecorded.
//
// A removed member who accepts gets a new membership with a new ID. Its old
// membership, and the API keys revoked with it, stay as they were.
func AcceptInvitation(ctx context.Context, tx pgx.Tx, a Acceptor, token string) (Invitation, Membership, error) {
	if !a.EmailVerified {
		return Invitation{}, Membership{}, zserrors.New(zserrors.CodeEmailNotVerified, "")
	}
	if !a.RecentAuth {
		return Invitation{}, Membership{}, zserrors.New(zserrors.CodeRecentAuthRequired, "")
	}
	if a.Brand.IsNil() || a.IdentityID.IsNil() {
		return Invitation{}, Membership{}, scope.ErrMissing
	}
	if len(token) == 0 || len(token) > 128 {
		return Invitation{}, Membership{}, ErrInvitationNotFound
	}
	digest := invitationDigest(token)
	var (
		brand, tenantID idgen.UUID
		stored          []byte
	)
	// The token is the only key the acceptor holds. The lookup is still
	// limited to the acceptor's brand, so a token never reaches another
	// brand's tenant.
	err := tx.QueryRow(ctx, `SELECT brand_id, tenant_id, token_digest FROM invitations WHERE brand_id = $1 AND token_digest = $2`, a.Brand, digest).
		Scan(&brand, &tenantID, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, Membership{}, ErrInvitationNotFound
	}
	if err != nil {
		return Invitation{}, Membership{}, unavailable(err)
	}
	if subtle.ConstantTimeCompare(stored, digest) != 1 || brand != a.Brand {
		return Invitation{}, Membership{}, ErrInvitationNotFound
	}
	s := scope.Scope{Brand: brand, Tenant: tenantID}
	if _, err := lockWritable(ctx, tx, s); err != nil {
		// A tenant that is not active hides its invitations. A database
		// failure stays a retryable 503.
		if zserrors.IsCode(err, zserrors.CodeTenantNotFound) {
			return Invitation{}, Membership{}, ErrInvitationNotFound
		}
		return Invitation{}, Membership{}, err
	}
	args, err := s.Bind(digest)
	if err != nil {
		return Invitation{}, Membership{}, err
	}
	now := time.Now().UTC()
	inv, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM invitations
WHERE brand_id = $1 AND tenant_id = $2 AND token_digest = $3 FOR UPDATE`, args...), now)
	if err != nil {
		return Invitation{}, Membership{}, unavailable(err)
	}
	if inv.Status != InvitationPending {
		return Invitation{}, Membership{}, ErrInvitationNotFound
	}
	if normalizeEmail(inv.Email) != normalizeEmail(a.Email) || a.Email == "" {
		return Invitation{}, Membership{}, zserrors.New(zserrors.CodeInvitationEmailMismatch, "")
	}
	caller := Caller{Actor: events.Actor{Type: events.ActorUser, ID: idgen.Format(idgen.Identity, a.IdentityID)}, CorrelationID: a.CorrelationID}
	stale, err := inviterLostAuthority(ctx, tx, s, inv)
	if err != nil {
		return Invitation{}, Membership{}, err
	}
	if stale {
		args, err := s.Bind(inv.ID)
		if err != nil {
			return Invitation{}, Membership{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE invitations SET status = 'revoked', revoked_at = now()
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, args...); err != nil {
			return Invitation{}, Membership{}, unavailable(err)
		}
		if err := writeAudit(ctx, tx, s, caller, "tenant.invitation.refused_stale_inviter", "", map[string]any{
			"invitation_id": inv.PublicID(), "role": string(inv.Role),
			"invited_by_membership_id": idgen.Format(idgen.Membership, inv.InvitedByMembershipID),
		}); err != nil {
			return Invitation{}, Membership{}, err
		}
		return Invitation{}, Membership{}, &RefusalRecorded{Err: ErrInvitationNotFound}
	}
	m, err := addMembership(ctx, tx, s, a.IdentityID, inv.Role)
	if err != nil {
		return Invitation{}, Membership{}, err
	}
	args, err = s.Bind(inv.ID, m.ID)
	if err != nil {
		return Invitation{}, Membership{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE invitations SET status = 'accepted', accepted_at = now(), accepted_membership_id = $4
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, args...); err != nil {
		return Invitation{}, Membership{}, unavailable(err)
	}
	inv.Status = InvitationAccepted
	accepted := now
	inv.AcceptedAt = &accepted
	if err := writeAudit(ctx, tx, s, caller, "tenant.member.added", "", map[string]any{
		"membership_id": m.PublicID(), "invitation_id": inv.PublicID(), "role": string(m.Role),
	}); err != nil {
		return Invitation{}, Membership{}, err
	}
	return inv, m, nil
}

// inviterLostAuthority reports whether the membership that sent inv can no
// longer send it: it is not active, or it lacks member:invite or a
// permission the invited role implies. The tenant row is locked.
func inviterLostAuthority(ctx context.Context, tx pgx.Tx, s scope.Scope, inv Invitation) (bool, error) {
	perms, m, err := Effective(ctx, tx, s, inv.InvitedByMembershipID)
	if errors.Is(err, ErrMemberNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if m.Status != MembershipActive {
		return true, nil
	}
	registered, err := Registered(ctx, tx, s.Brand)
	if err != nil {
		return false, err
	}
	return !canInvite(Caller{Membership: m, Permissions: perms}, inv.Role, registered), nil
}
