package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idempotency"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// CreditLease is how long a request holds a pending credit while it calls
// Kill Bill. A retry resumes the credit only after it.
const CreditLease = 2 * time.Minute

// MaxCreditMinor caps one credit, in minor units.
const MaxCreditMinor = 100_000_000_00

// Credit sources.
const (
	SourceCreditCode = "credit_code"
	SourceStaff      = "staff"
)

// Credit is a credit Core wrote to Kill Bill: a code redemption or a staff
// credit. KillBillCreditID is the Kill Bill credit item. ExpiresAt is when
// Core reverses the unused part; Kill Bill itself never expires credit.
type Credit struct {
	ID               string     `json:"id"`
	Source           string     `json:"source"`
	CreditCodeID     string     `json:"credit_code_id,omitempty"`
	AmountMinor      int64      `json:"amount_minor"`
	Currency         string     `json:"currency"`
	KillBillCreditID string     `json:"killbill_credit_id"`
	ExpiresAt        *time.Time `json:"expires_at"`
	ExpiredAt        *time.Time `json:"expired_at"`
	ReversedMinor    int64      `json:"reversed_minor"`
	CreatedAt        time.Time  `json:"created_at"`
}

// creditDescription is the description of the Kill Bill credit Core writes
// for a credit row. A retry after a lost answer finds the credit by it.
func creditDescription(id idgen.UUID) string {
	return creditDescriptionPrefix + strings.TrimPrefix(idgen.Format(idgen.Credit, id), "crd_")
}

// creditDescriptionPrefix starts the description of every Kill Bill credit
// Core writes.
const creditDescriptionPrefix = "zs-core credit crd_"

// reversalDescription is the description of the external charge that
// reverses the unused part of an expired credit.
func reversalDescription(id idgen.UUID) string {
	return "zs-core credit expiry " + idgen.Format(idgen.Credit, id)
}

// findItem returns the account's invoice item of itemType with the
// description, if Kill Bill holds one.
func (s *Service) findItem(ctx context.Context, a account, itemType, description string) (killbill.InvoiceItem, bool, error) {
	invoices, err := s.KillBill.AccountInvoices(ctx, a.creds, a.id(), true)
	if err != nil {
		return killbill.InvoiceItem{}, false, kbError(err)
	}
	for _, in := range invoices {
		for _, it := range in.Items {
			if it.ItemType == itemType && it.Description == description {
				return it, true, nil
			}
		}
	}
	return killbill.InvoiceItem{}, false, nil
}

// writeCredit writes the Kill Bill credit of a pending credit row, unless a
// previous attempt already did. resumed is true when the row was pending
// from an earlier request.
func (s *Service) writeCredit(ctx context.Context, a account, call Call, row creditRow, resumed bool) (killbill.InvoiceItem, error) {
	desc := creditDescription(row.id)
	if resumed {
		return s.matchOriginalCredit(ctx, a, row)
	}
	amount, err := FromMinor(row.amountMinor, row.currency)
	if err != nil {
		return killbill.InvoiceItem{}, validation("The currency is not supported.")
	}
	item, err := s.KillBill.CreateCredit(ctx, a.creds, call.kb(), killbillItem(a.id(), amount, row.currency, desc))
	if err != nil {
		return killbill.InvoiceItem{}, err
	}
	return item, nil
}

func recoveryPending() error {
	return zserrors.New(zserrors.CodeBillingUnavailable, "Credit recovery requires manual review.")
}

// reviewNeeded is the one way a recovery that cannot prove its receipt ends:
// the credit stays pending, nothing is posted, the answer is the same manual
// review for every cause, and one log line names the credit and a fixed reason
// code, never an amount or an address.
func (s *Service) reviewNeeded(row creditRow, reason string) error {
	s.log().Warn("credit recovery needs review", "credit_id", idgen.Format(idgen.Credit, row.id), "source", row.table, "reason", reason)
	return recoveryPending()
}

