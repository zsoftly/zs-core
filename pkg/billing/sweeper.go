package billing

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// SweeperActor is the audit actor of the credit sweeper.
var SweeperActor = events.Actor{Type: events.ActorSystem}

// Retry backoff of a credit whose expiry failed: 10 seconds after the first
// failure, doubling up to a day.
const (
	sweepBackoffFirst = 10 * time.Second
	sweepBackoffMax   = 24 * time.Hour
)

// CreditSweeper expires the credits Core issued. Kill Bill does not expire
// credit, so for each credit past its expiry the sweeper writes a Kill Bill
// external charge for the unused remainder, which Kill Bill pays from the
// account credit, and records it. One pass runs at a time across every Core
// instance, through a database advisory lock held for the pass on its own
// connection. Every row is processed in its own short transactions.
//
// Kill Bill reports one credit balance per account, not per credit. The
// sweeper assumes credits are used oldest first, and that account credit
// that did not come from Core is used last, so it is never reversed. The
// amount reversed for an expired credit is the least of:
//
//   - the credit's own amount, less what was already reversed
//   - the account credit, read from Kill Bill just before the charge
//   - the account credit, less the unexpired Core credits issued after it,
//     less the credit that did not come from Core
//
// The credit that did not come from Core is the larger of the account
// credit less every unexpired Core credit of the tenant, and the credit
// Kill Bill added from other sources that the account still holds, and
// never less than zero. Pending credits count as unexpired, and a tenant
// with a pending credit is skipped until it completes. When nothing is left
// to reverse, the credit is marked expired with nothing reversed.
type CreditSweeper struct {
	Service *Service
	Log     *slog.Logger
	// Batch is how many due credits one page reads. Zero means 100.
	Batch int
}

// CreditSweepReport counts what one pass did.
type CreditSweepReport struct {
	Skipped       bool
	Expired       int
	ReversedMinor int64
	// Deferred counts credits left for a later pass without a failure: the
	// tenant has a pending credit, or its brand is not active.
	Deferred int
	Errors   int
	// NotificationsDeleted counts push notification receipts older than
	// NotificationRetention that the pass deleted.
	NotificationsDeleted int64
}

func (c *CreditSweeper) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

