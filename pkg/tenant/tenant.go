// Package tenant implements tenants, memberships, teams, invitations and
// activation decisions of ZSoftly Core.
//
// A tenant is a personal or organization account inside one brand. It is
// backed by a Keycloak Organization in the brand realm and a Kill Bill
// account in the brand's Kill Bill tenant, whose external key is the Core
// tenant ID. A person reaches a tenant only through a membership Core
// records. Core membership is the source of truth for authorization; the
// Keycloak Organization mirrors it and the sweeper corrects any drift
// towards Core.
//
// Every tenant-owned query filters on the brand and the tenant through
// scope.Bind. Functions that change one tenant inside Core take a pgx.Tx, so
// a library caller can run them in its own transaction. Functions that also
// call Keycloak or Kill Bill take a DB, because a database transaction cannot
// span the engines: provisioning and lifecycle changes commit Core rows
// after the engines succeed. Membership changes commit first and reconcile
// their billing recipient and Organization projections after commit.
package tenant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Kind is personal or organization.
type Kind string

// Tenant kinds.
const (
	KindPersonal     Kind = "personal"
	KindOrganization Kind = "organization"
)

// Status is the lifecycle state of a tenant.
type Status string

// Tenant statuses. Suspended blocks every write in Core and reads continue.
// Closed is terminal.
const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusClosed    Status = "closed"
)

// Activation states.
const (
	ActivationPending  = "pending"
	ActivationApproved = "approved"
	ActivationRejected = "rejected"
)

// Tenant is a row of the tenants table.
type Tenant struct {
	ID                     idgen.UUID
	BrandID                idgen.UUID
	Kind                   Kind
	Slug                   string
	Name                   string
	Status                 Status
	Activation             string
	KeycloakOrganizationID string
	KillBillAccountID      string
	BillingCountry         string
	BillingRecipientStatus BillingRecipientStatus
	CreatedByIdentityID    idgen.UUID
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// PublicID returns the tnt_ ID.
func (t Tenant) PublicID() string { return idgen.Format(idgen.Tenant, t.ID) }

// Scope returns the brand and tenant scope of t.
func (t Tenant) Scope() scope.Scope { return scope.Scope{Brand: t.BrandID, Tenant: t.ID} }

// Querier is the subset of pgxpool.Pool and pgx.Tx the reads need.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB is a Querier that can begin transactions. Operations that call the
// engines take it, because they commit in steps.
type DB interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ErrNotFound is the answer for a tenant that does not exist in the brand.
var ErrNotFound = zserrors.New(zserrors.CodeTenantNotFound, "")

func unavailable(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
}

// tenantColumnsOf lists the tenant columns scanTenant reads, qualified with
// the table alias a, such as "t.", or unqualified when a is empty.
func tenantColumnsOf(a string) string {
	ref := a
	if ref == "" {
		ref = "tenants."
	}
	return a + "id, " + a + "brand_id, " + a + "kind, " + a + "slug, " + a + "name, " + a + "status, " + a + "activation, " +
		a + "keycloak_organization_id, " + a + "killbill_account_id::text, coalesce(" + a + "billing_country, ''), " +
		a + "created_by_identity_id, " + a + "created_at, " + a + "updated_at, " +
		"coalesce((SELECT r.status FROM tenant_billing_recipients r WHERE r.brand_id = " + ref + "brand_id AND r.tenant_id = " + ref + "id), 'pending')"
}

var tenantColumns = tenantColumnsOf("")

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	var kind, status string
	err := row.Scan(&t.ID, &t.BrandID, &kind, &t.Slug, &t.Name, &status, &t.Activation, &t.KeycloakOrganizationID,
		&t.KillBillAccountID, &t.BillingCountry, &t.CreatedByIdentityID, &t.CreatedAt, &t.UpdatedAt, &t.BillingRecipientStatus)
	t.Kind, t.Status = Kind(kind), Status(status)
	return t, err
}

// Get returns the tenant of scope s in any status, or tenant_not_found.
func Get(ctx context.Context, q Querier, s scope.Scope) (Tenant, error) {
	args, err := s.Bind()
	if err != nil {
		return Tenant{}, err
	}
	t, err := scanTenant(q.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE brand_id = $1 AND id = $2`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, unavailable(err)
	}
	return t, nil
}

// lockTenant reads the tenant of s and locks its row until tx ends. Every
// change to owners, roles and memberships of a tenant takes this lock first,
// so the last-owner guard sees a stable set of owners.
func lockTenant(ctx context.Context, tx pgx.Tx, s scope.Scope) (Tenant, error) {
	args, err := s.Bind()
	if err != nil {
		return Tenant{}, err
	}
	t, err := scanTenant(tx.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE brand_id = $1 AND id = $2 FOR UPDATE`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, unavailable(err)
	}
	return t, nil
}