func (s *Service) matchOriginalCredit(ctx context.Context, a account, row creditRow) (killbill.InvoiceItem, error) {
	if row.originalEndpoint == nil || row.originalKBTenant == nil || row.originalKBAccount == nil || row.originalCurrency == nil || row.originalAmount == nil || row.originalDescription == nil {
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "target_missing")
	}
	if *row.originalKBTenant != a.brand.KillBillTenantID || *row.originalKBAccount != a.id() || *row.originalCurrency != row.currency || *row.originalAmount != row.amountMinor || *row.originalDescription != creditDescription(row.id) {
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "target_mismatch")
	}
	wantEndpoint := "grant"
	if row.table == "credit_code_redemptions" {
		wantEndpoint = "redeem"
	}
	if *row.originalEndpoint != wantEndpoint {
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "target_mismatch")
	}
	wantAmount, err := FromMinor(-row.amountMinor, row.currency)
	if err != nil {
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "target_mismatch")
	}
	items, err := s.KillBill.AccountInvoices(ctx, a.creds, a.id(), true)
	if err != nil {
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "history_unavailable")
	}
	var candidate killbill.InvoiceItem
	var invoice killbill.Invoice
	count := 0
	for _, in := range items {
		if in.AccountID != a.id() {
			return killbill.InvoiceItem{}, s.reviewNeeded(row, "history_foreign_account")
		}
		for _, item := range in.Items {
			if item.ItemType == "CREDIT_ADJ" && item.Description == *row.originalDescription {
				candidate, invoice = item, in
				count++
			}
		}
	}
	switch {
	case count == 0:
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "receipt_absent")
	case count > 1:
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "receipt_ambiguous")
	case invoice.Status != "COMMITTED":
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "receipt_not_committed")
	case invoice.Currency != row.currency || candidate.InvoiceID == "" || candidate.InvoiceID != invoice.InvoiceID || candidate.InvoiceItemID == "" ||
		candidate.AccountID != a.id() || candidate.Currency != row.currency || !sameDecimal(candidate.Amount, wantAmount):
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "receipt_mismatch")
	}
	if _, err := idgen.ParseUUID(invoice.InvoiceID); err != nil {
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "receipt_mismatch")
	}
	if _, err := idgen.ParseUUID(candidate.InvoiceItemID); err != nil {
		return killbill.InvoiceItem{}, s.reviewNeeded(row, "receipt_mismatch")
	}
	return candidate, nil
}

// creditCompletionError maps the refusal of the database to put a receipt on a
// second credit to the manual review the recovery ends in, and anything else
// to a database failure.
func creditCompletionError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return recoveryPending()
	}
	return unavailable(err)
}

func sameDecimal(got, want json.Number) bool {
	g, ok := creditDecimal(got)
	if !ok {
		return false
	}
	w, ok := creditDecimal(want)
	return ok && g.Cmp(w) == 0
}

var creditDecimalPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

func creditDecimal(v json.Number) (*big.Rat, bool) {
	if len(v) == 0 || len(v) > 256 || !creditDecimalPattern.MatchString(string(v)) {
		return nil, false
	}
	if i := strings.IndexAny(string(v), "eE"); i >= 0 {
		exponent := string(v)[i+1:]
		if exponent[0] == '+' || exponent[0] == '-' {
			exponent = exponent[1:]
		}
		value := 0
		for _, digit := range exponent {
			value = value*10 + int(digit-'0')
			if value > 512 {
				return nil, false
			}
		}
	}
	r, ok := new(big.Rat).SetString(string(v))
	return r, ok
}

func killbillItem(accountID string, amount json.Number, currency, description string) killbill.InvoiceItem {
	return killbill.InvoiceItem{AccountID: accountID, Amount: amount, Currency: currency, Description: description}
}

// rejected reports whether Kill Bill refused a write, so it certainly did
// not happen.
func rejected(err error) bool {
	var kbErr *killbill.Error
	return errors.As(err, &kbErr) || errors.Is(err, killbill.ErrNotFound) || errors.Is(err, killbill.ErrConflict)
}

// CreditCode is a promo or voucher code of a brand. The code itself is
// never stored: only a digest and its first characters, Hint.
type CreditCode struct {
	ID              string     `json:"id"`
	Hint            string     `json:"hint"`
	AmountMinor     int64      `json:"amount_minor"`
	Currency        string     `json:"currency"`
	MaxRedemptions  *int       `json:"max_redemptions"`
	PerTenantLimit  int        `json:"per_tenant_limit"`
	Redemptions     int        `json:"redemptions"`
	ExpiresAt       *time.Time `json:"expires_at"`
	CreditValidDays *int       `json:"credit_valid_days"`
	Reason          string     `json:"reason"`
	Status          string     `json:"status"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	DisabledAt      *time.Time `json:"disabled_at"`
}

// Credit code statuses.
const (
	CodeActive   = "active"
	CodeDisabled = "disabled"
)

var codePattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{3,63}$`)

