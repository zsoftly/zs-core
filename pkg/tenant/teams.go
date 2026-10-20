package tenant

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// The system teams. Core creates them with the tenant, locks them, and keeps
// their members in step with the membership roles in the same transaction
// as every role change.
const (
	TeamOwners  = "owners"
	TeamAdmins  = "admins"
	TeamMembers = "members"
)

// MembersTeamGrants are the grants the members team starts with. Every
// active member is in the members team, so every member can read the tenant,
// its members and its teams. The owners and admins teams carry no grants of
// their own: the role implies them.
var MembersTeamGrants = []string{"tenant:read", "member:read", "team:read"}

// systemTeamsFor returns the system teams a membership with role belongs to.
func systemTeamsFor(role Role) []string {
	switch role {
	case RoleOwner:
		return []string{TeamOwners, TeamMembers}
	case RoleAdmin:
		return []string{TeamAdmins, TeamMembers}
	}
	return []string{TeamMembers}
}

// Team is a row of the teams table with its grants and members.
type Team struct {
	ID            idgen.UUID
	Name          string
	System        bool
	Locked        bool
	Permissions   []string
	MembershipIDs []idgen.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// PublicID returns the tm_ ID.
func (t Team) PublicID() string { return idgen.Format(idgen.Team, t.ID) }

// ErrTeamNotFound is the answer for a team that does not exist in the
// tenant.
var ErrTeamNotFound = zserrors.New(zserrors.CodeNotFound, "The team was not found.")

func errTeamLocked() error {
	return zserrors.New(zserrors.CodeTeamLocked, "This team is locked: Core manages its name and members.")
}

func createSystemTeams(ctx context.Context, tx pgx.Tx, s scope.Scope) error {
	for _, name := range []string{TeamOwners, TeamAdmins, TeamMembers} {
		id, err := idgen.NewV7()
		if err != nil {
			return err
		}
		args, err := s.Bind(id, name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO teams (brand_id, tenant_id, id, name, system, locked) VALUES ($1, $2, $3, $4, true, true)`, args...); err != nil {
			return unavailable(err)
		}
		if name == TeamMembers {
			if err := replaceGrants(ctx, tx, s, id, MembersTeamGrants); err != nil {
				return err
			}
		}
	}
	return nil
}

// syncSystemTeams puts a membership in the system teams of its role and
// takes it out of the others. A removed membership leaves every team.
func syncSystemTeams(ctx context.Context, tx pgx.Tx, s scope.Scope, m Membership) error {
	removed := m.Status == MembershipRemoved
	args, err := s.Bind(m.ID, removed)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM team_members tm USING teams t
WHERE tm.brand_id = $1 AND tm.tenant_id = $2 AND tm.membership_id = $3
  AND t.brand_id = tm.brand_id AND t.tenant_id = tm.tenant_id AND t.id = tm.team_id AND (t.system OR $4)`, args...); err != nil {
		return unavailable(err)
	}
	if removed {
		return nil
	}
	args, err = s.Bind(m.ID, systemTeamsFor(m.Role))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_members (brand_id, tenant_id, team_id, membership_id)
SELECT $1, $2, t.id, $3 FROM teams t WHERE t.brand_id = $1 AND t.tenant_id = $2 AND t.system AND t.name = ANY($4)
ON CONFLICT DO NOTHING`, args...); err != nil {
		return unavailable(err)
	}
	return nil
}

func scanTeam(row pgx.Row) (Team, error) {
	var t Team
	err := row.Scan(&t.ID, &t.Name, &t.System, &t.Locked, &t.Permissions, &t.MembershipIDs, &t.CreatedAt, &t.UpdatedAt)
	if t.Permissions == nil {
		t.Permissions = []string{}
	}
	if t.MembershipIDs == nil {
		t.MembershipIDs = []idgen.UUID{}
	}
	return t, err
}

const teamSelect = `SELECT t.id, t.name, t.system, t.locked,
       coalesce((SELECT array_agg(g.permission ORDER BY g.permission) FROM team_grants g
                 WHERE g.brand_id = t.brand_id AND g.tenant_id = t.tenant_id AND g.team_id = t.id), '{}'),
       coalesce((SELECT array_agg(tm.membership_id ORDER BY tm.membership_id) FROM team_members tm
                 WHERE tm.brand_id = t.brand_id AND tm.tenant_id = t.tenant_id AND tm.team_id = t.id), '{}'),
       t.created_at, t.updated_at
FROM teams t`

// GetTeam returns a team of the tenant.
func GetTeam(ctx context.Context, q Querier, s scope.Scope, id idgen.UUID) (Team, error) {
	args, err := s.Bind(id)
	if err != nil {
		return Team{}, err
	}
	t, err := scanTeam(q.QueryRow(ctx, teamSelect+` WHERE t.brand_id = $1 AND t.tenant_id = $2 AND t.id = $3`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, ErrTeamNotFound
	}
	if err != nil {
		return Team{}, unavailable(err)
	}
	return t, nil
}

// ListTeams returns the teams of the tenant, oldest first.
func ListTeams(ctx context.Context, q Querier, s scope.Scope, page Page) ([]Team, string, error) {
	limit, after, err := page.After()
	if err != nil {
		return nil, "", err
	}
	args, err := s.Bind(after, limit+1)
	if err != nil {
		return nil, "", err
	}
	rows, err := q.Query(ctx, teamSelect+` WHERE t.brand_id = $1 AND t.tenant_id = $2 AND t.id > $3 ORDER BY t.id LIMIT $4`, args...)
	if err != nil {
		return nil, "", unavailable(err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Team, error) { return scanTeam(r) })
	if err != nil {
		return nil, "", unavailable(err)
	}
	return pageOf(list, limit, func(t Team) idgen.UUID { return t.ID })
}

func checkTeamName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 100 || !utf8.ValidString(name) {
		return "", validation("The team name must be 1 to 100 characters.")
	}
	return name, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateTeam creates an unlocked team with no members and no grants. It
// needs team:manage.
func CreateTeam(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, name string) (Team, error) {
	if err := caller.require("team:manage"); err != nil {
		return Team{}, err
	}
	name, err := checkTeamName(name)
	if err != nil {
		return Team{}, err
	}
	_, caller, err = lockForCaller(ctx, tx, s, caller)
	if err != nil {
		return Team{}, err
	}
	if err := caller.require("team:manage"); err != nil {
		return Team{}, err
	}
	id, err := idgen.NewV7()
	if err != nil {
		return Team{}, err
	}
	args, err := s.Bind(id, name)
	if err != nil {
		return Team{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO teams (brand_id, tenant_id, id, name) VALUES ($1, $2, $3, $4)`, args...); err != nil {
		if isUniqueViolation(err) {
			return Team{}, zserrors.New(zserrors.CodeConflict, "A team with this name already exists.")
		}
		return Team{}, unavailable(err)
	}
	t, err := GetTeam(ctx, tx, s, id)
	if err != nil {
		return Team{}, err
	}
	return t, writeAudit(ctx, tx, s, caller, "tenant.team.created", "", map[string]any{"team_id": t.PublicID(), "name": name})
}

// manageableTeam locks the tenant, re-reads the caller, reads the team and
// refuses it unless the caller holds team:manage and every permission the
// team grants. Nobody can change a team that grants more than they hold. It
// returns the caller as read under the lock.
func manageableTeam(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, id idgen.UUID) (Team, Caller, error) {
	if err := caller.require("team:manage"); err != nil {
		return Team{}, Caller{}, err
	}
	_, caller, err := lockForCaller(ctx, tx, s, caller)
	if err != nil {
		return Team{}, Caller{}, err
	}
	if err := caller.require("team:manage"); err != nil {
		return Team{}, Caller{}, err
	}
	t, err := GetTeam(ctx, tx, s, id)
	if err != nil {
		return Team{}, Caller{}, err
	}
	if err := caller.requireAll(t.Permissions); err != nil {
		return Team{}, Caller{}, err
	}
	return t, caller, nil
}

// RenameTeam renames an unlocked team.
func RenameTeam(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, id idgen.UUID, name string) (Team, error) {
	name, err := checkTeamName(name)
	if err != nil {
		return Team{}, err
	}
	t, caller, err := manageableTeam(ctx, tx, s, caller, id)
	if err != nil {
		return Team{}, err
	}
	if t.Locked {
		return Team{}, errTeamLocked()
	}
	args, err := s.Bind(id, name)
	if err != nil {
		return Team{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE teams SET name = $4, updated_at = now() WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, args...); err != nil {
		if isUniqueViolation(err) {
			return Team{}, zserrors.New(zserrors.CodeConflict, "A team with this name already exists.")
		}
		return Team{}, unavailable(err)
	}
	if err := writeAudit(ctx, tx, s, caller, "tenant.team.renamed", "", map[string]any{
		"team_id": t.PublicID(), "name": map[string]any{"before": t.Name, "after": name},
	}); err != nil {
		return Team{}, err
	}
	return GetTeam(ctx, tx, s, id)
}

// DeleteTeam deletes an unlocked team with its grants and members.
func DeleteTeam(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, id idgen.UUID) error {
	t, caller, err := manageableTeam(ctx, tx, s, caller, id)
	if err != nil {
		return err
	}
	if t.Locked {
		return errTeamLocked()
	}
	args, err := s.Bind(id)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM teams WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, args...); err != nil {
		return unavailable(err)
	}
	return writeAudit(ctx, tx, s, caller, "tenant.team.deleted", "", map[string]any{
		"team_id": t.PublicID(), "name": t.Name, "permissions": t.Permissions,
	})
}

// SetTeamMembers replaces the members of an unlocked team. Every membership
// must be an active or suspended member of the tenant.
func SetTeamMembers(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, id idgen.UUID, members []idgen.UUID) (Team, error) {
	if len(members) > MaxLimit {
		return Team{}, validation("A team holds at most 200 members in one request.")
	}
	t, caller, err := manageableTeam(ctx, tx, s, caller, id)
	if err != nil {
		return Team{}, err
	}
	if t.Locked {
		return Team{}, errTeamLocked()
	}
	members = uniqueIDs(members)
	args, err := s.Bind(members)
	if err != nil {
		return Team{}, err
	}
	var found int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM memberships
WHERE brand_id = $1 AND tenant_id = $2 AND id = ANY($3) AND status <> 'removed'`, args...).Scan(&found); err != nil {
		return Team{}, unavailable(err)
	}
	if found != len(members) {
		return Team{}, ErrMemberNotFound
	}
	args, err = s.Bind(id, members)
	if err != nil {
		return Team{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM team_members WHERE brand_id = $1 AND tenant_id = $2 AND team_id = $3 AND NOT (membership_id = ANY($4))`, args...); err != nil {
		return Team{}, unavailable(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_members (brand_id, tenant_id, team_id, membership_id)
SELECT $1, $2, $3, unnest($4::uuid[]) ON CONFLICT DO NOTHING`, args...); err != nil {
		return Team{}, unavailable(err)
	}
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = idgen.Format(idgen.Membership, m)
	}
	if err := writeAudit(ctx, tx, s, caller, "tenant.team.members.set", "", map[string]any{"team_id": t.PublicID(), "membership_ids": ids}); err != nil {
		return Team{}, err
	}
	return GetTeam(ctx, tx, s, id)
}

// SetTeamGrants replaces the grants of a team, locked or not. Every
// permission must be registered for the brand, and the caller must hold
// every permission the team grants before and after the change.
func SetTeamGrants(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, id idgen.UUID, permissions []string) (Team, error) {
	if len(permissions) > MaxLimit {
		return Team{}, validation("A team holds at most 200 grants.")
	}
	permissions = union(permissions)
	registered, err := Registered(ctx, tx, s.Brand)
	if err != nil {
		return Team{}, err
	}
	if err := CheckRegistered(permissions, registered); err != nil {
		return Team{}, err
	}
	t, caller, err := manageableTeam(ctx, tx, s, caller, id)
	if err != nil {
		return Team{}, err
	}
	if err := caller.requireAll(permissions); err != nil {
		return Team{}, err
	}
	if err := replaceGrants(ctx, tx, s, id, permissions); err != nil {
		return Team{}, err
	}
	if err := writeAudit(ctx, tx, s, caller, "tenant.team.grants.set", "", map[string]any{
		"team_id": t.PublicID(), "permissions": map[string]any{"before": t.Permissions, "after": permissions},
	}); err != nil {
		return Team{}, err
	}
	return GetTeam(ctx, tx, s, id)
}

func replaceGrants(ctx context.Context, tx pgx.Tx, s scope.Scope, team idgen.UUID, permissions []string) error {
	args, err := s.Bind(team, permissions)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM team_grants WHERE brand_id = $1 AND tenant_id = $2 AND team_id = $3 AND NOT (permission = ANY($4))`, args...); err != nil {
		return unavailable(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_grants (brand_id, tenant_id, team_id, permission)
SELECT $1, $2, $3, unnest($4::text[]) ON CONFLICT DO NOTHING`, args...); err != nil {
		return unavailable(err)
	}
	return nil
}

func uniqueIDs(in []idgen.UUID) []idgen.UUID {
	seen := map[idgen.UUID]bool{}
	out := make([]idgen.UUID, 0, len(in))
	for _, id := range in {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// writeAudit appends a tenant event for the caller in tx.
func writeAudit(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, action, reason string, details map[string]any) error {
	_, err := audit.Write(ctx, tx, s, audit.Entry{
		Actor: caller.Actor, Action: action, Reason: reason, Outcome: audit.OutcomeSuccess,
		CorrelationID: caller.CorrelationID, Details: details,
	})
	return err
}
