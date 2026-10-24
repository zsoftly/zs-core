package killbill

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// stateRefusalCode is the Kill Bill error code ENT_CANCEL_BAD_STATE. Kill
// Bill 0.24.22 answers it with HTTP 500 when the subscription is not in a
// state it can be cancelled from, such as one that is already CANCELLED. The
// request changed nothing.
const stateRefusalCode = 1031

// CancelHow says how a subscription is cancelled. Exactly one field is set;
// nothing is defaulted.
//
// With Policy, IMMEDIATE or END_OF_TERM, the policy is sent as both the
// entitlement and the billing policy. Kill Bill 0.24.22 ignores a requested
// date when a policy is present, so the two are never sent together.
//
// With RequestedDate, a day written YYYY-MM-DD, the date is sent with
// useRequestedDateForBilling=true so that billing ends on the day the
// entitlement does. Without that flag Kill Bill ends billing at once.
type CancelHow struct {
	Policy        string
	RequestedDate string
}

func (h CancelHow) validate() error {
	switch {
	case h.Policy == "" && h.RequestedDate == "":
		return errors.New("killbill cancel subscription: a policy or a requested date is required")
	case h.Policy != "" && h.RequestedDate != "":
		return errors.New("killbill cancel subscription: a policy and a requested date cannot be sent together")
	case h.Policy != "" && h.Policy != PolicyImmediate && h.Policy != PolicyEndOfTerm:
		return errors.New("killbill cancel subscription: the policy must be IMMEDIATE or END_OF_TERM")
	}
	if h.RequestedDate != "" {
		if _, err := time.Parse(time.DateOnly, h.RequestedDate); err != nil {
			return errors.New("killbill cancel subscription: the requested date must be a day, YYYY-MM-DD")
		}
	}
	return nil
}

// CancelOutcome is what a cancel request proves about the subscription. The
// zero value is CancelUnknown, so an unset outcome is never read as safe.
type CancelOutcome int

const (
	// CancelUnknown: Kill Bill may or may not have cancelled. A timeout, a
	// connection that broke after the request was written, 408, 429, a
	// server error that is not a state refusal, or a redirect. Read the
	// subscription before anything else.
	CancelUnknown CancelOutcome = iota
	// Cancelled: Kill Bill answered 2xx. The cancel is scheduled or done;
	// read the subscription for its state and effective date.
	Cancelled
	// CancelRefused: Kill Bill refused because of the subscription's state
	// (error code 1031, answered as HTTP 500) and changed nothing. The state
	// is named in Kill Bill's message but is not read here: read the
	// subscription to see whether it is already cancelled.
	CancelRefused
	// CancelNotFound: Kill Bill has no such subscription (HTTP 404).
	CancelNotFound
	// NotCancelled: nothing was changed. The request was not sent (a bad
	// argument, a subscription that is not in the account, a cancelled
	// context, a connection that was never made) or Kill Bill rejected it
	// with another 4xx.
	NotCancelled
)

// CancelResult is the outcome of CancelSubscriptionOnce, the HTTP status
// behind it (0 when Kill Bill did not answer) and Kill Bill's error code
// (0 when there is none).
type CancelResult struct {
	Outcome CancelOutcome
	Status  int
	Code    int
}