// Run sweeps every interval until ctx ends.
func (c *CreditSweeper) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := c.SweepOnce(ctx)
		if err != nil {
			c.log().Error("credit sweep failed", "error", err.Error())
		} else if !report.Skipped && (report.Expired > 0 || report.Errors > 0) {
			c.log().Info("credit sweep", "expired", report.Expired, "reversed_minor", report.ReversedMinor,
				"deferred", report.Deferred, "errors", report.Errors)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// dueCredit is a credit past its expiry, from either table.
type dueCredit struct {
	creditRow
	brandID, tenantID idgen.UUID
	attempts          int
}

// sweepLockKey is the advisory lock of a pass.
const sweepLockKey = `hashtextextended('zs-core.credit.sweep', 0)`

// errSweepDeferred marks a credit left for a later pass without a failure.
var errSweepDeferred = errors.New("credit expiry deferred")

// SweepOnce runs one pass. It returns Skipped when another pass holds the
// lock.
func (c *CreditSweeper) SweepOnce(ctx context.Context) (CreditSweepReport, error) {
	var report CreditSweepReport
	s := c.Service
	if !s.Ready() {
		return report, errNotReady
	}
	pool, ok := s.DB.(interface {
		Acquire(context.Context) (*pgxpool.Conn, error)
	})
	if !ok {
		return report, zserrors.New(zserrors.CodeNotReady, "The credit sweeper needs a connection pool.")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return report, unavailable(err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+sweepLockKey+`)`).Scan(&locked); err != nil {
		conn.Release()
		return report, unavailable(err)
	}
	if !locked {
		conn.Release()
		report.Skipped = true
		return report, nil
	}
	defer func() {
		// A connection that still holds the lock never goes back to the
		// pool.
		release := context.WithoutCancel(ctx)
		if _, err := conn.Exec(release, `SELECT pg_advisory_unlock(`+sweepLockKey+`)`); err != nil {
			_ = conn.Hijack().Close(release)
			return
		}
		conn.Release()
	}()

	tag, err := s.DB.Exec(ctx, `DELETE FROM killbill_notifications WHERE received_at < $1`, s.now().Add(-NotificationRetention))
	if err != nil {
		return report, unavailable(err)
	}
	report.NotificationsDeleted = tag.RowsAffected()

	batch := c.Batch
	if batch <= 0 {
		batch = 100
	}
	correlationID := idgen.MustNewV7().String()
	// Pages walk the due credits oldest first, past the ones a pass defers,
	// so a deferred credit never holds up the others.
	var after struct {
		at time.Time
		id idgen.UUID
	}
	for {
		due, err := c.duePage(ctx, after.at, after.id, batch)
		if err != nil {
			return report, err
		}
		for _, d := range due {
			after.at, after.id = d.createdAt, d.id
			reversed, err := c.expire(ctx, d, correlationID)
			switch {
			case errors.Is(err, errSweepDeferred):
				report.Deferred++
			case err != nil:
				report.Errors++
				c.log().Error("credit expiry failed", "credit_id", idgen.Format(idgen.Credit, d.id),
					"tenant_id", idgen.Format(idgen.Tenant, d.tenantID), "error", err.Error())
				if err := c.retryLater(ctx, d); err != nil {
					return report, err
				}
			default:
				report.Expired++
				report.ReversedMinor += reversed
			}
		}
		if len(due) < batch {
			return report, nil
		}
	}
}

// duePage returns the next page of credits past their expiry and due for
// an attempt, oldest first, after the given position.
func (c *CreditSweeper) duePage(ctx context.Context, afterAt time.Time, afterID idgen.UUID, batch int) ([]dueCredit, error) {
	s := c.Service
	rows, err := s.DB.Query(ctx, `SELECT * FROM (
SELECT 'credit_code_redemptions' AS source, id, brand_id, tenant_id, amount_minor, currency, expires_at, reversed_minor, created_at, attempts
FROM credit_code_redemptions WHERE status = 'completed' AND expires_at IS NOT NULL AND expired_at IS NULL AND expires_at <= $1
    AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
UNION ALL
SELECT 'staff_credits', id, brand_id, tenant_id, amount_minor, currency, expires_at, reversed_minor, created_at, attempts
FROM staff_credits WHERE status = 'completed' AND expires_at IS NOT NULL AND expired_at IS NULL AND expires_at <= $1
    AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
) due WHERE (created_at, id) > ($2, $3) ORDER BY created_at, id LIMIT $4`, s.now(), afterAt, afterID, batch)
	if err != nil {
		return nil, unavailable(err)
	}
	due, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dueCredit, error) {
		var d dueCredit
		err := r.Scan(&d.table, &d.id, &d.brandID, &d.tenantID, &d.amountMinor, &d.currency, &d.expiresAt, &d.reversedMinor, &d.createdAt, &d.attempts)
		return d, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return due, nil
}

// expire reverses the unused part of one expired credit and records it. It
// returns errSweepDeferred when the credit waits for a later pass.
func (c *CreditSweeper) expire(ctx context.Context, d dueCredit, correlationID string) (int64, error) {
	s := c.Service
	sc := scope.Scope{Brand: d.brandID, Tenant: d.tenantID}
	call := Call{Actor: SweeperActor, Reason: "credit expiry", CorrelationID: correlationID}
	rec, err := brand.Get(ctx, s.DB, d.brandID)
	if err != nil {
		return 0, err
	}
	if rec.Status != brand.StatusActive || rec.AppliedAt.IsZero() {
		return 0, errSweepDeferred
	}
	t, err := tenant.Get(ctx, s.DB, sc)
	if err != nil {
		return 0, err
	}
	if t.Status == tenant.StatusClosed {
		// Closing the account settled it in Kill Bill. Nothing is left to
		// reverse.
		return 0, c.record(ctx, sc, d, 0, "", call)
	}
	pending, err := hasPendingCredit(ctx, s.DB, sc)
	if err != nil {
		return 0, err
	}
	if pending {
		// A pending credit may already be in Kill Bill's account credit.
		return 0, errSweepDeferred
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return 0, err
	}
	desc := reversalDescription(d.id)
	invoices, err := s.KillBill.AccountInvoices(ctx, a.creds, a.id(), true)
	if err != nil {
		return 0, kbError(err)
	}
	// A charge an earlier attempt wrote is recorded as it is, never
	// written again or recomputed.
	for _, in := range invoices {
		for _, it := range in.Items {
			if it.ItemType == "EXTERNAL_CHARGE" && it.Description == desc {
				charged, err := ToMinor(it.Amount, d.currency)
				if err != nil {
					return 0, kbError(err)
				}
				return charged, c.record(ctx, sc, d, charged, it.InvoiceItemID, call)
			}
		}
	}
	added, err := nonCoreCreditAdded(invoices, d.currency)
	if err != nil {
		return 0, err
	}
	acct, err := s.KillBill.AccountWithBalance(ctx, a.creds, a.id())
	if err != nil {
		return 0, kbError(err)
	}
	cba, err := ToMinor(acct.AccountCBA, acct.Currency)
	if err != nil {
		return 0, kbError(err)
	}
	live, err := liveCredits(ctx, s.DB, sc, acct.Currency)
	if err != nil {
		return 0, err
	}
	unused := int64(0)
	if d.currency == acct.Currency {
		unused = reversible(d, cba, live, added)
	}
	if unused <= 0 {
		return 0, c.record(ctx, sc, d, 0, "", call)
	}
	amount, err := FromMinor(unused, d.currency)
	if err != nil {
		return 0, err
	}
	charge, err := s.KillBill.CreateExternalCharge(ctx, a.creds, call.kb(), killbillItem(a.id(), amount, d.currency, desc))
	if err != nil {
		return 0, kbError(err)
	}
	return unused, c.record(ctx, sc, d, unused, charge.InvoiceItemID, call)
}

// reversible is the part of an expired credit the sweeper reverses, given
// the account credit in Kill Bill, the tenant's unexpired Core credits, the
// expired one included, and the credit Kill Bill added from other sources.
// The credit that did not come from Core is the larger of two estimates:
// the account credit beyond every unexpired Core credit, and the credit
// from other sources that the account still holds when it is used last. See
// CreditSweeper.
func reversible(d dueCredit, cba int64, live []liveCredit, nonCoreAdded int64) int64 {
	var newer, core int64
	for _, l := range live {
		core += l.remaining
		if l.id != d.id && (l.createdAt.After(d.createdAt) || (l.createdAt.Equal(d.createdAt) && l.id.String() > d.id.String())) {
			newer += l.remaining
		}
	}
	nonCore := max(0, cba-core, min(cba, nonCoreAdded))
	return min(d.amountMinor-d.reversedMinor, cba, cba-newer-nonCore)
}

// nonCoreCreditAdded is the account credit Kill Bill added from sources
// other than Core: every credit added to the account balance, a positive
// CBA_ADJ, less the credits Core wrote, whose CREDIT_ADJ names a Core
// credit. An overpayment or another party's credit counts here.
func nonCoreCreditAdded(invoices []killbill.Invoice, currency string) (int64, error) {
	var added, core int64
	for _, in := range invoices {
		for _, it := range in.Items {
			amount, err := ToMinor(it.Amount, currency)
			if err != nil {
				return 0, kbError(err)
			}
			switch {
			case it.ItemType == "CBA_ADJ" && amount > 0:
				added += amount
			case it.ItemType == "CREDIT_ADJ" && strings.HasPrefix(it.Description, creditDescriptionPrefix):
				core += max(amount, -amount)
			}
		}
	}
	return max(0, added-core), nil
}

// retryLater counts a failed attempt and sets when the next one is due.
func (c *CreditSweeper) retryLater(ctx context.Context, d dueCredit) error {
	s := c.Service
	args, err := scope.Scope{Brand: d.brandID, Tenant: d.tenantID}.Bind(d.id, s.now().Add(sweepBackoff(d.attempts+1)))
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(context.WithoutCancel(ctx), `UPDATE `+d.table+` SET attempts = attempts + 1, next_attempt_at = $4
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND expired_at IS NULL`, args...); err != nil {
		return unavailable(err)
	}
	return nil
}

// sweepBackoff is the wait after the given number of failed attempts.
func sweepBackoff(attempts int) time.Duration {
	wait := sweepBackoffFirst
	for i := 1; i < attempts && wait < sweepBackoffMax; i++ {
		wait *= 2
	}
	return min(wait, sweepBackoffMax)
}

// record marks a credit expired with the part reversed, and audits it when
// this pass made the change.
func (c *CreditSweeper) record(ctx context.Context, sc scope.Scope, d dueCredit, reversed int64, chargeID string, call Call) error {
	args, err := sc.Bind(d.id, reversed, nullable(chargeID))
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	return inTx(ctx, c.Service.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE `+d.table+` SET expired_at = now(), reversed_minor = reversed_minor + $4,
    reversal_killbill_id = $5::uuid, next_attempt_at = NULL WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND expired_at IS NULL`, args...)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		action := "billing.credit.expired"
		details := map[string]any{"credit_id": idgen.Format(idgen.Credit, d.id), "amount_minor": d.amountMinor,
			"reversed_minor": reversed, "currency": d.currency}
		if chargeID != "" {
			details["killbill_charge_id"] = chargeID
		}
		if reversed == 0 {
			action = "billing.credit.expired_nothing_to_reverse"
		}
		_, err = audit.Write(ctx, tx, sc, audit.Entry{
			Actor: call.Actor, Action: action, Reason: call.Reason, Outcome: audit.OutcomeSuccess,
			CorrelationID: call.CorrelationID, Details: details,
		})
		return err
	})
}

// liveCredit is an unexpired Core credit of a tenant, pending or completed.
type liveCredit struct {
	id        idgen.UUID
	remaining int64
	createdAt time.Time
}

// liveCredits returns the tenant's unexpired Core credits in currency,
// pending ones included, with the part not yet reversed.
func liveCredits(ctx context.Context, q DB, sc scope.Scope, currency string) ([]liveCredit, error) {
	args, err := sc.Bind(currency)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT id, amount_minor - reversed_minor, created_at FROM credit_code_redemptions
WHERE brand_id = $1 AND tenant_id = $2 AND currency = $3 AND expired_at IS NULL
UNION ALL
SELECT id, amount_minor - reversed_minor, created_at FROM staff_credits
WHERE brand_id = $1 AND tenant_id = $2 AND currency = $3 AND expired_at IS NULL`, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (liveCredit, error) {
		var l liveCredit
		err := r.Scan(&l.id, &l.remaining, &l.createdAt)
		return l, err
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return out, nil
}

// hasPendingCredit reports whether the tenant has a credit Core has not yet
// seen Kill Bill write.
func hasPendingCredit(ctx context.Context, q DB, sc scope.Scope) (bool, error) {
	args, err := sc.Bind()
	if err != nil {
		return false, err
	}
	var pending bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM credit_code_redemptions WHERE brand_id = $1 AND tenant_id = $2 AND status = 'pending')
    OR EXISTS (SELECT 1 FROM staff_credits WHERE brand_id = $1 AND tenant_id = $2 AND status = 'pending')`, args...).Scan(&pending); err != nil {
		return false, unavailable(err)
	}
	return pending, nil
}