// NormalizeCode trims a code and puts it in upper case. Codes are 4 to 64
// letters, digits and dashes and compare without case.
func NormalizeCode(code string) (string, bool) {
	c := strings.ToUpper(strings.TrimSpace(code))
	return c, codePattern.MatchString(c)
}

// CodeDigest is the stored digest of a normalized code in a brand. The
// brand is part of it, so one code in two brands has two digests.
func CodeDigest(brandID idgen.UUID, normalized string) []byte {
	h := sha256.New()
	h.Write([]byte("zs-core.credit-code.v1\x00"))
	h.Write([]byte(brandID.String()))
	h.Write([]byte{0})
	h.Write([]byte(normalized))
	return h.Sum(nil)
}

// NewCreditCode describes a code to add. PerTenantLimit defaults to 1.
type NewCreditCode struct {
	Code            string
	AmountMinor     int64
	Currency        string
	MaxRedemptions  *int
	PerTenantLimit  int
	ExpiresAt       *time.Time
	CreditValidDays *int
	Reason          string
	CreatedBy       string
}

const codeColumns = `id, hint, amount_minor, currency, max_redemptions, per_tenant_limit, redemptions, expires_at,
       credit_valid_days, reason, status, created_by, created_at, disabled_at`

func scanCode(row pgx.Row) (CreditCode, error) {
	var c CreditCode
	var id idgen.UUID
	err := row.Scan(&id, &c.Hint, &c.AmountMinor, &c.Currency, &c.MaxRedemptions, &c.PerTenantLimit, &c.Redemptions,
		&c.ExpiresAt, &c.CreditValidDays, &c.Reason, &c.Status, &c.CreatedBy, &c.CreatedAt, &c.DisabledAt)
	c.ID = idgen.Format(idgen.CreditCode, id)
	return c, err
}

// AddCreditCode adds a promo or voucher code to a brand in tx and audits it
// as a brand event. The currency must be one of the brand's currencies. A
// code the brand already has is conflict.
func AddCreditCode(ctx context.Context, tx pgx.Tx, brandID idgen.UUID, req NewCreditCode, actor events.Actor, correlationID string) (CreditCode, error) {
	code, ok := NormalizeCode(req.Code)
	if !ok {
		return CreditCode{}, validation("The code must be 4 to 64 letters, digits and dashes, starting with a letter or digit.")
	}
	if req.AmountMinor <= 0 || req.AmountMinor > MaxCreditMinor {
		return CreditCode{}, validation("The amount must be positive and at most 10000000000 minor units.")
	}
	if err := checkCurrency(req.Currency); err != nil {
		return CreditCode{}, err
	}
	if req.PerTenantLimit == 0 {
		req.PerTenantLimit = 1
	}
	if req.PerTenantLimit < 0 || (req.MaxRedemptions != nil && *req.MaxRedemptions <= 0) {
		return CreditCode{}, validation("The redemption limits must be positive.")
	}
	if req.CreditValidDays != nil && (*req.CreditValidDays < 1 || *req.CreditValidDays > 3660) {
		return CreditCode{}, validation("The credit validity must be 1 to 3660 days.")
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		return CreditCode{}, validation("The code expiry must be in the future.")
	}
	if err := checkReason(req.Reason); err != nil {
		return CreditCode{}, err
	}
	if strings.TrimSpace(req.CreatedBy) == "" || len(req.CreatedBy) > 255 {
		return CreditCode{}, validation("The creator is required.")
	}
	rec, err := brand.Get(ctx, tx, brandID)
	if err != nil {
		return CreditCode{}, err
	}
	if rec.Manifest == nil || !containsString(rec.Manifest.Billing.Currencies, req.Currency) {
		return CreditCode{}, validation("The currency must be one of the brand's billing currencies.")
	}
	id := idgen.MustNewV7()
	hint := code[:4]
	c, err := scanCode(tx.QueryRow(ctx, `INSERT INTO credit_codes (id, brand_id, code_digest, hint, amount_minor, currency,
    max_redemptions, per_tenant_limit, expires_at, credit_valid_days, reason, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (brand_id, code_digest) DO NOTHING
RETURNING `+codeColumns, id, brandID, CodeDigest(brandID, code), hint, req.AmountMinor, req.Currency,
		req.MaxRedemptions, req.PerTenantLimit, req.ExpiresAt, req.CreditValidDays, strings.TrimSpace(req.Reason), req.CreatedBy))
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditCode{}, zserrors.New(zserrors.CodeConflict, "The brand already has this code.")
	}
	if err != nil {
		return CreditCode{}, unavailable(err)
	}
	_, err = audit.WriteBrand(ctx, tx, brandID, audit.Entry{
		Actor: actor, Action: "billing.credit_code.created", Reason: strings.TrimSpace(req.Reason), Outcome: audit.OutcomeSuccess,
		CorrelationID: correlationID, Details: map[string]any{"credit_code_id": c.ID, "hint": c.Hint,
			"amount_minor": c.AmountMinor, "currency": c.Currency},
	})
	return c, err
}

