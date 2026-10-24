package tenant

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idempotency"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// ProvisioningLease is how long a request or the sweeper holds a tenant
// creation while it calls the engines.
const ProvisioningLease = 2 * time.Minute

// maxRetryDelay caps the delay between two sweeper attempts of one creation.
const maxRetryDelay = time.Hour

// CreateRequest describes a tenant to create. Creator is the identity that
// becomes its owner; only an identity with a verified email is ever recorded.
type CreateRequest struct {
	Brand          idgen.UUID
	Creator        idgen.UUID
	Name           string
	BillingCountry string
	// IdempotencyKey is required for an organization and ignored for a
	// personal tenant, which is unique per identity.
	IdempotencyKey string
	// Locale is the creator's locale, such as fr or fr-CA, from the token.
	// The Kill Bill account gets it when the brand offers it, and the
	// brand's default locale otherwise. Kill Bill renders the tenant's
	// invoices and email in the account's locale.
	Locale        string
	Actor         events.Actor
	CorrelationID string
}

// provisioning is a row of tenant_provisioning.
type provisioning struct {
	TenantID       idgen.UUID
	BrandID        idgen.UUID
	Kind           Kind
	Slug           string
	Name           string
	BillingCountry string
	CreatedBy      idgen.UUID
	IdempotencyKey string
	Fingerprint    string
	Status         string
	OrgID          string
	AccountID      string
	Attempts       int
	EligibleAt     time.Time
	Claim          []byte
	// BillingLocale is the creator's locale in Kill Bill's form, such as
	// fr_CA, or "".
	BillingLocale string
}

func (p provisioning) scope() scope.Scope { return scope.Scope{Brand: p.BrandID, Tenant: p.TenantID} }

func (p provisioning) publicID() string { return idgen.Format(idgen.Tenant, p.TenantID) }

const provisioningColumns = `tenant_id, brand_id, kind, slug, name, coalesce(billing_country, ''), created_by_identity_id,
       coalesce(idempotency_key, ''), fingerprint, status, coalesce(keycloak_organization_id, ''),
       coalesce(killbill_account_id::text, ''), attempts, next_attempt_at, claim_token, coalesce(billing_locale, '')`

func scanProvisioning(row pgx.Row) (provisioning, error) {
	var p provisioning
	var kind string
	err := row.Scan(&p.TenantID, &p.BrandID, &kind, &p.Slug, &p.Name, &p.BillingCountry, &p.CreatedBy,
		&p.IdempotencyKey, &p.Fingerprint, &p.Status, &p.OrgID, &p.AccountID, &p.Attempts, &p.EligibleAt, &p.Claim, &p.BillingLocale)
	p.Kind = Kind(kind)
	return p, err
}

// CreateOrganization creates an organization tenant with the creator as its
// owner. It is idempotent on the request's idempotency key, scoped to the
// brand and the creator: the same key with the same name and billing country
// returns the same tenant, with a different payload it is
// idempotency_key_conflict, and while another request holds it,
// idempotency_key_in_progress.
//
// Core first records the creation, then creates the Keycloak Organization
// and the Kill Bill account, and only then inserts the tenant. When an engine
// fails the tenant row is not written, the failure is audited, and the
// answer is a retryable 503. A retry with the same key, or the sweeper,
// completes the same tenant.
func CreateOrganization(ctx context.Context, db DB, eng Engines, req CreateRequest) (Tenant, error) {
	name, err := checkName(req.Name)
	if err != nil {
		return Tenant{}, err
	}
	if err := checkCountry(req.BillingCountry); err != nil {
		return Tenant{}, err
	}
	if err := idempotency.ValidateKey(req.IdempotencyKey); err != nil {
		return Tenant{}, err
	}
	payload, err := json.Marshal(map[string]string{"name": name, "billing_country": req.BillingCountry})
	if err != nil {
		return Tenant{}, err
	}
	fingerprint, err := idempotency.Fingerprint("tenant.create", payload)
	if err != nil {
		return Tenant{}, err
	}
	p, existing, err := begin(ctx, db, req, KindOrganization, name, fingerprint)
	if err != nil || existing != nil {
		return deref(existing), err
	}
	return provision(ctx, db, eng, p, req.Actor, req.CorrelationID)
}

