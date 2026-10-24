package tenant

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

var countryPattern = regexp.MustCompile(`^[A-Z]{2}$`)

// MaxNameLength is the longest tenant name, in characters. Kill Bill stores
// the account name, which is the tenant name, in a varchar(100).
const MaxNameLength = 100

func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxNameLength || !utf8.ValidString(name) {
		return "", validation("The name must be 1 to 100 characters.")
	}
	return name, nil
}

// truncateName cuts name to MaxNameLength characters on a character
// boundary, after dropping invalid UTF-8 and outer spaces.
func truncateName(name string) string {
	name = strings.TrimSpace(strings.ToValidUTF8(name, ""))
	if utf8.RuneCountInString(name) <= MaxNameLength {
		return name
	}
	runes := []rune(name)
	return strings.TrimSpace(string(runes[:MaxNameLength]))
}

func checkCountry(country string) error {
	if country != "" && !countryPattern.MatchString(country) {
		return validation("The billing_country must be an ISO 3166-1 alpha-2 code in upper case, such as CA.")
	}
	return nil
}

// Change is a change to a tenant. Nil fields are left alone. An empty
// BillingCountry clears it.
type Change struct {
	Name           *string
	BillingCountry *string
}

// Update changes the name or billing country of an active tenant. It needs
// tenant:update. The Keycloak Organization takes the new name from the
// sweeper or from SyncOrganization.
func Update(ctx context.Context, tx pgx.Tx, s scope.Scope, caller Caller, c Change) (Tenant, error) {
	if err := caller.require("tenant:update"); err != nil {
		return Tenant{}, err
	}
	if c.Name == nil && c.BillingCountry == nil {
		return Tenant{}, validation("Give a name or a billing_country.")
	}
	t, caller, err := lockForCaller(ctx, tx, s, caller)
	if err != nil {
		return Tenant{}, err
	}
	if err := caller.require("tenant:update"); err != nil {
		return Tenant{}, err
	}
	name, country := t.Name, t.BillingCountry
	if c.Name != nil {
		if name, err = checkName(*c.Name); err != nil {
			return Tenant{}, err
		}
	}
	if c.BillingCountry != nil {
		country = *c.BillingCountry
		if err := checkCountry(country); err != nil {
			return Tenant{}, err
		}
	}
	if name == t.Name && country == t.BillingCountry {
		return t, nil
	}
	args, err := s.Bind(name, nullable(country))
	if err != nil {
		return Tenant{}, err
	}
	updated, err := scanTenant(tx.QueryRow(ctx, `UPDATE tenants SET name = $3, billing_country = $4, updated_at = now()
WHERE brand_id = $1 AND id = $2 RETURNING `+tenantColumns, args...))
	if err != nil {
		return Tenant{}, unavailable(err)
	}
	details := map[string]any{}
	if name != t.Name {
		details["name"] = map[string]any{"before": t.Name, "after": name}
	}
	if country != t.BillingCountry {
		details["billing_country"] = map[string]any{"before": t.BillingCountry, "after": country}
	}
	return updated, writeAudit(ctx, tx, s, caller, "tenant.updated", "", details)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Suspend blocks every write to the tenant in Core and blocks its Kill Bill
// account through the entitlement blocking API. The Keycloak Organization is
// disabled. Reads continue. Both engines change first, so a failure leaves
// the tenant as it was and the operator retries. Suspending a suspended
// tenant is a no-op.
func Suspend(ctx context.Context, db DB, eng Engines, s scope.Scope, actor events.Actor, reason, correlationID string) (Tenant, error) {
	return transition(ctx, db, eng, s, actor, reason, correlationID, StatusSuspended)
}

// Reactivate lifts a suspension in Kill Bill, Keycloak and Core.
func Reactivate(ctx context.Context, db DB, eng Engines, s scope.Scope, actor events.Actor, reason, correlationID string) (Tenant, error) {
	return transition(ctx, db, eng, s, actor, reason, correlationID, StatusActive)
}

// Close closes the tenant. It is terminal: the Kill Bill account is closed,
// its subscriptions cancelled and unpaid invoices written off, the Keycloak
// Organization disabled, and the tenant no longer answers in Core. Every API
// key of the tenant is revoked with reason tenant_closed in the same
// transaction. A closed tenant cannot be suspended, reactivated or closed
// again: tenant_closed.
func Close(ctx context.Context, db DB, eng Engines, s scope.Scope, actor events.Actor, reason, correlationID string) (Tenant, error) {
	return transition(ctx, db, eng, s, actor, reason, correlationID, StatusClosed)
}

func transition(ctx context.Context, db DB, eng Engines, s scope.Scope, actor events.Actor, reason, correlationID string, to Status) (Tenant, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 1000 {
		return Tenant{}, zserrors.New(zserrors.CodeReasonRequired, "")
	}
	t, err := Get(ctx, db, s)
	if err != nil {
		return Tenant{}, err
	}
	if t.Status == StatusClosed {
		return Tenant{}, errTenantClosed()
	}
	if t.Status == StatusClosing {
		return Tenant{}, errTenantClosing()
	}
	if t.Status == to {
		return t, nil
	}
	be, err := eng.forBrand(ctx, db, s.Brand)
	if err != nil {
		return Tenant{}, err
	}
	cc := callContext(correlationID, reason)
	switch to {
	case StatusSuspended:
		err = eng.KillBill.SetAccountBlockingState(ctx, be.creds, cc, t.KillBillAccountID, killbill.BlockingState{
			StateName: BlockingSuspended, Service: BlockingService, IsBlockChange: true, IsBlockEntitlement: true, IsBlockBilling: true,
		})
	case StatusActive:
		err = eng.KillBill.SetAccountBlockingState(ctx, be.creds, cc, t.KillBillAccountID, killbill.BlockingState{
			StateName: BlockingActive, Service: BlockingService,
		})
	case StatusClosed:
		err = eng.KillBill.CloseAccount(ctx, be.creds, cc, t.KillBillAccountID)
	}
	if err != nil {
		return Tenant{}, engineError(err)
	}
	if err := setOrganizationEnabled(ctx, eng.Keycloak, be.realm, t.KeycloakOrganizationID, to == StatusActive); err != nil {
		return Tenant{}, engineError(err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return Tenant{}, unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	locked, err := lockTenant(ctx, tx, s)
	if err != nil {
		return Tenant{}, err
	}
	if locked.Status == StatusClosed || locked.Status == StatusClosing {
		// A close, or the start of a closure, committed while this change
		// called the engines. Closed is terminal and a closing tenant is
		// settled by its closure alone, so this change is refused. A change to
		// active has just enabled the Organization: it is restored to disabled
		// here, as in the closed case, before the refusal returns. The sweeper
		// corrects it if that call fails. A Kill Bill blocking state set by the
		// change stays: the closure blocks the account itself at its last step.
		_ = tx.Rollback(context.WithoutCancel(ctx))
		if to == StatusActive {
			_ = setOrganizationEnabled(context.WithoutCancel(ctx), eng.Keycloak, be.realm, t.KeycloakOrganizationID, false)
		}
		if locked.Status == StatusClosing {
			return Tenant{}, errTenantClosing()
		}
		return Tenant{}, errTenantClosed()
	}
	args, err := s.Bind(string(to))
	if err != nil {
		return Tenant{}, err
	}
	updated, err := scanTenant(tx.QueryRow(ctx, `UPDATE tenants SET status = $3, updated_at = now(),
       suspended_at = CASE WHEN $3 = 'suspended' THEN now() WHEN $3 = 'active' THEN NULL ELSE suspended_at END,
       closed_at = CASE WHEN $3 = 'closed' THEN now() ELSE closed_at END
WHERE brand_id = $1 AND id = $2 RETURNING `+tenantColumns, args...))
	if err != nil {
		return Tenant{}, unavailable(err)
	}
	action := map[Status]string{StatusSuspended: "tenant.suspended", StatusActive: "tenant.reactivated", StatusClosed: "tenant.closed"}[to]
	if _, err := audit.Write(ctx, tx, s, audit.Entry{
		Actor: actor, Action: action, Reason: reason, Outcome: audit.OutcomeSuccess, CorrelationID: correlationID,
		Details: map[string]any{"status": map[string]any{"before": string(locked.Status), "after": string(to)}},
	}); err != nil {
		return Tenant{}, err
	}
	if to == StatusClosed {
		if err := revokeKeys(ctx, tx, s, Caller{Actor: actor, CorrelationID: correlationID}, idgen.Nil, KeyRevokedTenantClosed); err != nil {
			return Tenant{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, unavailable(err)
	}
	return updated, nil
}

func errTenantClosed() error {
	return zserrors.New(zserrors.CodeTenantClosed, "")
}

func errTenantClosing() error {
	return zserrors.New(zserrors.CodeNotReady, "The tenant is closing, and its closure settles it.")
}

func setOrganizationEnabled(ctx context.Context, kc *keycloak.Client, realm, id string, enabled bool) error {
	org, err := kc.GetOrganization(ctx, realm, id)
	if err != nil {
		return err
	}
	if org.Enabled == enabled {
		return nil
	}
	org.Enabled = enabled
	return kc.UpdateOrganization(ctx, realm, org)
}
