package tenant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// ClosureProcessResult describes one closure processor pass.
type ClosureProcessResult struct {
	Claimed   bool
	Attempted bool
}

// ClosureExecutionResult describes the separately invoked D1b execution pass.
type ClosureExecutionResult struct {
	Posted    bool
	Evidenced bool
	// NoInvoice is true when Kill Bill answered that there was nothing to
	// invoice for the target date.
	NoInvoice bool
}

const (
	// closurePostBound is the most a started invoice POST may take. It runs
	// apart from the caller's context, so a stop after the start still sees
	// the answer and records it.
	closurePostBound = 25 * time.Second
	// closureWriteAttempts is how many times an outcome is written, and
	// closureWriteBackoff the growing wait between tries.
	closureWriteAttempts = 3
	closureWriteBackoff  = 200 * time.Millisecond
)

// ProcessClosureInvoiceOnce commits at most one immutable D1a closure intent.
// It does not resolve secrets, call engines, or change closure completion.
func ProcessClosureInvoiceOnce(ctx context.Context, db DB, _ Engines, sc scope.Scope) (ClosureProcessResult, error) {
	if _, ok := db.(pgx.Tx); ok {
		return ClosureProcessResult{}, zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
	}
	if _, err := sc.Bind(); err != nil {
		return ClosureProcessResult{}, err
	}
	rec, err := brand.Get(ctx, db, sc.Brand)
	if err != nil {
		return ClosureProcessResult{}, err
	}
	kbTenant, err := idgen.ParseUUID(rec.KillBillTenantID)
	if err != nil {
		return ClosureProcessResult{}, zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
	}
	attemptID := idgen.MustNewV7()
	claimed := false
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var (
			status, settlement, accountID string
			targetDate                    time.Time
			cancelled, drained            *time.Time
			requested                     *time.Time
			legacyAttempt, legacyInvoice  *idgen.UUID
		)
		err := tx.QueryRow(ctx, `SELECT t.status, s.status, t.killbill_account_id::text, s.finance_target_date,
    s.cancellation_completed_at, s.drain_completed_at, s.finance_requested_at, s.finance_attempt_id, s.finance_invoice_id
FROM tenants t JOIN tenant_closure_settlements s ON s.brand_id = t.brand_id AND s.tenant_id = t.id
WHERE t.brand_id = $1 AND t.id = $2 FOR UPDATE`, sc.Brand, sc.Tenant).Scan(&status, &settlement, &accountID, &targetDate,
			&cancelled, &drained, &requested, &legacyAttempt, &legacyInvoice)
		if errors.Is(err, pgx.ErrNoRows) {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
		}
		if err != nil {
			return unavailable(err)
		}
		if status != "closing" || settlement != "finance_pending" || cancelled == nil || drained == nil || requested != nil || legacyAttempt != nil || legacyInvoice != nil {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
		}
		kbAccount, err := idgen.ParseUUID(accountID)
		if err != nil {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice processing is not ready.")
		}
		tag, err := tx.Exec(ctx, `INSERT INTO tenant_closure_invoice_attempts
    (id, brand_id, tenant_id, killbill_tenant_id, killbill_account_id, target_date)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (brand_id, tenant_id) DO NOTHING`, attemptID, sc.Brand, sc.Tenant, kbTenant, kbAccount, targetDate)
		if err != nil {
			return unavailable(err)
		}
		claimed = tag.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return ClosureProcessResult{}, err
	}
	return ClosureProcessResult{Claimed: claimed}, nil
}

