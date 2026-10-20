package billing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/zsoftly/zs-core/internal/killbill"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// Invoice is a Kill Bill invoice of the tenant. ID is the Kill Bill invoice
// ID. Items are set on a single invoice only.
type Invoice struct {
	ID             string        `json:"id"`
	Number         string        `json:"number"`
	Date           string        `json:"date"`
	TargetDate     string        `json:"target_date"`
	Status         string        `json:"status"`
	Currency       string        `json:"currency"`
	AmountMinor    int64         `json:"amount_minor"`
	BalanceMinor   int64         `json:"balance_minor"`
	CreditAdjMinor int64         `json:"credit_adj_minor"`
	RefundAdjMinor int64         `json:"refund_adj_minor"`
	Items          []InvoiceItem `json:"items,omitempty"`
}

// InvoiceItem is a line of an invoice.
type InvoiceItem struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	Description    string `json:"description,omitempty"`
	SubscriptionID string `json:"subscription_id,omitempty"`
	Product        string `json:"product,omitempty"`
	Plan           string `json:"plan,omitempty"`
	Phase          string `json:"phase,omitempty"`
	Usage          string `json:"usage,omitempty"`
	StartDate      string `json:"start_date,omitempty"`
	EndDate        string `json:"end_date,omitempty"`
	AmountMinor    int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
}

func invoiceOf(in killbill.Invoice, withItems bool) (Invoice, error) {
	out := Invoice{ID: in.InvoiceID, Number: in.InvoiceNumber, Date: in.InvoiceDate, TargetDate: in.TargetDate,
		Status: in.Status, Currency: in.Currency}
	var err error
	for _, f := range []struct {
		v   *int64
		raw json.Number
	}{{&out.AmountMinor, in.Amount}, {&out.BalanceMinor, in.Balance}, {&out.CreditAdjMinor, in.CreditAdj}, {&out.RefundAdjMinor, in.RefundAdj}} {
		if *f.v, err = ToMinor(f.raw, in.Currency); err != nil {
			return Invoice{}, kbError(err)
		}
	}
	if !withItems {
		return out, nil
	}
	out.Items = []InvoiceItem{}
	for _, it := range in.Items {
		currency := it.Currency
		if currency == "" {
			currency = in.Currency
		}
		amount, err := ToMinor(it.Amount, currency)
		if err != nil {
			return Invoice{}, kbError(err)
		}
		out.Items = append(out.Items, InvoiceItem{ID: it.InvoiceItemID, Type: it.ItemType, Description: it.Description,
			SubscriptionID: it.SubscriptionID, Product: it.ProductName, Plan: it.PlanName, Phase: it.PhaseName,
			Usage: it.UsageName, StartDate: it.StartDate, EndDate: it.EndDate, AmountMinor: amount, Currency: currency})
	}
	return out, nil
}

// pageByNumber returns one page of items ordered by a numeric key, newest
// first, and the cursor of the next page. Kill Bill returns these lists
// whole, so Core pages them.
func pageByNumber[T any](items []T, number func(T) int64, page tenant.Page) ([]T, string, error) {
	limit := page.Limit
	switch {
	case limit == 0:
		limit = tenant.DefaultLimit
	case limit < 0 || limit > tenant.MaxLimit:
		return nil, "", zserrors.New(zserrors.CodeInvalidLimit, "")
	}
	after := int64(-1)
	if page.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(page.Cursor)
		rest, ok := strings.CutPrefix(string(raw), "n1.")
		n, convErr := strconv.ParseInt(rest, 10, 64)
		if err != nil || !ok || convErr != nil || n < 0 || len(page.Cursor) > 64 {
			return nil, "", zserrors.New(zserrors.CodeInvalidCursor, "")
		}
		after = n
	}
	sort.SliceStable(items, func(i, j int) bool { return number(items[i]) > number(items[j]) })
	out := []T{}
	for _, it := range items {
		if after >= 0 && number(it) >= after {
			continue
		}
		if len(out) == limit {
			return out, base64.RawURLEncoding.EncodeToString([]byte("n1." + strconv.FormatInt(number(out[len(out)-1]), 10))), nil
		}
		out = append(out, it)
	}
	return out, "", nil
}

func numberOf(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ListInvoices returns one page of the tenant's invoices, newest first.
func (s *Service) ListInvoices(ctx context.Context, sc scope.Scope, page tenant.Page) ([]Invoice, string, error) {
	a, err := s.account(ctx, sc)
	if err != nil {
		return nil, "", err
	}
	list, err := s.KillBill.AccountInvoices(ctx, a.creds, a.id(), false)
	if err != nil {
		return nil, "", kbError(err)
	}
	pageItems, next, err := pageByNumber(list, func(i killbill.Invoice) int64 { return numberOf(i.InvoiceNumber) }, page)
	if err != nil {
		return nil, "", err
	}
	out := make([]Invoice, 0, len(pageItems))
	for _, in := range pageItems {
		inv, err := invoiceOf(in, false)
		if err != nil {
			return nil, "", err
		}
		out = append(out, inv)
	}
	return out, next, nil
}

func (s *Service) ownedInvoice(ctx context.Context, a account, id string) (killbill.Invoice, error) {
	if err := checkKillBillID(id); err != nil {
		return killbill.Invoice{}, err
	}
	in, err := s.KillBill.Invoice(ctx, a.creds, id)
	if err != nil {
		return killbill.Invoice{}, kbError(err)
	}
	if err := owned(a, in.AccountID); err != nil {
		return killbill.Invoice{}, err
	}
	return in, nil
}

// GetInvoice returns an invoice of the tenant with its items.
func (s *Service) GetInvoice(ctx context.Context, sc scope.Scope, id string) (Invoice, error) {
	a, err := s.account(ctx, sc)
	if err != nil {
		return Invoice{}, err
	}
	in, err := s.ownedInvoice(ctx, a, id)
	if err != nil {
		return Invoice{}, err
	}
	return invoiceOf(in, true)
}

// InvoicePDF answers invoice_pdf_unavailable for an invoice of the tenant.
// PDF rendering needs an invoice template plugin, and components.yaml pins
// none in this release. An invoice of another tenant is not_found.
func (s *Service) InvoicePDF(ctx context.Context, sc scope.Scope, id string) ([]byte, error) {
	a, err := s.account(ctx, sc)
	if err != nil {
		return nil, err
	}
	if _, err := s.ownedInvoice(ctx, a, id); err != nil {
		return nil, err
	}
	return nil, zserrors.New(zserrors.CodeInvoicePDFUnavailable, "")
}