// lockWritable locks the tenant and refuses one that is not active: writes
// to a suspended or closed tenant answer tenant_not_found.
func lockWritable(ctx context.Context, tx pgx.Tx, s scope.Scope) (Tenant, error) {
	t, err := lockTenant(ctx, tx, s)
	if err != nil {
		return Tenant{}, err
	}
	if t.Status != StatusActive {
		return Tenant{}, ErrNotFound
	}
	return t, nil
}

// Caller is who acts on a tenant: the acting membership, its effective
// permissions in the tenant, whether its token shows a second factor within
// the last 15 minutes, and the audit actor. Scope resolution builds it. An
// API key caller acts as the membership that created the key, with the
// key's permissions, and never has recent MFA. APIKeyID names the key and is
// nil for a person's token.
//
// Scope resolution reads the membership before the handler's transaction
// starts, so every write re-reads it with CurrentCaller after it locks the
// tenant row, and checks the caller's authority again on what it read.
type Caller struct {
	Membership    Membership
	Permissions   []string
	RecentMFA     bool
	Actor         events.Actor
	CorrelationID string
	APIKeyID      idgen.UUID
}

// CurrentCaller re-reads the caller's membership in q and returns the caller
// with its current role, status and effective permissions. Call it inside the
// write's transaction after the tenant row is locked: every change to a
// membership, a team or a grant takes the same lock, so what it reads stays
// true until the transaction ends. A membership that is no longer active is
// tenant_not_found. An API key caller also needs its key to be unrevoked and
// unexpired, and keeps only the key's permissions that the creator still
// holds.
func CurrentCaller(ctx context.Context, q Querier, s scope.Scope, c Caller) (Caller, error) {
	if c.Membership.ID.IsNil() {
		return Caller{}, ErrNotFound
	}
	perms, m, err := Effective(ctx, q, s, c.Membership.ID)
	if zserrors.IsCode(err, zserrors.CodeNotFound) {
		return Caller{}, ErrNotFound
	}
	if err != nil {
		return Caller{}, err
	}
	if m.Status != MembershipActive {
		return Caller{}, ErrNotFound
	}
	if !c.APIKeyID.IsNil() {
		args, err := s.Bind(c.APIKeyID, m.ID)
		if err != nil {
			return Caller{}, err
		}
		var keyPerms []string
		err = q.QueryRow(ctx, `SELECT permissions FROM api_keys
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND created_by_membership_id = $4
  AND revoked_at IS NULL AND expires_at > now()`, args...).Scan(&keyPerms)
		if errors.Is(err, pgx.ErrNoRows) {
			return Caller{}, ErrNotFound
		}
		if err != nil {
			return Caller{}, unavailable(err)
		}
		perms = intersect(keyPerms, perms)
	}
	c.Membership, c.Permissions = m, perms
	return c, nil
}

// lockForCaller locks a writable tenant and re-reads the caller under the
// lock. Every write of this package that a member makes starts with it.
func lockForCaller(ctx context.Context, tx pgx.Tx, s scope.Scope, c Caller) (Tenant, Caller, error) {
	t, err := lockWritable(ctx, tx, s)
	if err != nil {
		return Tenant{}, Caller{}, err
	}
	c, err = CurrentCaller(ctx, tx, s, c)
	if err != nil {
		return Tenant{}, Caller{}, err
	}
	return t, c, nil
}

// Has reports whether the caller holds permission.
func (c Caller) Has(permission string) bool {
	for _, p := range c.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

// require returns permission_denied unless the caller holds permission.
func (c Caller) require(permission string) error {
	if !c.Has(permission) {
		return scope.ErrPermissionDenied
	}
	return nil
}

// requireAll returns permission_denied unless the caller holds every
// permission in want.
func (c Caller) requireAll(want []string) error {
	for _, p := range want {
		if !c.Has(p) {
			return scope.ErrPermissionDenied
		}
	}
	return nil
}

func (c Caller) requireMFA() error {
	if !c.RecentMFA {
		return zserrors.New(zserrors.CodeRecentMFARequired, "")
	}
	return nil
}

func validation(message string) error {
	return zserrors.New(zserrors.CodeValidation, message)
}

// normalizeEmail lowercases and trims an email for comparison. Email is never
// a key; it only decides whether an invitation matches.
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }
