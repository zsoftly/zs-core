package tenant

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// The closure invoice answers these when it needs a person: an unknown
// outcome of the POST, a start another executor made, or a readback that did
// not match. RunClosureInvoice reports them as manual review, not as errors to
// retry.
//
// Every statement of the closure code names the brand and the tenant as its
// first two arguments, because the lifecycle guard checks them against the
// scope it holds before any SQL runs.
var (
	errClosureManualReview   = zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is pending manual review.")
	errClosureReadbackReview = zserrors.New(zserrors.CodeNotReady, "Closure invoice readback is pending manual review.")
)

// IsClosureManualReview reports whether err says the closure invoice waits
// for manual review.
func IsClosureManualReview(err error) bool {
	return errors.Is(err, errClosureManualReview) || errors.Is(err, errClosureReadbackReview)
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
VALUES ($3, $1, $2, $4, $5, $6)
ON CONFLICT (brand_id, tenant_id) DO NOTHING`, sc.Brand, sc.Tenant, attemptID, kbTenant, kbAccount, targetDate)
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
	var (
		rec              brand.Record
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
	manualReview := errClosureManualReview
	// lock reads the attempt under its row lock and, inside that lock, the
	// brand, once. It refuses the attempt unless the tenant is closing, the
	// settlement waits for finance, and the brand still stores the Kill Bill
	// tenant the attempt was claimed against.
	lock := func(ctx context.Context, tx pgx.Tx) error {
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
		if rec, err = brand.Get(ctx, tx, sc.Brand); err != nil {
			return err
		}
		if rec.Manifest == nil || len(rec.Manifest.Billing.Currencies) == 0 {
			return zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is not ready.")
		}
		if !strings.EqualFold(attemptTenant, rec.KillBillTenantID) {
			return manualReview
		}
		return nil
	}
	// First read what is already recorded. Nothing is started yet.
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := lock(ctx, tx); err != nil {
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
	// A recorded start or invoice always wins: a real invoice is never hidden.
	if noInvoice && !recorded {
		return ClosureExecutionResult{NoInvoice: true}, nil
	}
	if recorded && invoiceID == "" {
		return ClosureExecutionResult{}, manualReview
	}
	// Preflight, before anything is started: the credentials resolve, Kill
	// Bill answers, and the tenant its API key resolves to is the one the
	// brand stores and the attempt was claimed against. A failure here leaves
	// nothing to review.
	read := rec
	be, err := eng.billingFor(ctx, read)
	if err != nil {
		return ClosureExecutionResult{}, err
	}
	live, err := eng.KillBill.TenantByAPIKey(ctx, be.creds.APIKey)
	if err != nil {
		return ClosureExecutionResult{}, engineError(err)
	}
	if !strings.EqualFold(live.TenantID, read.KillBillTenantID) || !strings.EqualFold(live.TenantID, attemptTenant) {
		return ClosureExecutionResult{}, manualReview
	}
	if !recorded {
		// The account is read with the brand credentials: it proves the secret,
		// that the account belongs to this tenant, and the currency to expect.
		acct, err := eng.KillBill.AccountWithBalance(ctx, be.creds, accountID)
		if err != nil {
			return ClosureExecutionResult{}, engineError(err)
		}
		if !strings.EqualFold(acct.AccountID, accountID) || !slices.Contains(read.Manifest.Billing.Currencies, acct.Currency) {
			return ClosureExecutionResult{}, zserrors.New(zserrors.CodeNotReady, "Closure invoice execution is not ready.")
		}
		expectedCurrency = acct.Currency
		// The dry run asks for the invoice a real run would create for the same
		// account and target date. It answers 204 when there is nothing to
		// invoice, which is recorded as definite and posts nothing. The tenant
		// is closing, so nothing new is billed between this read and the POST
		// below.
		_, found, err := eng.KillBill.DryRunInvoice(ctx, be.creds, callContext("", "closure invoice dry run"), accountID, targetDate.Format(time.DateOnly))
		if err != nil {
			return ClosureExecutionResult{}, engineError(err)
		}
		if !found {
			// The dry run may be stale: another executor can have started and
			// posted since this one read. So no_invoice is recorded under the
			// attempt lock, and only while nothing has been started.
			// Every statement of this write runs apart from the caller's context,
			// so a stop after the dry run cannot leave the answer unrecorded.
			writeCtx := context.WithoutCancel(ctx)
			err := withClosureWriteRetries(func() error {
				return pgx.BeginFunc(writeCtx, db, func(tx pgx.Tx) error {
					if err := lock(writeCtx, tx); err != nil {
						return err
					}
					var begun bool
					if err := tx.QueryRow(writeCtx, `SELECT EXISTS (SELECT 1 FROM tenant_closure_invoice_outcomes
WHERE attempt_id = $1 AND kind IN ('started', 'invoice_id', 'post_unknown'))`, attemptID).Scan(&begun); err != nil {
						return unavailable(err)
					}
					if begun {
						return manualReview
					}
					if _, err := tx.Exec(writeCtx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind)
SELECT $3, $4, 'no_invoice'
WHERE EXISTS (SELECT 1 FROM tenant_closure_invoice_attempts WHERE id = $4 AND brand_id = $1 AND tenant_id = $2)
ON CONFLICT (attempt_id, kind) DO NOTHING`, sc.Brand, sc.Tenant, idgen.MustNewV7(), attemptID); err != nil {
						return unavailable(err)
					}
					return nil
				})
			})
			if err != nil {
				return ClosureExecutionResult{}, err
			}
			return ClosureExecutionResult{NoInvoice: true}, nil
		}
		err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			if err := lock(ctx, tx); err != nil {
				return err
			}
			if rec.KillBillTenantID != read.KillBillTenantID || rec.KillBillAPIKey != read.KillBillAPIKey ||
				rec.KillBillAPISecretRef != read.KillBillAPISecretRef || rec.Status != read.Status {
				return manualReview
			}
			var nothing bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_closure_invoice_outcomes WHERE attempt_id = $1 AND kind = 'no_invoice')`, attemptID).Scan(&nothing); err != nil {
				return unavailable(err)
			}
			if nothing {
				return errClosureNothingToInvoice
			}
			tag, err := tx.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, expected_currency)
SELECT $3, $4, 'started', $5
WHERE EXISTS (SELECT 1 FROM tenant_closure_invoice_attempts WHERE id = $4 AND brand_id = $1 AND tenant_id = $2)
ON CONFLICT (attempt_id, kind) DO NOTHING`, sc.Brand, sc.Tenant, idgen.MustNewV7(), attemptID, expectedCurrency)
			if err != nil {
				return unavailable(err)
			}
			started = tag.RowsAffected() == 1
			return nil
		})
		if errors.Is(err, errClosureNothingToInvoice) {
			return ClosureExecutionResult{NoInvoice: true}, nil
		}
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
		invoiceID, err = eng.KillBill.TriggerInvoice(postCtx, be.creds, callContext("", "closure invoice"), accountID, targetDate.Format(time.DateOnly))
		cancelPost()
		if err != nil {
			// Any failure of the real POST, a bare 404 included, proves nothing:
			// it is an unknown outcome for manual review, never posted again.
			if persistErr := recordClosureInvoiceOutcome(ctx, db, sc, attemptID, "post_unknown", ""); persistErr != nil {
				return ClosureExecutionResult{}, errors.Join(engineError(err), persistErr)
			}
			return ClosureExecutionResult{}, engineError(err)
		}
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
		return ClosureExecutionResult{Posted: started}, errClosureReadbackReview
	}
	if err := recordClosureInvoiceOutcome(ctx, db, sc, attemptID, "evidenced", invoiceID); err != nil {
		return ClosureExecutionResult{Posted: started}, err
	}
	return ClosureExecutionResult{Posted: started, Evidenced: true}, nil
}

