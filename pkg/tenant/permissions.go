package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Builtin is the list of built-in permissions. A brand registers more in
// the permissions list of its manifest.
var Builtin = []string{
	"tenant:read", "tenant:update", "tenant:transfer",
	"member:read", "member:invite", "member:update", "member:remove",
	"team:read", "team:manage",
	"apikey:create", "apikey:read", "apikey:revoke",
	"billing:read", "billing:manage",
	"audit:read",
}

// Role is the role of a membership.
type Role string

// Roles.
const (
	RoleOwner        Role = "owner"
	RoleAdmin        Role = "admin"
	RoleBillingAdmin Role = "billing_admin"
	RoleMember       Role = "member"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleBillingAdmin, RoleMember:
		return true
	}
	return false
}

// TransferPermission is the permission of an ownership transfer. Only owners
// hold it through their role.
const TransferPermission = "tenant:transfer"

var permissionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$`)

// ValidPermission reports whether p has the resource:action form.
func ValidPermission(p string) bool { return len(p) <= 100 && permissionPattern.MatchString(p) }

func resourceOf(p string) string {
	resource, _, _ := strings.Cut(p, ":")
	return resource
}

// RoleImplies returns the permissions a role gives by itself, out of the
// registered permissions:
//
//   - owner: every registered permission.
//   - admin: every registered permission except billing:* and
//     tenant:transfer. An admin manages members and teams but not billing.
//   - billing_admin: billing:* only.
//   - member: none. A member has what its teams grant.
func RoleImplies(role Role, registered []string) []string {
	var out []string
	for _, p := range registered {
		billing := resourceOf(p) == "billing"
		switch role {
		case RoleOwner:
			out = append(out, p)
		case RoleAdmin:
			if !billing && p != TransferPermission {
				out = append(out, p)
			}
		case RoleBillingAdmin:
			if billing {
				out = append(out, p)
			}
		}
	}
	return out
}

// Registered returns the built-in permissions and the brand's registered
// ones, sorted and without duplicates. The brand's list comes from the
// manifest of its last apply.
func Registered(ctx context.Context, q Querier, brand idgen.UUID) ([]string, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT coalesce(manifest->'permissions', '[]'::jsonb) FROM brands WHERE id = $1`, brand).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, unavailable(err)
	}
	var custom []string
	if err := json.Unmarshal(raw, &custom); err != nil {
		return nil, zserrors.Wrap(zserrors.CodeInternal, "", err)
	}
	return union(Builtin, custom), nil
}

// CheckRegistered returns permission_unknown, naming the first offender,
// unless every permission is registered.
func CheckRegistered(want, registered []string) error {
	set := toSet(registered)
	for _, p := range want {
		if _, ok := set[p]; !ok {
			name := p
			if !ValidPermission(p) {
				name = "a malformed permission"
			}
			return zserrors.New(zserrors.CodePermissionUnknown, "The permission "+name+" is not registered for this brand.")
		}
	}
	return nil
}

// Effective returns the effective permissions of an active membership: the
// union of what its role implies and what its teams grant, limited to the
// registered permissions. A membership that is not active has none.
func Effective(ctx context.Context, q Querier, s scope.Scope, membershipID idgen.UUID) ([]string, Membership, error) {
	m, err := GetMembership(ctx, q, s, membershipID)
	if err != nil {
		return nil, Membership{}, err
	}
	if m.Status != MembershipActive {
		return nil, m, nil
	}
	registered, err := Registered(ctx, q, s.Brand)
	if err != nil {
		return nil, Membership{}, err
	}
	args, err := s.Bind(membershipID)
	if err != nil {
		return nil, Membership{}, err
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT g.permission
FROM team_grants g
JOIN team_members tm ON tm.brand_id = g.brand_id AND tm.tenant_id = g.tenant_id AND tm.team_id = g.team_id
WHERE g.brand_id = $1 AND g.tenant_id = $2 AND tm.membership_id = $3`, args...)
	if err != nil {
		return nil, Membership{}, unavailable(err)
	}
	granted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, Membership{}, unavailable(err)
	}
	return intersect(union(RoleImplies(m.Role, registered), granted), registered), m, nil
}

func toSet(list []string) map[string]struct{} {
	set := make(map[string]struct{}, len(list))
	for _, p := range list {
		set[p] = struct{}{}
	}
	return set
}

func union(lists ...[]string) []string {
	set := map[string]struct{}{}
	for _, l := range lists {
		for _, p := range l {
			set[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Intersect returns the permissions present in both lists, sorted.
func Intersect(a, b []string) []string { return intersect(a, b) }

func intersect(a, b []string) []string {
	set := toSet(b)
	out := []string{}
	for _, p := range union(a) {
		if _, ok := set[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Subset reports whether every permission of want is in have.
func Subset(want, have []string) bool {
	set := toSet(have)
	for _, p := range want {
		if _, ok := set[p]; !ok {
			return false
		}
	}
	return true
}
