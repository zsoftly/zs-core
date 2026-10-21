package killbill

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// Account is a Kill Bill account. Core creates one per tenant with the tenant
// ID as the external key. AccountBalance and AccountCBA are set only by
// AccountWithBalance: the balance owed, negative when the account holds
// credit, and the account credit, both in the account currency.
type Account struct {
	AccountID      string      `json:"accountId,omitempty"`
	ExternalKey    string      `json:"externalKey"`
	Name           string      `json:"name"`
	Currency       string      `json:"currency"`
	Country        string      `json:"country,omitempty"`
	Email          string      `json:"email,omitempty"`
	Locale         string      `json:"locale,omitempty"`
	AccountBalance json.Number `json:"accountBalance,omitempty"`
	AccountCBA     json.Number `json:"accountCBA,omitempty"`
}

// CreateAccount creates an account and returns its ID. An external key that
// is already used in the tenant returns ErrConflict.
func (c *Client) CreateAccount(ctx context.Context, creds TenantCredentials, cc CallContext, a Account) (string, error) {
	a.AccountID, a.AccountBalance, a.AccountCBA = "", "", ""
	body, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, request{op: "create account", method: http.MethodPost, path: "/1.0/kb/accounts",
		tenant: &creds, call: &cc, contentType: "application/json", body: body})
	if err != nil {
		return "", err
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil && path.Base(u.Path) != "accounts" {
			return path.Base(u.Path), nil
		}
	}
	found, err := c.AccountByExternalKey(ctx, creds, a.ExternalKey)
	if err != nil {
		return "", err
	}
	return found.AccountID, nil
}

// AccountByExternalKey returns the account with the external key, or
// ErrNotFound.
func (c *Client) AccountByExternalKey(ctx context.Context, creds TenantCredentials, externalKey string) (Account, error) {
	q := url.Values{"externalKey": {externalKey}}
	resp, err := c.do(ctx, request{op: "get account", method: http.MethodGet, path: "/1.0/kb/accounts?" + q.Encode(), tenant: &creds})
	if err != nil {
		return Account{}, err
	}
	var a Account
	if err := json.Unmarshal(resp.Body, &a); err != nil || a.AccountID == "" {
		return Account{}, fmt.Errorf("killbill get account: unexpected response")
	}
	return a, nil
}

// AccountWithBalance returns the account with its balance and its account
// credit. An unknown account is ErrNotFound.
func (c *Client) AccountWithBalance(ctx context.Context, creds TenantCredentials, accountID string) (Account, error) {
	q := url.Values{"accountWithBalanceAndCBA": {"true"}}
	var a Account
	if err := c.getJSON(ctx, "get account", "/1.0/kb/accounts/"+url.PathEscape(accountID)+"?"+q.Encode(), creds, &a); err != nil {
		return Account{}, err
	}
	if a.AccountID == "" {
		return Account{}, fmt.Errorf("killbill get account: unexpected response")
	}
	return a, nil
}

// SetAccountEmail sets only an account's primary email. An empty email is
// sent explicitly to clear it. Kill Bill 0.24.22 merges omitted account
// fields when treatNullAsReset is false; additional account emails are
// unchanged. Callers verify the result with a scoped account read.
func (c *Client) SetAccountEmail(ctx context.Context, creds TenantCredentials, cc CallContext, accountID, email string) error {
	q := url.Values{"treatNullAsReset": {"false"}}
	_, err := c.sendJSON(ctx, "set account email", http.MethodPut,
		"/1.0/kb/accounts/"+url.PathEscape(accountID)+"?"+q.Encode(), creds, cc, struct {
			Email string `json:"email"`
		}{Email: email})
	return err
}

// AccountEmail is one unmanaged additional account email, separate from the
// primary Account.Email. Core only reads this collection; it never changes it.
type AccountEmail struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
}