// ListCreditCodes returns the codes of a brand, newest first.
func ListCreditCodes(ctx context.Context, q rowsQuerier, brandID idgen.UUID) ([]CreditCode, error) {
	rows, err := q.Query(ctx, `SELECT `+codeColumns+` FROM credit_codes WHERE brand_id = $1 ORDER BY created_at DESC, id DESC`, brandID)
	if err != nil {
		return nil, unavailable(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CreditCode, error) { return scanCode(r) })
	if err != nil {
		return nil, unavailable(err)
	}
	if out == nil {
		out = []CreditCode{}
	}
	return out, nil
}

type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// DisableCreditCode disables a code of a brand in tx. Credits already
// redeemed stay. Disabling a disabled code is a no-op.
func DisableCreditCode(ctx context.Context, tx pgx.Tx, brandID idgen.UUID, codeID string, reason string, actor events.Actor, correlationID string) (CreditCode, error) {
	if err := checkReason(reason); err != nil {
		return CreditCode{}, err
	}
	id, err := idgen.ParseAs(idgen.CreditCode, codeID)
	if err != nil {
		return CreditCode{}, zserrors.New(zserrors.CodeNotFound, "The credit code was not found.")
	}
	c, err := scanCode(tx.QueryRow(ctx, `UPDATE credit_codes SET status = 'disabled', disabled_at = coalesce(disabled_at, now()), updated_at = now()
WHERE brand_id = $1 AND id = $2 RETURNING `+codeColumns, brandID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditCode{}, zserrors.New(zserrors.CodeNotFound, "The credit code was not found.")
	}
	if err != nil {
		return CreditCode{}, unavailable(err)
	}
	_, err = audit.WriteBrand(ctx, tx, brandID, audit.Entry{
		Actor: actor, Action: "billing.credit_code.disabled", Reason: strings.TrimSpace(reason), Outcome: audit.OutcomeSuccess,
		CorrelationID: correlationID, Details: map[string]any{"credit_code_id": c.ID, "hint": c.Hint},
	})
	return c, err
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// creditRow is a pending or completed credit of either table.
type creditRow struct {
	table               string
	id                  idgen.UUID
	codeID              idgen.UUID
	status              string
	claimedUntil        *time.Time
	amountMinor         int64
	currency            string
	kbCreditID          *string
	expiresAt           *time.Time
	expiredAt           *time.Time
	reversedMinor       int64
	createdAt           time.Time
	fingerprint         string
	originalEndpoint    *string
	originalKBTenant    *string
	originalKBAccount   *string
	originalCurrency    *string
	originalAmount      *int64
	originalDescription *string
}

func (r creditRow) credit() Credit {
	c := Credit{ID: idgen.Format(idgen.Credit, r.id), Source: SourceStaff, AmountMinor: r.amountMinor, Currency: r.currency,
		ExpiresAt: r.expiresAt, ExpiredAt: r.expiredAt, ReversedMinor: r.reversedMinor, CreatedAt: r.createdAt.UTC()}
	if r.table == "credit_code_redemptions" {
		c.Source, c.CreditCodeID = SourceCreditCode, idgen.Format(idgen.CreditCode, r.codeID)
	}
	if r.kbCreditID != nil {
		c.KillBillCreditID = *r.kbCreditID
	}
	return c
}

const redemptionColumns = `id, credit_code_id, status, claimed_until, amount_minor, currency, killbill_credit_id::text, expires_at,
       expired_at, reversed_minor, created_at, original_endpoint_kind, original_killbill_tenant_id::text, original_killbill_account_id::text,
       original_currency, original_amount_minor, original_description`

func scanRedemption(row pgx.Row) (creditRow, error) {
	r := creditRow{table: "credit_code_redemptions"}
	err := row.Scan(&r.id, &r.codeID, &r.status, &r.claimedUntil, &r.amountMinor, &r.currency, &r.kbCreditID, &r.expiresAt,
		&r.expiredAt, &r.reversedMinor, &r.createdAt, &r.originalEndpoint, &r.originalKBTenant, &r.originalKBAccount,
		&r.originalCurrency, &r.originalAmount, &r.originalDescription)
	return r, err
}

// Redeem redeems a promo or voucher code for the tenant: one Kill Bill
// account credit of the code's amount with the code as the reason. It is
// idempotent per tenant and code: once the tenant has used its
// per-tenant limit of the code, a redemption returns the latest credit with
// created false. Another tenant can redeem until the code's cap. An
// unknown, disabled or expired code, or a code of another brand or
// currency, is credit_code_invalid; a code at its cap is
// credit_code_exhausted. While billing is on hold for the tenant it is
// wallet_on_hold.
func (s *Service) Redeem(ctx context.Context, sc scope.Scope, call Call, code string) (Credit, bool, error) {
	if err := call.validate(); err != nil {
		return Credit{}, false, err
	}
	norm, ok := NormalizeCode(code)
	if !ok {
		return Credit{}, false, zserrors.New(zserrors.CodeCreditCodeInvalid, "")
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return Credit{}, false, err
	}
	if a.tenant.Status != tenant.StatusActive {
		return Credit{}, false, zserrors.New(zserrors.CodeWalletOnHold, "Billing changes are on hold for this tenant: tenant_suspended.")
	}
	now := s.now()
	var (
		row          creditRow
		resume, done bool
		validDays    *int
	)
	args, err := sc.Bind()
	if err != nil {
		return Credit{}, false, err
	}
	err = inTx(ctx, s.DB, func(tx pgx.Tx) error {
		var (
			codeID                 idgen.UUID
			amount                 int64
			currency, status       string
			maxRedemptions         *int
			perTenant, redemptions int
			expiresAt              *time.Time
		)
		err := tx.QueryRow(ctx, `SELECT id, amount_minor, currency, max_redemptions, per_tenant_limit, redemptions, expires_at,
       credit_valid_days, status
FROM credit_codes WHERE brand_id = $1 AND code_digest = $2 FOR UPDATE`, sc.Brand, CodeDigest(sc.Brand, norm)).Scan(
			&codeID, &amount, &currency, &maxRedemptions, &perTenant, &redemptions, &expiresAt, &validDays, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return zserrors.New(zserrors.CodeCreditCodeInvalid, "")
		}
		if err != nil {
			return unavailable(err)
		}
		if status != CodeActive || (expiresAt != nil && !expiresAt.After(now)) {
			return zserrors.New(zserrors.CodeCreditCodeInvalid, "")
		}
		if currency != a.currency() {
			return zserrors.New(zserrors.CodeCreditCodeInvalid, "This code is for another currency.")
		}
		var used int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM credit_code_redemptions WHERE brand_id = $1 AND tenant_id = $2 AND credit_code_id = $3`,
			append(args, codeID)...).Scan(&used); err != nil {
			return unavailable(err)
		}
		if used >= perTenant {
			latest, err := scanRedemption(tx.QueryRow(ctx, `SELECT `+redemptionColumns+` FROM credit_code_redemptions
WHERE brand_id = $1 AND tenant_id = $2 AND credit_code_id = $3 ORDER BY created_at DESC, id DESC LIMIT 1 FOR UPDATE`, append(args, codeID)...))
			if err != nil {
				return unavailable(err)
			}
			row = latest
			if latest.status == "completed" {
				done = true
				return nil
			}
			if latest.claimedUntil != nil && latest.claimedUntil.After(now) {
				return zserrors.New(zserrors.CodeIdempotencyKeyInProgress, "This code is being redeemed for the tenant. Try again shortly.")
			}
			resume = true
			if _, err := tx.Exec(ctx, `UPDATE credit_code_redemptions SET claimed_until = $4 WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`,
				append(args, latest.id, now.Add(CreditLease))...); err != nil {
				return unavailable(err)
			}
			return nil
		}
		if maxRedemptions != nil && redemptions >= *maxRedemptions {
			return zserrors.New(zserrors.CodeCreditCodeExhausted, "")
		}
		// The credit's expiry runs from the first attempt, so a retry
		// after a lost answer never extends it.
		var expires *time.Time
		if validDays != nil {
			e := now.AddDate(0, 0, *validDays)
			expires = &e
		}
		row = creditRow{table: "credit_code_redemptions", id: idgen.MustNewV7(), codeID: codeID, status: "pending",
			amountMinor: amount, currency: currency, expiresAt: expires, createdAt: now}
		if _, err := tx.Exec(ctx, `INSERT INTO credit_code_redemptions (id, brand_id, tenant_id, credit_code_id, claimed_until,
    amount_minor, currency, expires_at, actor_type, actor_id, correlation_id, created_at,
    original_endpoint_kind, original_killbill_tenant_id, original_killbill_account_id, original_currency, original_amount_minor, original_description)
VALUES ($3, $1, $2, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'redeem', $13::uuid, $14::uuid, $15, $16, $17)`, append(args, row.id, codeID, now.Add(CreditLease), amount, currency, expires,
			string(call.Actor.Type), nullable(call.Actor.ID), call.CorrelationID, now, a.brand.KillBillTenantID, a.id(), currency, amount, creditDescription(row.id))...); err != nil {
			return unavailable(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE credit_codes SET redemptions = redemptions + 1, updated_at = now() WHERE id = $1`, codeID); err != nil {
			return unavailable(err)
		}
		return nil
	})
	if err != nil {
		return Credit{}, false, err
	}
	if done {
		return row.credit(), false, nil
	}
	if resume && row.originalEndpoint == nil {
		err := zserrors.New(zserrors.CodeBillingUnavailable, "Credit recovery requires manual review.")
		s.releaseCredit(ctx, sc, row, err)
		return Credit{}, false, err
	}
	kbCall := call
	kbCall.Reason = norm
	item, err := s.writeCredit(ctx, a, kbCall, row, resume)
	if err != nil {
		cleanupCause := err
		if resume {
			cleanupCause = recoveryPending()
		}
		s.releaseCredit(ctx, sc, row, cleanupCause)
		return Credit{}, false, kbError(err)
	}
	err = inTx(ctx, s.DB, func(tx pgx.Tx) error {
		completed, err := scanRedemption(tx.QueryRow(ctx, `UPDATE credit_code_redemptions SET status = 'completed', claimed_until = NULL,
    killbill_credit_id = $4::uuid, killbill_invoice_id = $5::uuid, completed_at = now()
	WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' RETURNING `+redemptionColumns, append(args, row.id, item.InvoiceItemID, nullable(item.InvoiceID))...))
		if errors.Is(err, pgx.ErrNoRows) {
			current, currentErr := scanRedemption(tx.QueryRow(ctx, `SELECT `+redemptionColumns+` FROM credit_code_redemptions
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, append(args, row.id)...))
			if currentErr != nil {
				return unavailable(currentErr)
			}
			if current.status != "completed" {
				return recoveryPending()
			}
			row, done = current, true
			return nil
		}
		if err != nil {
			return creditCompletionError(err)
		}
		row = completed
		if err := BumpWalletVersion(ctx, tx, sc); err != nil {
			return err
		}
		_, err = audit.Write(ctx, tx, sc, audit.Entry{
			Actor: call.Actor, Action: "billing.credit.redeemed", Outcome: audit.OutcomeSuccess, CorrelationID: call.CorrelationID,
			Details: map[string]any{"credit_id": idgen.Format(idgen.Credit, row.id), "credit_code_id": idgen.Format(idgen.CreditCode, row.codeID),
				"amount_minor": row.amountMinor, "currency": row.currency, "killbill_credit_id": item.InvoiceItemID},
		})
		return err
	})
	if err != nil {
		return Credit{}, false, err
	}
	if done {
		return row.credit(), false, nil
	}
	return row.credit(), true, nil
}

// releaseCredit undoes a pending credit row after Kill Bill refused the
// credit, so the code is not spent. When Kill Bill could not be reached the
// credit may exist, so the row stays pending with its claim ended and the
// next request resumes it.
func (s *Service) releaseCredit(ctx context.Context, sc scope.Scope, row creditRow, cause error) {
	ctx = context.WithoutCancel(ctx)
	args, err := sc.Bind(row.id)
	if err != nil {
		return
	}
	err = inTx(ctx, s.DB, func(tx pgx.Tx) error {
		if !rejected(cause) {
			_, err := tx.Exec(ctx, `UPDATE `+row.table+` SET claimed_until = NULL WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending'`, args...)
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM `+row.table+` WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending'`, args...)
		if err != nil || tag.RowsAffected() == 0 || row.table != "credit_code_redemptions" {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE credit_codes SET redemptions = redemptions - 1, updated_at = now() WHERE id = $1 AND redemptions > 0`, row.codeID)
		return err
	})
	if err != nil {
		s.log().ErrorContext(ctx, "pending credit not released", "credit_id", idgen.Format(idgen.Credit, row.id), "error", err.Error())
	}
}

// StaffCredit is a credit staff grant a tenant. Its currency must be the
// account currency. With ExpiresAt set, the sweeper reverses the unused part
// at that time.
type StaffCredit struct {
	AmountMinor    int64
	Currency       string
	ExpiresAt      *time.Time
	IdempotencyKey string
}

const staffCreditColumns = `id, status, claimed_until, amount_minor, currency, killbill_credit_id::text, expires_at, expired_at,
       reversed_minor, created_at, fingerprint, original_endpoint_kind, original_killbill_tenant_id::text, original_killbill_account_id::text,
       original_currency, original_amount_minor, original_description`

func scanStaffCredit(row pgx.Row) (creditRow, error) {
	r := creditRow{table: "staff_credits"}
	err := row.Scan(&r.id, &r.status, &r.claimedUntil, &r.amountMinor, &r.currency, &r.kbCreditID, &r.expiresAt, &r.expiredAt,
		&r.reversedMinor, &r.createdAt, &r.fingerprint, &r.originalEndpoint, &r.originalKBTenant, &r.originalKBAccount,
		&r.originalCurrency, &r.originalAmount, &r.originalDescription)
	return r, err
}

// GrantCredit writes a Kill Bill credit to the tenant for staff, with the
// call's reason, which is required. It is idempotent on the idempotency key
// of the staff actor in the tenant: the same key and request return the same
// credit with created false, a different request is
// idempotency_key_conflict.
func (s *Service) GrantCredit(ctx context.Context, sc scope.Scope, call Call, req StaffCredit) (Credit, bool, error) {
	if err := call.validate(); err != nil {
		return Credit{}, false, err
	}
	if err := checkReason(call.Reason); err != nil {
		return Credit{}, false, err
	}
	if err := idempotency.ValidateKey(req.IdempotencyKey); err != nil {
		return Credit{}, false, err
	}
	if req.AmountMinor <= 0 || req.AmountMinor > MaxCreditMinor {
		return Credit{}, false, validation("The amount_minor must be positive and at most 10000000000.")
	}
	if err := checkCurrency(req.Currency); err != nil {
		return Credit{}, false, err
	}
	now := s.now()
	if req.ExpiresAt != nil {
		// The database keeps microseconds; the fingerprint must match a
		// retry that sends back the stored time.
		e := req.ExpiresAt.UTC().Truncate(time.Microsecond)
		req.ExpiresAt = &e
	}
	if req.ExpiresAt != nil && (!req.ExpiresAt.After(now) || req.ExpiresAt.After(now.AddDate(10, 0, 0))) {
		return Credit{}, false, validation("The expires_at must be in the future and within 10 years.")
	}
	a, err := s.account(ctx, sc)
	if err != nil {
		return Credit{}, false, err
	}
	if req.Currency != a.currency() {
		return Credit{}, false, validation("The currency must be the tenant's billing currency, " + a.currency() + ".")
	}
	payload, err := json.Marshal(map[string]any{"amount_minor": req.AmountMinor, "currency": req.Currency, "reason": strings.TrimSpace(call.Reason), "expires_at": req.ExpiresAt})
	if err != nil {
		return Credit{}, false, err
	}
	fingerprint, err := idempotency.Fingerprint("billing.credit.grant", payload)
	if err != nil {
		return Credit{}, false, err
	}
	args, err := sc.Bind()
	if err != nil {
		return Credit{}, false, err
	}
	var (
		row          creditRow
		resume, done bool
	)
	err = inTx(ctx, s.DB, func(tx pgx.Tx) error {
		newID := idgen.MustNewV7()
		tag, err := tx.Exec(ctx, `INSERT INTO staff_credits (id, brand_id, tenant_id, idempotency_key, fingerprint, claimed_until,
    amount_minor, currency, reason, expires_at, actor_type, actor_id, correlation_id, created_at,
    original_endpoint_kind, original_killbill_tenant_id, original_killbill_account_id, original_currency, original_amount_minor, original_description)
VALUES ($3, $1, $2, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 'grant', $15::uuid, $16::uuid, $17, $18, $19)
ON CONFLICT (brand_id, tenant_id, actor_id, idempotency_key) DO NOTHING`, append(args, newID, req.IdempotencyKey, fingerprint,
			now.Add(CreditLease), req.AmountMinor, req.Currency, strings.TrimSpace(call.Reason), req.ExpiresAt,
			string(call.Actor.Type), call.Actor.ID, call.CorrelationID, now, a.brand.KillBillTenantID, a.id(), req.Currency, req.AmountMinor, creditDescription(newID))...)
		if err != nil {
			return unavailable(err)
		}
		if tag.RowsAffected() == 1 {
			row = creditRow{table: "staff_credits", id: newID, status: "pending", amountMinor: req.AmountMinor, currency: req.Currency,
				expiresAt: req.ExpiresAt, createdAt: now}
			return nil
		}
		existing, err := scanStaffCredit(tx.QueryRow(ctx, `SELECT `+staffCreditColumns+` FROM staff_credits
WHERE brand_id = $1 AND tenant_id = $2 AND actor_id = $3 AND idempotency_key = $4 FOR UPDATE`, append(args, call.Actor.ID, req.IdempotencyKey)...))
		if err != nil {
			return unavailable(err)
		}
		row = existing
		switch {
		case existing.fingerprint != fingerprint:
			return zserrors.New(zserrors.CodeIdempotencyKeyConflict, "")
		case existing.status == "completed":
			done = true
			return nil
		case existing.claimedUntil != nil && existing.claimedUntil.After(now):
			return zserrors.New(zserrors.CodeIdempotencyKeyInProgress, "")
		}
		resume = true
		_, err = tx.Exec(ctx, `UPDATE staff_credits SET claimed_until = $4 WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`,
			append(args, existing.id, now.Add(CreditLease))...)
		if err != nil {
			return unavailable(err)
		}
		return nil
	})
	if err != nil {
		return Credit{}, false, err
	}
	if done {
		return row.credit(), false, nil
	}
	if resume && row.originalEndpoint == nil {
		err := zserrors.New(zserrors.CodeBillingUnavailable, "Credit recovery requires manual review.")
		s.releaseCredit(ctx, sc, row, err)
		return Credit{}, false, err
	}
	item, err := s.writeCredit(ctx, a, call, row, resume)
	if err != nil {
		cleanupCause := err
		if resume {
			cleanupCause = recoveryPending()
		}
		s.releaseCredit(ctx, sc, row, cleanupCause)
		return Credit{}, false, kbError(err)
	}
	err = inTx(ctx, s.DB, func(tx pgx.Tx) error {
		completed, err := scanStaffCredit(tx.QueryRow(ctx, `UPDATE staff_credits SET status = 'completed', claimed_until = NULL,
    killbill_credit_id = $4::uuid, killbill_invoice_id = $5::uuid, completed_at = now()
	WHERE brand_id = $1 AND tenant_id = $2 AND id = $3 AND status = 'pending' RETURNING `+staffCreditColumns, append(args, row.id, item.InvoiceItemID, nullable(item.InvoiceID))...))
		if errors.Is(err, pgx.ErrNoRows) {
			current, currentErr := scanStaffCredit(tx.QueryRow(ctx, `SELECT `+staffCreditColumns+` FROM staff_credits
WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`, append(args, row.id)...))
			if currentErr != nil {
				return unavailable(currentErr)
			}
			if current.status != "completed" {
				return recoveryPending()
			}
			row, done = current, true
			return nil
		}
		if err != nil {
			return creditCompletionError(err)
		}
		row = completed
		if err := BumpWalletVersion(ctx, tx, sc); err != nil {
			return err
		}
		details := map[string]any{"credit_id": idgen.Format(idgen.Credit, row.id), "amount_minor": row.amountMinor,
			"currency": row.currency, "killbill_credit_id": item.InvoiceItemID}
		if row.expiresAt != nil {
			details["expires_at"] = row.expiresAt.UTC().Format(time.RFC3339)
		}
		_, err = audit.Write(ctx, tx, sc, audit.Entry{
			Actor: call.Actor, Action: "billing.credit.granted", Reason: strings.TrimSpace(call.Reason), Outcome: audit.OutcomeSuccess,
			CorrelationID: call.CorrelationID, Details: details,
		})
		return err
	})
	if err != nil {
		return Credit{}, false, err
	}
	if done {
		return row.credit(), false, nil
	}
	return row.credit(), true, nil
}
