package killbill

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
)

// Account is a Kill Bill account. Core creates one per tenant with the tenant
// ID as the external key.
type Account struct {
	AccountID   string `json:"accountId,omitempty"`
	ExternalKey string `json:"externalKey"`
	Name        string `json:"name"`
	Currency    string `json:"currency"`
	Country     string `json:"country,omitempty"`
}

// CreateAccount creates an account and returns its ID. An external key that
// is already used in the tenant returns ErrConflict.
func (c *Client) CreateAccount(ctx context.Context, creds TenantCredentials, cc CallContext, a Account) (string, error) {
	a.AccountID = ""
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

// BlockingState is an entitlement blocking state of an account.
type BlockingState struct {
	StateName          string `json:"stateName"`
	Service            string `json:"service"`
	IsBlockChange      bool   `json:"isBlockChange"`
	IsBlockEntitlement bool   `json:"isBlockEntitlement"`
	IsBlockBilling     bool   `json:"isBlockBilling"`
	Type               string `json:"type"`
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