// AccountEmails reads the additional-email collection of the scoped account.
// The pinned API returns an array, including [] for absence. Null, malformed
// entries or an entry from another account are failures, never proof of no CC.
func (c *Client) AccountEmails(ctx context.Context, creds TenantCredentials, accountID string) ([]AccountEmail, error) {
	var emails []AccountEmail
	if err := c.getJSON(ctx, "get account emails", "/1.0/kb/accounts/"+url.PathEscape(accountID)+"/emails", creds, &emails); err != nil {
		return nil, err
	}
	if emails == nil {
		return nil, fmt.Errorf("killbill get account emails: unexpected response")
	}
	for _, email := range emails {
		if email.AccountID != accountID || strings.TrimSpace(email.Email) == "" {
			return nil, fmt.Errorf("killbill get account emails: unexpected response")
		}
	}
	return emails, nil
}

// BlockingState is an entitlement blocking state of an account.
// EffectiveDate is set on the states Kill Bill returns.
type BlockingState struct {
	StateName          string `json:"stateName"`
	Service            string `json:"service"`
	IsBlockChange      bool   `json:"isBlockChange"`
	IsBlockEntitlement bool   `json:"isBlockEntitlement"`
	IsBlockBilling     bool   `json:"isBlockBilling"`
	Type               string `json:"type"`
	EffectiveDate      string `json:"effectiveDate,omitempty"`
}

// AccountBlockingStates returns every account-level blocking state of an
// account, oldest first. Kill Bill keeps every state; per service, the
// latest one is in force.
func (c *Client) AccountBlockingStates(ctx context.Context, creds TenantCredentials, accountID string) ([]BlockingState, error) {
	q := url.Values{"blockingStateTypes": {"ACCOUNT"}}
	var out []BlockingState
	if err := c.getJSON(ctx, "get blocking states", "/1.0/kb/accounts/"+url.PathEscape(accountID)+"/block?"+q.Encode(), creds, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// OverdueState is the overdue state of an account. Name is
// __KILLBILL__CLEAR__OVERDUE_STATE__ when the account is not overdue.
type OverdueState struct {
	Name                                  string `json:"name"`
	IsClearState                          bool   `json:"isClearState"`
	IsBlockChanges                        bool   `json:"isBlockChanges"`
	IsDisableEntitlementAndChangesBlocked bool   `json:"isDisableEntitlementAndChangesBlocked"`
}

// AccountOverdue returns the overdue state of an account.
func (c *Client) AccountOverdue(ctx context.Context, creds TenantCredentials, accountID string) (OverdueState, error) {
	var out OverdueState
	if err := c.getJSON(ctx, "get overdue state", "/1.0/kb/accounts/"+url.PathEscape(accountID)+"/overdue", creds, &out); err != nil {
		return OverdueState{}, err
	}
	if out.Name == "" {
		return OverdueState{}, fmt.Errorf("killbill get overdue state: unexpected response")
	}
	return out, nil
}

// SetAccountBlockingState adds an account-level blocking state through the
// entitlement blocking API. Kill Bill keeps every state; the latest one of a
// service is in force.
func (c *Client) SetAccountBlockingState(ctx context.Context, creds TenantCredentials, cc CallContext, accountID string, s BlockingState) error {
	s.Type = "ACCOUNT"
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, request{op: "set account blocking state", method: http.MethodPost,
		path: "/1.0/kb/accounts/" + url.PathEscape(accountID) + "/block", tenant: &creds, call: &cc,
		contentType: "application/json", body: body})
	return err
}

// CloseAccount closes an account: it cancels every subscription, writes off
// unpaid invoices and removes future notifications. Kill Bill answers 204
// for an account that is already closed.
func (c *Client) CloseAccount(ctx context.Context, creds TenantCredentials, cc CallContext, accountID string) error {
	q := url.Values{
		"cancelAllSubscriptions":    {"true"},
		"writeOffUnpaidInvoices":    {"true"},
		"itemAdjustUnpaidInvoices":  {"false"},
		"removeFutureNotifications": {"true"},
	}
	_, err := c.do(ctx, request{op: "close account", method: http.MethodDelete,
		path: "/1.0/kb/accounts/" + url.PathEscape(accountID) + "?" + q.Encode(), tenant: &creds, call: &cc})
	return err
}