// PersonalWait is how long EnsurePersonal waits for a creation of the same
// personal tenant that another request holds, and personalPoll how often it
// looks.
const (
	PersonalWait = 5 * time.Second
	personalPoll = 100 * time.Millisecond
)

// EnsurePersonal returns the creator's personal tenant in the brand, and
// creates it when there is none. created is true when this call created it.
// A personal tenant has the creator as its only member and is created like
// an organization: a Keycloak Organization and a Kill Bill account, and
// nothing in a payment plugin. Its name is req.Name cut to 100 characters, or
// "Personal". When another request is creating the same personal tenant,
// EnsurePersonal waits up to PersonalWait for it and returns the same tenant,
// and otherwise answers a retryable not_ready. It never answers
// idempotency_key_in_progress.
func EnsurePersonal(ctx context.Context, db DB, eng Engines, req CreateRequest) (Tenant, bool, error) {
	if req.Brand.IsNil() || req.Creator.IsNil() {
		return Tenant{}, false, scope.ErrMissing
	}
	name := truncateName(req.Name)
	if name == "" {
		name = "Personal"
	}
	name, err := checkName(name)
	if err != nil {
		return Tenant{}, false, err
	}
	req.IdempotencyKey = ""
	deadline := time.Now().Add(PersonalWait)
	for {
		t, created, err := ensurePersonalOnce(ctx, db, eng, req, name)
		if !zserrors.IsCode(err, zserrors.CodeIdempotencyKeyInProgress) {
			return t, created, err
		}
		if !time.Now().Before(deadline) {
			return Tenant{}, false, zserrors.New(zserrors.CodeNotReady, "The personal tenant is still being created. Retry shortly.")
		}
		select {
		case <-ctx.Done():
			return Tenant{}, false, zserrors.Wrap(zserrors.CodeNotReady, "The personal tenant is still being created. Retry shortly.", ctx.Err())
		case <-time.After(personalPoll):
		}
	}
}

func ensurePersonalOnce(ctx context.Context, db DB, eng Engines, req CreateRequest, name string) (Tenant, bool, error) {
	var tenantID idgen.UUID
	err := db.QueryRow(ctx, `SELECT tenant_id FROM personal_tenant_principals WHERE brand_id = $1 AND identity_id = $2`,
		req.Brand, req.Creator).Scan(&tenantID)
	switch {
	case err == nil:
		t, err := Get(ctx, db, scope.Scope{Brand: req.Brand, Tenant: tenantID})
		return t, false, err
	case !errors.Is(err, pgx.ErrNoRows):
		return Tenant{}, false, unavailable(err)
	}
	p, existing, err := begin(ctx, db, req, KindPersonal, name, "personal")
	if err != nil || existing != nil {
		return deref(existing), false, err
	}
	t, err := provision(ctx, db, eng, p, req.Actor, req.CorrelationID)
	return t, err == nil, err
}

func deref(t *Tenant) Tenant {
	if t == nil {
		return Tenant{}
	}
	return *t
}

func newClaim() ([]byte, error) {
	token := make([]byte, 16)
	_, err := rand.Read(token)
	return token, err
}