// ExecuteClosureInvoiceOnce records a durable attempt before invoking Kill Bill.
// It never marks financial completion or changes the tenant lifecycle.
func ExecuteClosureInvoiceOnce(ctx context.Context, db DB, eng Engines, sc scope.Scope) (ClosureExecutionResult, error) {
	if _, ok := db.(pgx.Tx); ok {
		return ClosureExecutionResult{}, zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is not ready.")
	}
	if _, err := sc.Bind(); err != nil {
		return ClosureExecutionResult{}, err
	}
	rec, err := brand.Get(ctx, db, sc.Brand)
	if err != nil || rec.Manifest == nil || len(rec.Manifest.Billing.Currencies) == 0 {
		if err != nil {
			return ClosureExecutionResult{}, err
		}
		return ClosureExecutionResult{}, zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is not ready.")
	}
	var (
		attemptID        idgen.UUID
		accountID        string
		targetDate       time.Time
		attemptTenant    string
		started          bool
		recorded         bool
		noInvoice        bool
		invoiceID        string
		expectedCurrency string
	)
	manualReview := zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is pending manual review.")
	// lock reads the attempt under its row lock and refuses it unless the
	// tenant is closing, the settlement waits for finance, and the brand still
	// stores the Kill Bill tenant the attempt was claimed against.
	lock := func(tx pgx.Tx) error {
		var status, settlement string
		err := tx.QueryRow(ctx, `SELECT a.id, a.killbill_account_id::text, a.target_date, t.status, s.status, a.killbill_tenant_id::text
FROM tenant_closure_invoice_attempts a
JOIN tenants t ON t.brand_id = a.brand_id AND t.id = a.tenant_id
JOIN tenant_closure_settlements s ON s.brand_id = a.brand_id AND s.tenant_id = a.tenant_id
WHERE a.brand_id = $1 AND a.tenant_id = $2 FOR UPDATE`, sc.Brand, sc.Tenant).Scan(&attemptID, &accountID, &targetDate, &status, &settlement, &attemptTenant)
		if errors.Is(err, pgx.ErrNoRows) {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is not ready.")
		}
		if err != nil {
			return unavailable(err)
		}
		if status != "closing" || settlement != "finance_pending" {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is not ready.")
		}
		if !strings.EqualFold(attemptTenant, rec.KillBillTenantID) {
			return manualReview
		}
		return nil
	}
	// First read what is already recorded. Nothing is started yet.
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := lock(tx); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_closure_invoice_outcomes WHERE attempt_id = $1 AND kind = 'no_invoice')`, attemptID).Scan(&noInvoice); err != nil {
			return unavailable(err)
		}
		err := tx.QueryRow(ctx, `SELECT started.expected_currency, coalesce(invoice.invoice_id::text, '')
FROM tenant_closure_invoice_outcomes started
LEFT JOIN tenant_closure_invoice_outcomes invoice ON invoice.attempt_id = started.attempt_id AND invoice.kind = 'invoice_id'
WHERE started.attempt_id = $1 AND started.kind = 'started'`, attemptID).Scan(&expectedCurrency, &invoiceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return unavailable(err)
		}
		recorded = true
		return nil
	})
	if err != nil {
		return ClosureExecutionResult{}, err
	}
	if noInvoice {
		return ClosureExecutionResult{NoInvoice: true}, nil
	}
	if recorded && invoiceID == "" {
		return ClosureExecutionResult{}, manualReview
	}
	// Preflight, before anything is started: the credentials resolve, Kill
	// Bill answers, and the tenant its API key resolves to is the one the
	// brand stores and the attempt was claimed against. A failure here leaves
	// nothing to review.
	be, err := eng.forBillingBrand(ctx, db, sc.Brand)
	if err != nil {
		return ClosureExecutionResult{}, err
	}
	live, err := eng.KillBill.TenantByAPIKey(ctx, be.creds.APIKey)
	if err != nil {
		return ClosureExecutionResult{}, engineError(err)
	}
	if !strings.EqualFold(live.TenantID, rec.KillBillTenantID) || !strings.EqualFold(live.TenantID, attemptTenant) {
		return ClosureExecutionResult{}, manualReview
	}
	if !recorded {
		err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			if err := lock(tx); err != nil {
				return err
			}
			expectedCurrency = rec.Manifest.Billing.Currencies[0]
			tag, err := tx.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, expected_currency)
VALUES ($1, $2, 'started', $3)
ON CONFLICT (attempt_id, kind) DO NOTHING`, idgen.MustNewV7(), attemptID, expectedCurrency)
			if err != nil {
				return unavailable(err)
			}
			started = tag.RowsAffected() == 1
			return nil
		})
		if err != nil {
			return ClosureExecutionResult{}, err
		}
		if !started {
			// Another executor started it between the read and now.
			return ClosureExecutionResult{}, manualReview
		}
	}
	if started {
		postCtx, cancelPost := context.WithTimeout(context.WithoutCancel(ctx), closurePostBound)
		trigger, err := eng.KillBill.TriggerInvoice(postCtx, be.creds, callContext("", "closure invoice"), accountID, targetDate.Format(time.DateOnly))
		cancelPost()
		if err != nil {
			if persistErr := recordClosureInvoiceOutcome(ctx, db, sc, attemptID, "post_unknown", ""); persistErr != nil {
				return ClosureExecutionResult{}, errors.Join(engineError(err), persistErr)
			}
			return ClosureExecutionResult{}, engineError(err)
		}
		if !trigger.Created {
			// Kill Bill answered that there is nothing to invoice. That is
			// definite: it is recorded and the attempt is never posted again.
			if err := recordClosureInvoiceOutcome(ctx, db, sc, attemptID, "no_invoice", ""); err != nil {
				return ClosureExecutionResult{Posted: true}, err
			}
			return ClosureExecutionResult{Posted: true, NoInvoice: true}, nil
		}
		invoiceID = trigger.InvoiceID
		if err := recordClosureInvoiceOutcome(ctx, db, sc, attemptID, "invoice_id", invoiceID); err != nil {
			return ClosureExecutionResult{}, err
		}
	}
	invoice, err := eng.KillBill.Invoice(ctx, be.creds, invoiceID)
	if err != nil {
		return ClosureExecutionResult{Posted: started}, engineError(err)
	}
	if invoice.InvoiceID != invoiceID || invoice.AccountID != accountID || invoice.Status != "COMMITTED" || invoice.Currency != expectedCurrency || invoice.TargetDate != targetDate.Format(time.DateOnly) {
		if err := recordClosureInvoiceOutcome(ctx, db, sc, attemptID, "read_mismatch", invoiceID); err != nil {
			return ClosureExecutionResult{Posted: started}, err
		}
		return ClosureExecutionResult{Posted: started}, zserrors.New(zserrors.CodeNotReady, "Closure invoice readback is pending manual review.")
	}
	if err := recordClosureInvoiceOutcome(ctx, db, sc, attemptID, "evidenced", invoiceID); err != nil {
		return ClosureExecutionResult{Posted: started}, err
	}
	return ClosureExecutionResult{Posted: started, Evidenced: true}, nil
}

