package tenant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// BillingRecipientStatus describes the last observed email projection. It
// never promises delivery or recall of already materialized mail.
type BillingRecipientStatus string

const (
	BillingRecipientPending         BillingRecipientStatus = "pending"
	BillingRecipientReady           BillingRecipientStatus = "ready"
	BillingRecipientNoEligibleOwner BillingRecipientStatus = "no_eligible_owner"
)

// BillingRecipientReport contains no recipient address or engine credentials.
// Matches is true only after an account read agrees with current selection.
// Deferred means the persisted failure backoff has not elapsed.
type BillingRecipientReport struct {
	Status   BillingRecipientStatus
	Changed  bool
	Matches  bool
	Deferred bool
	// Inspection-only metadata. Addresses never leave the adapter through
	// this report. A false Checked value is not evidence of an empty list.
	AccountID               string
	AdditionalEmails        int
	AdditionalEmailsChecked bool
}

type billingRecipientSelection struct {
	Tenant       Tenant
	MembershipID idgen.UUID
	Email        string
	Attempts     int
}

// scheduleBillingRecipient records pending work and an existing mirror event
// in the membership transaction. A named transfer replaces the binding.
// Events contain scope only, so delayed delivery always re-reads membership.
func scheduleBillingRecipient(ctx context.Context, tx pgx.Tx, s scope.Scope, bound idgen.UUID, caller Caller) error {
	args, err := s.Bind(nullableMembership(bound))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_billing_recipients (brand_id, tenant_id, membership_id)
VALUES ($1, $2, $3)
ON CONFLICT (brand_id, tenant_id) DO UPDATE SET
    membership_id = coalesce(EXCLUDED.membership_id, tenant_billing_recipients.membership_id),
    status = 'pending', attempts = 0, next_attempt_at = now(), last_error_code = NULL`, args...); err != nil {
		return unavailable(err)
	}
	return EnqueueMirror(ctx, tx, s, caller.Actor, caller.CorrelationID)
}

func nullableMembership(id idgen.UUID) any {
	if id.IsNil() {
		return nil
	}
	return id
}

// selectBillingRecipient reads only the exact tenant and brand. A still
// eligible binding wins; fallback is deterministic by membership age and ID.
func selectBillingRecipient(ctx context.Context, q Querier, t Tenant) (billingRecipientSelection, error) {
	sel := billingRecipientSelection{Tenant: t}
	var bound idgen.UUID
	err := q.QueryRow(ctx, `SELECT coalesce(membership_id, '00000000-0000-0000-0000-000000000000'::uuid), attempts
FROM tenant_billing_recipients WHERE brand_id = $1 AND tenant_id = $2`, t.BrandID, t.ID).Scan(&bound, &sel.Attempts)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return sel, unavailable(err)
	}
	err = q.QueryRow(ctx, `SELECT m.id, btrim(i.email)
FROM memberships m JOIN identities i ON i.brand_id = m.brand_id AND i.id = m.identity_id
WHERE m.brand_id = $1 AND m.tenant_id = $2 AND m.role = 'owner' AND m.status = 'active'
  AND i.email_verified AND btrim(coalesce(i.email, '')) <> ''
ORDER BY (m.id = $3) DESC, m.created_at, m.id LIMIT 1`, t.BrandID, t.ID, bound).Scan(&sel.MembershipID, &sel.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return sel, nil
	}
	if err != nil {
		return sel, unavailable(err)
	}
	return sel, nil
}

// prepareBillingRecipient holds the same short tenant lock as membership
// writes, then releases it before any engine call. There is no upstream fence.
func prepareBillingRecipient(ctx context.Context, db DB, s scope.Scope) (billingRecipientSelection, bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return billingRecipientSelection{}, false, unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	t, err := lockTenant(ctx, tx, s)
	if err != nil {
		return billingRecipientSelection{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tenant_billing_recipients (brand_id, tenant_id)
VALUES ($1, $2) ON CONFLICT DO NOTHING`, s.Brand, s.Tenant); err != nil {
		return billingRecipientSelection{}, false, unavailable(err)
	}
	var due bool
	if err := tx.QueryRow(ctx, `SELECT next_attempt_at <= now() FROM tenant_billing_recipients
WHERE brand_id = $1 AND tenant_id = $2`, s.Brand, s.Tenant).Scan(&due); err != nil {
		return billingRecipientSelection{}, false, unavailable(err)
	}
	if !due {
		return billingRecipientSelection{}, true, nil
	}
	sel, err := selectBillingRecipient(ctx, tx, t)
	if err != nil {
		return sel, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tenant_billing_recipients SET membership_id = $3, status = 'pending'
WHERE brand_id = $1 AND tenant_id = $2`, s.Brand, s.Tenant, nullableMembership(sel.MembershipID)); err != nil {
		return sel, false, unavailable(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sel, false, unavailable(err)
	}
	return sel, false, nil
}

func recipientProjectionStatus(sel billingRecipientSelection) BillingRecipientStatus {
	if sel.MembershipID.IsNil() {
		return BillingRecipientNoEligibleOwner
	}
	return BillingRecipientReady
}

// InspectBillingRecipient compares actual upstream email with current Core
// membership without writing either system. Doctor uses it even during backoff.
func InspectBillingRecipient(ctx context.Context, db DB, eng Engines, s scope.Scope, correlationID string) (BillingRecipientReport, error) {
	report := BillingRecipientReport{Status: BillingRecipientPending}
	t, err := Get(ctx, db, s)
	if err != nil {
		return report, err
	}
	sel, err := selectBillingRecipient(ctx, db, t)
	if err != nil {
		return report, err
	}
	be, err := eng.forBillingBrand(ctx, db, s.Brand)
	if err != nil {
		return report, err
	}
	account, err := eng.KillBill.AccountByExternalKey(ctx, be.creds, t.PublicID())
	if err != nil {
		return report, engineError(err)
	}
	if account.AccountID != t.KillBillAccountID || account.ExternalKey != t.PublicID() {
		return report, zserrors.New(zserrors.CodeBillingUnavailable, "The scoped billing account does not match the Core tenant.")
	}
	report.AccountID = account.AccountID
	emails, err := eng.KillBill.AccountEmails(ctx, be.creds, account.AccountID)
	if err != nil {
		return report, engineError(err)
	}
	report.AdditionalEmails = len(emails)
	report.AdditionalEmailsChecked = true
	report.Matches = account.Email == sel.Email
	if report.Matches {
		report.Status = recipientProjectionStatus(sel)
	}
	return report, nil
}

// ReconcileBillingRecipient reads current Core eligibility, compares the
// brand-scoped Kill Bill account, and writes only a different primary email.
// Membership transactions never call this before committing. A subsequent
// pass repairs duplicate or delayed writes; no instantaneous revocation or
// cancellation of already accepted requests is promised.
func ReconcileBillingRecipient(ctx context.Context, db DB, eng Engines, s scope.Scope, correlationID string) (BillingRecipientReport, error) {
	report := BillingRecipientReport{Status: BillingRecipientPending}
	sel, deferred, err := prepareBillingRecipient(ctx, db, s)
	if err != nil || deferred {
		report.Deferred = deferred
		return report, err
	}
	be, err := eng.forBillingBrand(ctx, db, s.Brand)
	if err == nil {
		account, readErr := eng.KillBill.AccountByExternalKey(ctx, be.creds, sel.Tenant.PublicID())
		err = engineError(readErr)
		if err == nil && (account.AccountID != sel.Tenant.KillBillAccountID || account.ExternalKey != sel.Tenant.PublicID()) {
			err = zserrors.New(zserrors.CodeBillingUnavailable, "The scoped billing account does not match the Core tenant.")
		}
		if err == nil && account.Email != sel.Email {
			err = engineError(eng.KillBill.SetAccountEmail(ctx, be.creds, callContext(correlationID, "reconcile billing recipient"), account.AccountID, sel.Email))
			if err == nil {
				report.Changed = true
				account, readErr = eng.KillBill.AccountByExternalKey(ctx, be.creds, sel.Tenant.PublicID())
				err = engineError(readErr)
			}
		}
		if err == nil && (account.AccountID != sel.Tenant.KillBillAccountID || account.ExternalKey != sel.Tenant.PublicID() || account.Email != sel.Email) {
			err = zserrors.New(zserrors.CodeBillingUnavailable, "The billing recipient write has not been observed on the scoped account.")
		}
	}
	// Persist failure or observed success even if the caller's deadline ended.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	status, finishErr := finishBillingRecipient(finishCtx, db, sel, err, correlationID)
	if finishErr != nil {
		return report, errors.Join(err, finishErr)
	}
	report.Status = status
	report.Matches = err == nil && status != BillingRecipientPending
	report.Deferred = err == nil && status == BillingRecipientPending
	return report, err
}

func finishBillingRecipient(ctx context.Context, db DB, sel billingRecipientSelection, cause error, correlationID string) (BillingRecipientStatus, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return BillingRecipientPending, unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	t, err := lockTenant(ctx, tx, sel.Tenant.Scope())
	if err != nil {
		return BillingRecipientPending, err
	}
	current, err := selectBillingRecipient(ctx, tx, t)
	if err != nil {
		return BillingRecipientPending, err
	}
	status := BillingRecipientPending
	if current.MembershipID != sel.MembershipID || current.Email != sel.Email {
		// Membership or the last-verified email changed during the request.
		// Do not mark the old projection ready or delay the new desired work.
		_, err = tx.Exec(ctx, `UPDATE tenant_billing_recipients SET status = 'pending', next_attempt_at = now()
WHERE brand_id = $1 AND tenant_id = $2`, t.BrandID, t.ID)
	} else if cause != nil {
		delay := 5 * time.Second << min(current.Attempts, 12)
		if delay > 5*time.Minute || delay <= 0 {
			delay = 5 * time.Minute
		}
		_, err = tx.Exec(ctx, `UPDATE tenant_billing_recipients SET status = 'pending',
    attempts = least(attempts + 1, 1000000), last_error_code = $3, next_attempt_at = now() + $4::interval
WHERE brand_id = $1 AND tenant_id = $2`, t.BrandID, t.ID, codeName(cause), fmt.Sprintf("%d seconds", int(delay.Seconds())))
	} else {
		status = recipientProjectionStatus(current)
		_, err = tx.Exec(ctx, `UPDATE tenant_billing_recipients SET status = $3, membership_id = $4,
    checked_at = now(), attempts = 0, last_error_code = NULL, next_attempt_at = now()
WHERE brand_id = $1 AND tenant_id = $2`, t.BrandID, t.ID, string(status), nullableMembership(current.MembershipID))
	}
	if err != nil {
		return BillingRecipientPending, unavailable(err)
	}
	if status != BillingRecipientPending && sel.Tenant.BillingRecipientStatus != status {
		if _, err := audit.Write(ctx, tx, t.Scope(), audit.Entry{
			Actor: SystemActor, Action: "tenant.billing_recipient.reconciled", Outcome: audit.OutcomeSuccess,
			CorrelationID: correlationID, Details: map[string]any{"status": string(status)},
		}); err != nil {
			return BillingRecipientPending, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return BillingRecipientPending, unavailable(err)
	}
	return status, nil
}