// CancelSubscriptionOnce cancels one subscription of accountID, once, and
// classifies what the answer proves. It never retries.
//
// It first reads the bundles of accountID with creds and refuses, without
// sending the cancel, a subscription that is not among them: Kill Bill
// answers a subscription of another Kill Bill tenant with a plain server
// error that cannot be told from a failure that happened. The cancel carries
// the audit headers of a write. The error is nil for Cancelled and never
// carries credentials or Kill Bill's message.
//
// Kill Bill 0.24.22 cancels the add-ons of a bundle together with its base
// subscription and with the same date. It answers a second IMMEDIATE cancel
// of a cancelled subscription with CancelRefused, and a second END_OF_TERM
// cancel with 2xx. A cancel of a pending one by requested date answers 2xx
// for the same day or an earlier one, which moves the cancel earlier, and
// CancelRefused for a later day, which Kill Bill does not move. A requested
// day before the subscription's last change is NotCancelled with code 1001.
func (c *Client) CancelSubscriptionOnce(ctx context.Context, creds TenantCredentials, cc CallContext, accountID, subscriptionID string, how CancelHow) (CancelResult, error) {
	notCancelled := func(err error) (CancelResult, error) {
		return CancelResult{Outcome: NotCancelled}, err
	}
	if accountID == "" || subscriptionID == "" {
		return notCancelled(errors.New("killbill cancel subscription: an account and a subscription are required"))
	}
	if err := how.validate(); err != nil {
		return notCancelled(err)
	}
	if cc.CreatedBy == "" {
		return notCancelled(errors.New("killbill cancel subscription: CreatedBy is required on writes"))
	}
	if err := ctx.Err(); err != nil {
		return notCancelled(fmt.Errorf("killbill cancel subscription: %w", err))
	}
	bundles, err := c.AccountBundles(ctx, creds, accountID)
	if err != nil {
		return notCancelled(fmt.Errorf("killbill cancel subscription: the account's subscriptions cannot be read: %w", err))
	}
	if !inBundles(bundles, accountID, subscriptionID) {
		return notCancelled(errors.New("killbill cancel subscription: the subscription is not one of the account's"))
	}
	q := url.Values{}
	if how.Policy != "" {
		q.Set("entitlementPolicy", how.Policy)
		q.Set("billingPolicy", how.Policy)
	} else {
		q.Set("requestedDate", how.RequestedDate)
		q.Set("useRequestedDateForBilling", "true")
	}
	resp, err := c.do(ctx, request{op: "cancel subscription", method: http.MethodDelete,
		path: "/1.0/kb/subscriptions/" + url.PathEscape(subscriptionID) + "?" + q.Encode(), tenant: &creds, call: &cc})
	res := CancelResult{Status: resp.Status, Code: errorCode(resp.Body)}
	if err == nil {
		res.Outcome = Cancelled
		return res, nil
	}
	switch {
	case resp.Status == 0:
		// No answer. Only a failure before the request was written proves
		// Kill Bill saw nothing.
		if neverWritten(err) {
			res.Outcome = NotCancelled
		}
	case resp.Status == http.StatusInternalServerError && res.Code == stateRefusalCode:
		res.Outcome = CancelRefused
	case resp.Status == http.StatusNotFound:
		res.Outcome = CancelNotFound
	case resp.Status >= 400 && resp.Status < 500 && resp.Status != http.StatusRequestTimeout && resp.Status != http.StatusTooManyRequests:
		res.Outcome = NotCancelled
	}
	return res, err
}

func inBundles(bundles []Bundle, accountID, subscriptionID string) bool {
	for _, b := range bundles {
		for _, s := range b.Subscriptions {
			if s.SubscriptionID == subscriptionID && (s.AccountID == "" || s.AccountID == accountID) {
				return true
			}
		}
	}
	return false
}

// CancelEffectiveDate returns the day, YYYY-MM-DD in UTC, on which a
// subscription's cancellation takes effect or took effect. Kill Bill dates
// the cancel at that day plus the bundle's start time of day, so only the day
// is meaningful. An IMMEDIATE cancel is dated today, a END_OF_TERM cancel the
// end of the term and a requested-date cancel the requested day. The error is
// non-nil when the subscription has no readable cancelled date.
func CancelEffectiveDate(s Subscription) (string, error) {
	if s.CancelledDate == "" {
		return "", errors.New("killbill subscription: no cancelled date")
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z07:00", time.DateOnly} {
		if t, err := time.Parse(layout, s.CancelledDate); err == nil {
			return t.UTC().Format(time.DateOnly), nil
		}
	}
	return "", errors.New("killbill subscription: the cancelled date cannot be read")
}

// CreateAddOn creates an add-on subscription to planName in the bundle of
// accountID and returns its ID. Kill Bill cancels it with the base
// subscription of the bundle.
func (c *Client) CreateAddOn(ctx context.Context, creds TenantCredentials, cc CallContext, accountID, bundleID, planName, externalKey string) (string, error) {
	resp, err := c.sendJSON(ctx, "create add-on", http.MethodPost, "/1.0/kb/subscriptions", creds, cc,
		Subscription{AccountID: accountID, BundleID: bundleID, PlanName: planName, ExternalKey: externalKey})
	if err != nil {
		return "", err
	}
	if id := locationID(resp); id != "" && id != "subscriptions" {
		return id, nil
	}
	s, err := c.SubscriptionByExternalKey(ctx, creds, externalKey)
	if err != nil {
		return "", err
	}
	return s.SubscriptionID, nil
}
