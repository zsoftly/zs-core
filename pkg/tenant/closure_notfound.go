package tenant

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// triggerNotFoundAnswer is what a closure invoice trigger that answered 404
// turned out to be. The zero value says it is not known, which is for a
// person.
type triggerNotFoundAnswer struct {
	// invoiceID is the committed invoice of the cancel, which the trigger found
	// already made.
	invoiceID string
	// nothing says Kill Bill has nothing left to invoice and the account has
	// no invoice of the cancel.
	nothing bool
}

// settleTriggerNotFound reads what a 404 of the trigger means, after a dry run
// that showed something. Kill Bill generates the invoice of an immediate
// cancel a few seconds after the cancel, and the trigger generates it at once,
// so the invoice can appear between the dry run and the trigger, which then
// has nothing to invoice and answers 404.
//
// It asks the dry run again. Only when that says nothing is left does it read
// the account: the one committed invoice dated on or after the day of the
// cancel with a usage or repair item of a cancelled subscription of the
// account is the invoice of the cancel, and no such invoice means there was
// nothing to invoice. Anything else, a failed read included, is not known.
func settleTriggerNotFound(ctx context.Context, db DB, sc scope.Scope, kb *killbill.Client, creds killbill.TenantCredentials, triggerErr error, accountID, targetDate string) triggerNotFoundAnswer {
	if !errors.Is(triggerErr, killbill.ErrNotFound) {
		return triggerNotFoundAnswer{}
	}
	var cancelDay string
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce((cancellation_completed_at AT TIME ZONE 'UTC')::date::text, '')
FROM tenant_closure_settlements WHERE brand_id = $1 AND tenant_id = $2`, sc.Brand, sc.Tenant).Scan(&cancelDay)
	})
	if err != nil || cancelDay == "" {
		return triggerNotFoundAnswer{}
	}
	_, stillSomething, err := kb.DryRunInvoice(ctx, creds, callContext("", "closure invoice dry run"), accountID, targetDate)
	if err != nil || stillSomething {
		return triggerNotFoundAnswer{}
	}
	bundles, err := kb.AccountBundles(ctx, creds, accountID)
	if err != nil {
		return triggerNotFoundAnswer{}
	}
	cancelled := map[string]bool{}
	for _, b := range bundles {
		for _, s := range b.Subscriptions {
			if s.State == "CANCELLED" && s.SubscriptionID != "" {
				cancelled[s.SubscriptionID] = true
			}
		}
	}
	invoices, err := kb.AccountInvoices(ctx, creds, accountID, true)
	if err != nil {
		return triggerNotFoundAnswer{}
	}
	var matching []string
	for _, in := range invoices {
		if in.Status != "COMMITTED" || !strings.EqualFold(in.AccountID, accountID) || len(in.InvoiceDate) < len(cancelDay) || in.InvoiceDate[:len(cancelDay)] < cancelDay {
			continue
		}
		for _, item := range in.Items {
			if (item.ItemType == "USAGE" || item.ItemType == "REPAIR_ADJ") && cancelled[item.SubscriptionID] {
				matching = append(matching, in.InvoiceID)
				break
			}
		}
	}
	switch len(matching) {
	case 0:
		return triggerNotFoundAnswer{nothing: true}
	case 1:
		return triggerNotFoundAnswer{invoiceID: matching[0]}
	}
	return triggerNotFoundAnswer{}
}
