package killbill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// UsageRecord is one amount of a unit on a date. RecordDate is an ISO 8601
// date or date-time. Kill Bill 0.24.22 stores Amount as a decimal with nine
// digits after the point.
type UsageRecord struct {
	RecordDate string      `json:"recordDate"`
	Amount     json.Number `json:"amount"`
}

// UnitUsageRecords are the records of one usage unit.
type UnitUsageRecords struct {
	UnitType     string        `json:"unitType"`
	UsageRecords []UsageRecord `json:"usageRecords"`
}

// SubscriptionUsageRecord is what POST /1.0/kb/usages takes: records of
// one subscription under one tracking ID.
type SubscriptionUsageRecord struct {
	SubscriptionID   string             `json:"subscriptionId"`
	TrackingID       string             `json:"trackingId"`
	UnitUsageRecords []UnitUsageRecords `json:"unitUsageRecords"`
}

// ErrTrackingIDExists is a usage record whose tracking ID Kill Bill already
// holds for the subscription. Kill Bill 0.24.22 answers it with HTTP 400 and
// error code 50000, whatever the amounts, and records nothing.
var ErrTrackingIDExists = errors.New("killbill: a usage record with this tracking ID exists")

// trackingIDExistsCode is the error code of ErrTrackingIDExists.
const trackingIDExistsCode = 50000

// RecordUsage records usage on a subscription. Kill Bill deduplicates on the
// tracking ID per subscription: a second record with the same tracking ID
// returns ErrTrackingIDExists. Kill Bill 0.24.22 checks neither the unit,
// the sign of the amount nor the state of the subscription, so callers
// check them first.
func (c *Client) RecordUsage(ctx context.Context, creds TenantCredentials, cc CallContext, rec SubscriptionUsageRecord) error {
	_, err := c.sendJSON(ctx, "record usage", http.MethodPost, "/1.0/kb/usages", creds, cc, rec)
	var kbErr *Error
	if errors.As(err, &kbErr) && kbErr.Status == http.StatusBadRequest && kbErr.Code == trackingIDExistsCode {
		return fmt.Errorf("killbill record usage: %w", ErrTrackingIDExists)
	}
	return err
}

// RolledUpUnit is the total of one unit over a period.
type RolledUpUnit struct {
	UnitType string      `json:"unitType"`
	Amount   json.Number `json:"amount"`
}

// RolledUpUsage is the usage of a subscription over a period, per unit.
type RolledUpUsage struct {
	SubscriptionID string         `json:"subscriptionId"`
	StartDate      string         `json:"startDate"`
	EndDate        string         `json:"endDate"`
	RolledUpUnits  []RolledUpUnit `json:"rolledUpUnits"`
}

// RolledUpUsage returns the usage Kill Bill holds for a subscription from
// startDate, inclusive, to endDate, exclusive, both YYYY-MM-DD. Kill Bill
// requires both dates.
func (c *Client) RolledUpUsage(ctx context.Context, creds TenantCredentials, subscriptionID, startDate, endDate string) (RolledUpUsage, error) {
	q := url.Values{"startDate": {startDate}, "endDate": {endDate}}
	var out RolledUpUsage
	if err := c.getJSON(ctx, "get usage", "/1.0/kb/usages/"+url.PathEscape(subscriptionID)+"?"+q.Encode(), creds, &out); err != nil {
		return RolledUpUsage{}, err
	}
	return out, nil
}

// DryRunInvoice asks Kill Bill for the invoice it would generate for the
// account at targetDate, YYYY-MM-DD, without making it. Kill Bill 0.24.22
// answers 204 when nothing would be invoiced, which is found false. It needs
// the audit headers like a write, although it changes nothing. Amount is the
// sum of the charges. The account credit Kill Bill would apply is a
// separate CBA_ADJ item and is left out of Amount.
func (c *Client) DryRunInvoice(ctx context.Context, creds TenantCredentials, cc CallContext, accountID, targetDate string) (Invoice, bool, error) {
	q := url.Values{"accountId": {accountID}, "targetDate": {targetDate}}
	resp, err := c.sendJSON(ctx, "dry run invoice", http.MethodPost, "/1.0/kb/invoices/dryRun?"+q.Encode(), creds, cc,
		map[string]string{"dryRunType": "TARGET_DATE"})
	if err != nil {
		return Invoice{}, false, err
	}
	if resp.Status == http.StatusNoContent || len(resp.Body) == 0 {
		return Invoice{}, false, nil
	}
	var out Invoice
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return Invoice{}, false, fmt.Errorf("killbill dry run invoice: unexpected response")
	}
	return out, true, nil
}