// errClosureNothingToInvoice stops a start that finds nothing-to-invoice
// already recorded for the attempt.
var errClosureNothingToInvoice = errors.New("closure invoice has nothing to invoice")

// withClosureWriteRetries runs a database write, trying it up to
// closureWriteAttempts times when it fails because the database is
// unavailable, and waiting longer between tries, but not after the last.
func withClosureWriteRetries(write func() error) error {
	var err error
	for attempt := 0; attempt < closureWriteAttempts; attempt++ {
		if err = write(); err == nil || !zserrors.IsCode(err, zserrors.CodeDatabaseUnavailable) {
			return err
		}
		if attempt < closureWriteAttempts-1 {
			time.Sleep(time.Duration(attempt+1) * closureWriteBackoff)
		}
	}
	return err
}

// recordClosureInvoiceOutcome writes one outcome apart from the caller's
// context, because it often records what Kill Bill just answered. A database
// failure is retried a few times; a refusal is not. When every try fails the
// error names the invoice, so staff can find it in Kill Bill.
func recordClosureInvoiceOutcome(ctx context.Context, db DB, sc scope.Scope, attemptID idgen.UUID, kind, invoiceID string) error {
	ctx = context.WithoutCancel(ctx)
	err := withClosureWriteRetries(func() error {
		return recordClosureInvoiceOutcomeOnce(ctx, db, sc, attemptID, kind, invoiceID)
	})
	if err != nil && invoiceID != "" {
		return fmt.Errorf("closure invoice %s outcome %q not recorded: %w", invoiceID, kind, err)
	}
	return err
}

func recordClosureInvoiceOutcomeOnce(ctx context.Context, db DB, sc scope.Scope, attemptID idgen.UUID, kind, invoiceID string) error {
	return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var invoice *idgen.UUID
		if invoiceID != "" {
			parsed, err := idgen.ParseUUID(invoiceID)
			if err != nil {
				return errClosureManualReview
			}
			invoice = &parsed
		}
		tag, err := tx.Exec(ctx, `INSERT INTO tenant_closure_invoice_outcomes (id, attempt_id, kind, invoice_id)
SELECT $3, $4, $5, $6
WHERE EXISTS (SELECT 1 FROM tenant_closure_invoice_attempts WHERE id = $4 AND brand_id = $1 AND tenant_id = $2)
ON CONFLICT (attempt_id, kind) DO NOTHING`, sc.Brand, sc.Tenant, idgen.MustNewV7(), attemptID, kind, invoice)
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
			return errClosureManualReview
		}
		return nil
	})
}