// begin records the creation, or finds the one recorded for the same key, and
// claims it. It returns the tenant instead when the creation completed.
func begin(ctx context.Context, db DB, req CreateRequest, kind Kind, name, fingerprint string) (provisioning, *Tenant, error) {
	if req.Brand.IsNil() || req.Creator.IsNil() {
		return provisioning{}, nil, scope.ErrMissing
	}
	id, err := idgen.NewV7()
	if err != nil {
		return provisioning{}, nil, err
	}
	claim, err := newClaim()
	if err != nil {
		return provisioning{}, nil, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return provisioning{}, nil, unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var key any
	if kind == KindOrganization {
		key = req.IdempotencyKey
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_provisioning
    (tenant_id, brand_id, kind, slug, name, billing_country, created_by_identity_id, idempotency_key, fingerprint, billing_locale)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) ON CONFLICT DO NOTHING`,
		id, req.Brand, string(kind), deriveSlug(name, id), name, nullable(req.BillingCountry), req.Creator, key, fingerprint,
		nullable(billingLocale(req.Locale))); err != nil {
		return provisioning{}, nil, unavailable(err)
	}
	where := `brand_id = $1 AND created_by_identity_id = $2 AND idempotency_key = $3`
	args := []any{req.Brand, req.Creator, req.IdempotencyKey}
	if kind == KindPersonal {
		where, args = `brand_id = $1 AND created_by_identity_id = $2 AND kind = 'personal'`, args[:2]
	}
	p, err := scanProvisioning(tx.QueryRow(ctx, `SELECT `+provisioningColumns+` FROM tenant_provisioning WHERE `+where+` FOR UPDATE`, args...))
	if err != nil {
		return provisioning{}, nil, unavailable(err)
	}
	if p.Fingerprint != fingerprint {
		return provisioning{}, nil, zserrors.New(zserrors.CodeIdempotencyKeyConflict, "")
	}
	if p.Status == "completed" {
		t, err := Get(ctx, tx, p.scope())
		if err != nil {
			return provisioning{}, nil, err
		}
		return provisioning{}, &t, nil
	}
	var claimed bool
	err = tx.QueryRow(ctx, `UPDATE tenant_provisioning SET claim_token = $2, claimed_until = now() + $3::interval, updated_at = now()
WHERE tenant_id = $1 AND (claimed_until IS NULL OR claimed_until <= now()) RETURNING true`,
		p.TenantID, claim, fmt.Sprintf("%d seconds", int(ProvisioningLease.Seconds()))).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return provisioning{}, nil, zserrors.New(zserrors.CodeIdempotencyKeyInProgress, "")
	}
	if err != nil {
		return provisioning{}, nil, unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return provisioning{}, nil, unavailable(err)
	}
	p.Claim = claim
	return p, nil, nil
}

// deriveSlug makes a tenant slug from its name and the random tail of its
// ID: lowercase letters and digits in words joined by single hyphens.
func deriveSlug(name string, id idgen.UUID) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	base := strings.Trim(b.String(), "-")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	if base == "" {
		base = "tenant"
	}
	public := idgen.Format(idgen.Tenant, id)
	return base + "-" + public[len(public)-8:]
}

// provision creates whatever of the Keycloak Organization and the Kill Bill
// account is missing, then inserts the tenant. It holds the claim in p.
func provision(ctx context.Context, db DB, eng Engines, p provisioning, actor events.Actor, correlationID string) (Tenant, error) {
	be, err := eng.forBrand(ctx, db, p.BrandID)
	if err != nil {
		return Tenant{}, failProvisioning(ctx, db, p, "engines", err, actor, correlationID)
	}
	var keycloakUser, email string
	args, err := p.scope().Bind(p.CreatedBy)
	if err != nil {
		return Tenant{}, failProvisioning(ctx, db, p, "identity", err, actor, correlationID)
	}
	if err := db.QueryRow(ctx, `SELECT coalesce(i.keycloak_user_id, ''), CASE WHEN i.email_verified THEN coalesce(i.email, '') ELSE '' END
FROM tenant_provisioning p JOIN identities i ON i.brand_id = p.brand_id AND i.id = p.created_by_identity_id
WHERE p.brand_id = $1 AND p.tenant_id = $2 AND i.id = $3`, args...).Scan(&keycloakUser, &email); err != nil {
		return Tenant{}, failProvisioning(ctx, db, p, "identity", unavailable(err), actor, correlationID)
	}
	if p.OrgID == "" {
		org, err := ensureOrganization(ctx, eng.Keycloak, be.realm, p)
		if err != nil {
			return Tenant{}, failProvisioning(ctx, db, p, "keycloak.organization", engineError(err), actor, correlationID)
		}
		if err := recordUpstream(ctx, db, p, "keycloak_organization_id", org); err != nil {
			return Tenant{}, err
		}
		p.OrgID = org
	}
	if keycloakUser != "" {
		if err := eng.Keycloak.AddOrganizationMember(ctx, be.realm, p.OrgID, keycloakUser); err != nil && !errors.Is(err, keycloak.ErrConflict) {
			return Tenant{}, failProvisioning(ctx, db, p, "keycloak.member", engineError(err), actor, correlationID)
		}
	}
	if p.AccountID == "" {
		account, err := ensureAccount(ctx, eng.KillBill, be, p, email, correlationID)
		if err != nil {
			return Tenant{}, failProvisioning(ctx, db, p, "killbill.account", engineError(err), actor, correlationID)
		}
		if err := recordUpstream(ctx, db, p, "killbill_account_id", account); err != nil {
			return Tenant{}, err
		}
		p.AccountID = account
	}
	return completeProvisioning(ctx, db, p, actor, correlationID)
}

func ensureOrganization(ctx context.Context, kc *keycloak.Client, realm string, p provisioning) (string, error) {
	tid := p.publicID()
	found, err := kc.FindOrganizationByAttribute(ctx, realm, OrganizationTenantAttribute, tid)
	if err == nil {
		return found.ID, nil
	}
	if !errors.Is(err, keycloak.ErrNotFound) {
		return "", err
	}
	org := keycloak.Organization{
		Name: p.Name, Alias: tid, Enabled: true,
		Attributes: map[string][]string{
			OrganizationTenantAttribute: {tid},
			OrganizationBrandAttribute:  {idgen.Format(idgen.Brand, p.BrandID)},
		},
	}
	id, err := kc.CreateOrganization(ctx, realm, org)
	if errors.Is(err, keycloak.ErrConflict) {
		// Organization names are unique in a realm and tenant names are
		// not: the tenant ID makes the name unique.
		org.Name = fallbackOrganizationName(p.Name, tid)
		id, err = kc.CreateOrganization(ctx, realm, org)
	}
	if errors.Is(err, keycloak.ErrConflict) {
		found, ferr := kc.FindOrganizationByAttribute(ctx, realm, OrganizationTenantAttribute, tid)
		if ferr != nil {
			return "", err
		}
		return found.ID, nil
	}
	return id, err
}

func fallbackOrganizationName(name, tenantID string) string { return name + " (" + tenantID + ")" }

// billingLocale turns a locale from a token, such as fr or fr-CA, into Kill
// Bill's form, fr or fr_CA, or "" when it is not one.
func billingLocale(locale string) string {
	l := brand.KillBillLocale(locale)
	if !killBillLocale.MatchString(l) {
		return ""
	}
	return l
}

var killBillLocale = regexp.MustCompile(`^[a-z]{2}(_[A-Z]{2})?$`)

// accountLocale is the locale of a new Kill Bill account: the creator's
// locale when the brand offers it, otherwise the brand's default locale.
func accountLocale(be brandEngines, requested string) string {
	for _, l := range be.locales {
		if brand.KillBillLocale(l) == requested && requested != "" {
			return requested
		}
	}
	return brand.KillBillLocale(be.defaultLocale)
}

// ensureAccount finds or creates the tenant's Kill Bill account. A new
// account gets the tenant's locale and the creator's verified email, which
// Kill Bill renders invoices in and the email-notifications plugin writes
// to.
func ensureAccount(ctx context.Context, kb *killbill.Client, be brandEngines, p provisioning, email, correlationID string) (string, error) {
	tid := p.publicID()
	found, err := kb.AccountByExternalKey(ctx, be.creds, tid)
	if err == nil {
		return found.AccountID, nil
	}
	if !errors.Is(err, killbill.ErrNotFound) {
		return "", err
	}
	id, err := kb.CreateAccount(ctx, be.creds, callContext(correlationID, "tenant created"), killbill.Account{
		Name: p.Name, ExternalKey: tid, Currency: be.currency, Country: p.BillingCountry,
		Locale: accountLocale(be, p.BillingLocale), Email: email,
	})
	if errors.Is(err, killbill.ErrConflict) {
		found, ferr := kb.AccountByExternalKey(ctx, be.creds, tid)
		if ferr != nil {
			return "", err
		}
		return found.AccountID, nil
	}
	return id, err
}

// recordUpstream stores an upstream ID on the creation while the claim
// holds, so a retry reuses the object instead of creating another.
func recordUpstream(ctx context.Context, db DB, p provisioning, column, value string) error {
	var cast string
	if column == "killbill_account_id" {
		cast = "::uuid"
	}
	tag, err := execOne(ctx, db, `UPDATE tenant_provisioning SET `+column+` = $2`+cast+`, updated_at = now()
WHERE tenant_id = $1 AND claim_token = $3 AND status = 'pending'`, p.TenantID, value, p.Claim)
	if err != nil {
		return unavailable(err)
	}
	if !tag {
		return zserrors.New(zserrors.CodeIdempotencyKeyInProgress, "")
	}
	return nil
}

func execOne(ctx context.Context, db DB, sql string, args ...any) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// failProvisioning releases the claim, schedules the next sweeper attempt
// with a doubling delay, audits the failure and returns cause. Nothing of the
// tenant is written.
func failProvisioning(ctx context.Context, db DB, p provisioning, stage string, cause error, actor events.Actor, correlationID string) error {
	bg := context.WithoutCancel(ctx)
	delay := 10 * time.Second << min(p.Attempts, 12)
	if delay > maxRetryDelay || delay <= 0 {
		delay = maxRetryDelay
	}
	tx, err := db.Begin(bg)
	if err != nil {
		return cause
	}
	defer func() { _ = tx.Rollback(bg) }()
	code := codeName(cause)
	if _, err := tx.Exec(bg, `UPDATE tenant_provisioning SET attempts = attempts + 1, last_error = $3,
       next_attempt_at = now() + $4::interval, claim_token = NULL, claimed_until = NULL, updated_at = now()
WHERE tenant_id = $1 AND claim_token = $2`, p.TenantID, p.Claim, stage+": "+code, fmt.Sprintf("%d seconds", int(delay.Seconds()))); err != nil {
		return cause
	}
	if _, err := audit.Write(bg, tx, p.scope(), audit.Entry{
		Actor: actor, Action: "tenant.created", Outcome: audit.OutcomeFailure, CorrelationID: correlationID,
		Details: map[string]any{"tenant_id": p.publicID(), "kind": string(p.Kind), "stage": stage, "error": code, "attempt": p.Attempts + 1},
	}); err != nil {
		return cause
	}
	_ = tx.Commit(bg)
	return cause
}

// completeProvisioning inserts the tenant, its system teams and the owner
// membership in one transaction, and marks the creation completed.
func completeProvisioning(ctx context.Context, db DB, p provisioning, actor events.Actor, correlationID string) (Tenant, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return Tenant{}, unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var held bool
	err = tx.QueryRow(ctx, `SELECT true FROM tenant_provisioning WHERE tenant_id = $1 AND claim_token = $2 AND status = 'pending' FOR UPDATE`,
		p.TenantID, p.Claim).Scan(&held)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, zserrors.New(zserrors.CodeIdempotencyKeyInProgress, "")
	}
	if err != nil {
		return Tenant{}, unavailable(err)
	}
	s := p.scope()
	args, err := s.Bind(string(p.Kind), p.Slug, p.Name, p.OrgID, p.AccountID, nullable(p.BillingCountry), p.CreatedBy)
	if err != nil {
		return Tenant{}, err
	}
	t, err := scanTenant(tx.QueryRow(ctx, `INSERT INTO tenants
    (brand_id, id, kind, slug, name, keycloak_organization_id, killbill_account_id, billing_country, created_by_identity_id)
VALUES ($1, $2, $3, $4, $5, $6, $7::uuid, $8, $9) RETURNING `+tenantColumns, args...))
	if err != nil {
		return Tenant{}, unavailable(err)
	}
	if p.Kind == KindPersonal {
		if _, err := tx.Exec(ctx, `INSERT INTO personal_tenant_principals (brand_id, tenant_id, identity_id) VALUES ($1, $2, $3)`,
			p.BrandID, p.TenantID, p.CreatedBy); err != nil {
			return Tenant{}, unavailable(err)
		}
	}
	if err := createSystemTeams(ctx, tx, s); err != nil {
		return Tenant{}, err
	}
	owner, err := addMembership(ctx, tx, s, p.CreatedBy, RoleOwner)
	if err != nil {
		return Tenant{}, err
	}
	if err := scheduleBillingRecipient(ctx, tx, s, owner.ID, Caller{Actor: actor, CorrelationID: correlationID}); err != nil {
		return Tenant{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tenant_provisioning SET status = 'completed', completed_at = now(), updated_at = now(),
       claim_token = NULL, claimed_until = NULL, last_error = NULL WHERE tenant_id = $1`, p.TenantID); err != nil {
		return Tenant{}, unavailable(err)
	}
	if _, err := audit.Write(ctx, tx, s, audit.Entry{
		Actor: actor, Action: "tenant.created", Outcome: audit.OutcomeSuccess, CorrelationID: correlationID,
		Details: map[string]any{
			"tenant_id": t.PublicID(), "kind": string(t.Kind), "owner_membership_id": owner.PublicID(),
			"created_by_identity_id": idgen.Format(idgen.Identity, p.CreatedBy), "attempts": p.Attempts + 1,
		},
	}); err != nil {
		return Tenant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, unavailable(err)
	}
	return t, nil
}