// recordClosureInvoiceOutcome writes one outcome apart from the caller's
// context, because it often records what Kill Bill just answered. A database
// failure is retried a few times; a refusal is not.
func recordClosureInvoiceOutcome(ctx context.Context, db DB, sc scope.Scope, attemptID idgen.UUID, kind, invoiceID string) error {
	ctx = context.WithoutCancel(ctx)
	var err error
	for attempt := 0; attempt < closureWriteAttempts; attempt++ {
		if err = recordClosureInvoiceOutcomeOnce(ctx, db, sc, attemptID, kind, invoiceID); err == nil || !zserrors.IsCode(err, zserrors.CodeDatabaseUnavailable) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * closureWriteBackoff)
	}
	return err
}

func recordClosureInvoiceOutcomeOnce(ctx context.Context, db DB, sc scope.Scope, attemptID idgen.UUID, kind, invoiceID string) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var invoice *idgen.UUID
		if invoiceID != "" {
			parsed, err := idgen.ParseUUID(invoiceID)
			if err != nil {
				return zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is pending manual review.")
			}
			invoice = &parsed
		}
		tag, err := tx.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, invoice_id)
SELECT $1, $2, $3, $4
WHERE EXISTS (SELECT 1 FROM tenant_closure_invoice_attempts WHERE id = $2 AND brand_id = $5 AND tenant_id = $6)
ON CONFLICT (attempt_id, kind) DO NOTHING`, idgen.MustNewV7(), attemptID, kind, invoice, sc.Brand, sc.Tenant)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		// An outcome already recorded for this attempt, kind and invoice is a
		// repeat of the same fact, not a conflict. Anything else is not.
		var same bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_closure_invoice_outcomes o
JOIN tenant_closure_invoice_attempts a ON a.id = o.attempt_id
WHERE o.attempt_id = $1 AND o.kind = $2 AND o.invoice_id IS NOT DISTINCT FROM $3 AND a.brand_id = $4 AND a.tenant_id = $5)`,
			attemptID, kind, invoice, sc.Brand, sc.Tenant).Scan(&same); err != nil {
			return unavailable(err)
		}
		if !same {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is pending manual review.")
		}
		return nil
	})
}
