package killbill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Price is an amount in one currency. Value keeps Kill Bill's decimal text.
type Price struct {
	Currency string      `json:"currency"`
	Value    json.Number `json:"value"`
}

// Duration is the length of a catalogue phase. Number is -1 for UNLIMITED.
type Duration struct {
	Unit   string `json:"unit"`
	Number int    `json:"number"`
}

// TieredBlock prices a block of a usage unit.
type TieredBlock struct {
	Unit   string  `json:"unit"`
	Size   string  `json:"size"`
	Max    string  `json:"max"`
	Prices []Price `json:"prices"`
}

// CatalogTier is one tier of a usage.
type CatalogTier struct {
	Blocks []TieredBlock `json:"blocks"`
}

// CatalogUsage is a usage section of a phase.
type CatalogUsage struct {
	BillingPeriod string        `json:"billingPeriod"`
	Tiers         []CatalogTier `json:"tiers"`
}

// CatalogPhase is a phase of a plan: TRIAL, DISCOUNT, FIXEDTERM or
// EVERGREEN.
type CatalogPhase struct {
	Type        string         `json:"type"`
	Prices      []Price        `json:"prices"`
	FixedPrices []Price        `json:"fixedPrices"`
	Duration    Duration       `json:"duration"`
	Usages      []CatalogUsage `json:"usages"`
}

// CatalogPlan is a plan of a product.
type CatalogPlan struct {
	Name          string         `json:"name"`
	PrettyName    string         `json:"prettyName"`
	BillingPeriod string         `json:"billingPeriod"`
	Phases        []CatalogPhase `json:"phases"`
}

// CatalogProduct is a product: BASE, ADD_ON or STANDALONE.
type CatalogProduct struct {
	Type       string        `json:"type"`
	Name       string        `json:"name"`
	PrettyName string        `json:"prettyName"`
	Plans      []CatalogPlan `json:"plans"`
	Included   []string      `json:"included"`
	Available  []string      `json:"available"`
}

// CatalogUnit is a usage unit.
type CatalogUnit struct {
	Name       string `json:"name"`
	PrettyName string `json:"prettyName"`
}

// Catalog is one version of a tenant's catalogue.
type Catalog struct {
	Name          string           `json:"name"`
	EffectiveDate string           `json:"effectiveDate"`
	Currencies    []string         `json:"currencies"`
	Units         []CatalogUnit    `json:"units"`
	Products      []CatalogProduct `json:"products"`
}

// Catalogue returns every catalogue version of the tenant, oldest first.
// The last one is in force.
func (c *Client) Catalogue(ctx context.Context, creds TenantCredentials) ([]Catalog, error) {
	var out []Catalog
	if err := c.getJSON(ctx, "get catalogue", "/1.0/kb/catalog", creds, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Subscription is a Kill Bill subscription. Dates are Kill Bill's text.
type Subscription struct {
	AccountID          string `json:"accountId"`
	BundleID           string `json:"bundleId,omitempty"`
	SubscriptionID     string `json:"subscriptionId,omitempty"`
	ExternalKey        string `json:"externalKey,omitempty"`
	StartDate          string `json:"startDate,omitempty"`
	ProductName        string `json:"productName,omitempty"`
	ProductCategory    string `json:"productCategory,omitempty"`
	BillingPeriod      string `json:"billingPeriod,omitempty"`
	PhaseType          string `json:"phaseType,omitempty"`
	PriceList          string `json:"priceList,omitempty"`
	PlanName           string `json:"planName,omitempty"`
	State              string `json:"state,omitempty"`
	CancelledDate      string `json:"cancelledDate,omitempty"`
	ChargedThroughDate string `json:"chargedThroughDate,omitempty"`
	BillingEndDate     string `json:"billingEndDate,omitempty"`
}

// Bundle groups a base subscription with its add-ons.
type Bundle struct {
	AccountID     string         `json:"accountId"`
	BundleID      string         `json:"bundleId"`
	ExternalKey   string         `json:"externalKey"`
	Subscriptions []Subscription `json:"subscriptions"`
}

// AccountBundles returns the bundles of an account with their
// subscriptions.
func (c *Client) AccountBundles(ctx context.Context, creds TenantCredentials, accountID string) ([]Bundle, error) {
	var out []Bundle
	if err := c.getJSON(ctx, "get bundles", "/1.0/kb/accounts/"+url.PathEscape(accountID)+"/bundles", creds, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Subscription returns a subscription, or ErrNotFound.
func (c *Client) Subscription(ctx context.Context, creds TenantCredentials, id string) (Subscription, error) {
	var out Subscription
	if err := c.getJSON(ctx, "get subscription", "/1.0/kb/subscriptions/"+url.PathEscape(id), creds, &out); err != nil {
		return Subscription{}, err
	}
	return out, nil
}

// SubscriptionByExternalKey returns the subscription with the external key,
// or ErrNotFound. External keys are unique per Kill Bill tenant.
func (c *Client) SubscriptionByExternalKey(ctx context.Context, creds TenantCredentials, key string) (Subscription, error) {
	q := url.Values{"externalKey": {key}}
	var out Subscription
	if err := c.getJSON(ctx, "get subscription", "/1.0/kb/subscriptions?"+q.Encode(), creds, &out); err != nil {
		return Subscription{}, err
	}
	return out, nil
}

// CreateSubscription creates a subscription to a plan and returns its ID.
// Kill Bill 0.24.22 answers a second subscription with the same external key
// with HTTP 500, so callers look the key up first.
func (c *Client) CreateSubscription(ctx context.Context, creds TenantCredentials, cc CallContext, accountID, planName, externalKey string) (string, error) {
	resp, err := c.sendJSON(ctx, "create subscription", http.MethodPost, "/1.0/kb/subscriptions", creds, cc,
		Subscription{AccountID: accountID, PlanName: planName, ExternalKey: externalKey})
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

// Billing policies of a plan change or a cancellation.
const (
	PolicyImmediate = "IMMEDIATE"
	PolicyEndOfTerm = "END_OF_TERM"
)

// ChangePlan moves a subscription to another plan with a billing policy,
// IMMEDIATE or END_OF_TERM.
func (c *Client) ChangePlan(ctx context.Context, creds TenantCredentials, cc CallContext, subscriptionID, planName, policy string) error {
	q := url.Values{"billingPolicy": {policy}}
	_, err := c.sendJSON(ctx, "change plan", http.MethodPut, "/1.0/kb/subscriptions/"+url.PathEscape(subscriptionID)+"?"+q.Encode(),
		creds, cc, Subscription{PlanName: planName})
	return err
}

// CancelSubscription cancels a subscription. policy, IMMEDIATE or
// END_OF_TERM, applies to both the entitlement and billing.
func (c *Client) CancelSubscription(ctx context.Context, creds TenantCredentials, cc CallContext, subscriptionID, policy string) error {
	q := url.Values{"entitlementPolicy": {policy}, "billingPolicy": {policy}}
	_, err := c.do(ctx, request{op: "cancel subscription", method: http.MethodDelete,
		path: "/1.0/kb/subscriptions/" + url.PathEscape(subscriptionID) + "?" + q.Encode(), tenant: &creds, call: &cc})
	return err
}

// InvoiceItem is a line of an invoice, or a credit.
type InvoiceItem struct {
	InvoiceItemID  string      `json:"invoiceItemId,omitempty"`
	InvoiceID      string      `json:"invoiceId,omitempty"`
	AccountID      string      `json:"accountId"`
	SubscriptionID string      `json:"subscriptionId,omitempty"`
	ProductName    string      `json:"productName,omitempty"`
	PlanName       string      `json:"planName,omitempty"`
	PhaseName      string      `json:"phaseName,omitempty"`
	UsageName      string      `json:"usageName,omitempty"`
	ItemType       string      `json:"itemType,omitempty"`
	Description    string      `json:"description,omitempty"`
	StartDate      string      `json:"startDate,omitempty"`
	EndDate        string      `json:"endDate,omitempty"`
	Amount         json.Number `json:"amount,omitempty"`
	Currency       string      `json:"currency,omitempty"`
}

// Invoice is a Kill Bill invoice.
type Invoice struct {
	InvoiceID     string        `json:"invoiceId"`
	InvoiceNumber string        `json:"invoiceNumber"`
	InvoiceDate   string        `json:"invoiceDate"`
	TargetDate    string        `json:"targetDate"`
	Status        string        `json:"status"`
	Currency      string        `json:"currency"`
	AccountID     string        `json:"accountId"`
	Amount        json.Number   `json:"amount"`
	Balance       json.Number   `json:"balance"`
	CreditAdj     json.Number   `json:"creditAdj"`
	RefundAdj     json.Number   `json:"refundAdj"`
	Items         []InvoiceItem `json:"items"`
	// TrackingIDs are the tracking IDs of the usage records the invoice
	// bills. Kill Bill 0.24.22 lists them on an invoice read with its
	// components.
	TrackingIDs []string `json:"trackingIds,omitempty"`
}

// AccountInvoices returns the invoices of an account, with their items when
// withItems is set.
func (c *Client) AccountInvoices(ctx context.Context, creds TenantCredentials, accountID string, withItems bool) ([]Invoice, error) {
	q := url.Values{"includeInvoiceComponents": {fmt.Sprint(withItems)}}
	var out []Invoice
	if err := c.getJSON(ctx, "get invoices", "/1.0/kb/accounts/"+url.PathEscape(accountID)+"/invoices?"+q.Encode(), creds, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// TriggerInvoice asks Kill Bill to create a future invoice for one account
// and target date. A created invoice is identified only by a 201 Location.
func (c *Client) TriggerInvoice(ctx context.Context, creds TenantCredentials, cc CallContext, accountID, targetDate string) (string, error) {
	q := url.Values{"accountId": {accountID}, "targetDate": {targetDate}}
	resp, err := c.do(ctx, request{op: "trigger invoice", method: http.MethodPost, path: "/1.0/kb/invoices?" + q.Encode(), tenant: &creds, call: &cc})
	if err != nil {
		return "", err
	}
	if resp.Status != http.StatusCreated {
		return "", fmt.Errorf("killbill trigger invoice: unexpected HTTP %d", resp.Status)
	}
	id := locationID(resp)
	if _, err := idgen.ParseUUID(id); err != nil {
		return "", fmt.Errorf("killbill trigger invoice: invalid Location")
	}
	return id, nil
}

// Invoice returns an invoice with its items, or ErrNotFound.
func (c *Client) Invoice(ctx context.Context, creds TenantCredentials, id string) (Invoice, error) {
	var out Invoice
	if err := c.getJSON(ctx, "get invoice", "/1.0/kb/invoices/"+url.PathEscape(id), creds, &out); err != nil {
		return Invoice{}, err
	}
	return out, nil
}

// CreateCredit adds credit to an account and returns the credit item. The
// credit lands on an invoice of its own and raises the account credit. Kill
// Bill refuses an amount that is not strictly positive with error code 4009.
func (c *Client) CreateCredit(ctx context.Context, creds TenantCredentials, cc CallContext, item InvoiceItem) (InvoiceItem, error) {
	return c.invoiceItem(ctx, "create credit", "/1.0/kb/credits?autoCommit=true", creds, cc, item)
}

// CreateExternalCharge charges an account outside the catalogue and returns
// the charge item. Kill Bill pays the charge from the account credit first.
func (c *Client) CreateExternalCharge(ctx context.Context, creds TenantCredentials, cc CallContext, item InvoiceItem) (InvoiceItem, error) {
	return c.invoiceItem(ctx, "create external charge", "/1.0/kb/invoices/charges/"+url.PathEscape(item.AccountID)+"?autoCommit=true", creds, cc, item)
}

func (c *Client) invoiceItem(ctx context.Context, op, path string, creds TenantCredentials, cc CallContext, item InvoiceItem) (InvoiceItem, error) {
	resp, err := c.sendJSON(ctx, op, http.MethodPost, path, creds, cc, []InvoiceItem{item})
	if err != nil {
		return InvoiceItem{}, err
	}
	var out []InvoiceItem
	if err := json.Unmarshal(resp.Body, &out); err != nil || len(out) != 1 || out[0].InvoiceItemID == "" {
		return InvoiceItem{}, fmt.Errorf("killbill %s: unexpected response", op)
	}
	return out[0], nil
}

// PaymentTransaction is one transaction of a payment. Gateway error codes and
// messages are never read: they can hold provider text.
type PaymentTransaction struct {
	TransactionID          string      `json:"transactionId,omitempty"`
	TransactionExternalKey string      `json:"transactionExternalKey,omitempty"`
	PaymentID              string      `json:"paymentId,omitempty"`
	TransactionType        string      `json:"transactionType,omitempty"`
	Amount                 json.Number `json:"amount,omitempty"`
	Currency               string      `json:"currency,omitempty"`
	EffectiveDate          string      `json:"effectiveDate,omitempty"`
	Status                 string      `json:"status,omitempty"`
}

// Payment is a Kill Bill payment with its transactions.
type Payment struct {
	AccountID       string               `json:"accountId"`
	PaymentID       string               `json:"paymentId"`
	PaymentNumber   string               `json:"paymentNumber"`
	Currency        string               `json:"currency"`
	PaymentMethodID string               `json:"paymentMethodId"`
	AuthAmount      json.Number          `json:"authAmount"`
	CapturedAmount  json.Number          `json:"capturedAmount"`
	PurchasedAmount json.Number          `json:"purchasedAmount"`
	RefundedAmount  json.Number          `json:"refundedAmount"`
	CreditedAmount  json.Number          `json:"creditedAmount"`
	Transactions    []PaymentTransaction `json:"transactions"`
}

// AccountPayments returns the payments of an account.
func (c *Client) AccountPayments(ctx context.Context, creds TenantCredentials, accountID string) ([]Payment, error) {
	var out []Payment
	if err := c.getJSON(ctx, "get payments", "/1.0/kb/accounts/"+url.PathEscape(accountID)+"/payments", creds, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Payment returns a payment, or ErrNotFound.
func (c *Client) Payment(ctx context.Context, creds TenantCredentials, id string) (Payment, error) {
	var out Payment
	if err := c.getJSON(ctx, "get payment", "/1.0/kb/payments/"+url.PathEscape(id), creds, &out); err != nil {
		return Payment{}, err
	}
	return out, nil
}

// ErrTransactionKeyExists is a refund whose transaction external key Kill
// Bill already recorded as successful (error code 7030).
var ErrTransactionKeyExists = errors.New("killbill: a successful transaction with this external key exists")

// Refund refunds amount of a payment. transactionKey is the external key of
// the refund transaction. A second refund with the key of a successful one
// returns ErrTransactionKeyExists.
func (c *Client) Refund(ctx context.Context, creds TenantCredentials, cc CallContext, paymentID string, tx PaymentTransaction) error {
	_, err := c.sendJSON(ctx, "refund", http.MethodPost, "/1.0/kb/payments/"+url.PathEscape(paymentID)+"/refunds", creds, cc, tx)
	var kbErr *Error
	if errors.As(err, &kbErr) && kbErr.Code == 7030 {
		return fmt.Errorf("killbill refund: %w", ErrTransactionKeyExists)
	}
	return err
}

// PluginProperty is a key and value passed to a payment plugin. Properties
// are never logged.
type PluginProperty struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// PaymentMethodPluginInfo is what a payment plugin receives with a new
// payment method. ExternalPaymentMethodID is the provider's own reference,
// such as a token the provider's client library made.
type PaymentMethodPluginInfo struct {
	ExternalPaymentMethodID string           `json:"externalPaymentMethodId,omitempty"`
	Properties              []PluginProperty `json:"properties"`
}

// PaymentMethod is a payment method of an account. Plugin details are never
// read back.
type PaymentMethod struct {
	PaymentMethodID string                   `json:"paymentMethodId,omitempty"`
	ExternalKey     string                   `json:"externalKey,omitempty"`
	AccountID       string                   `json:"accountId,omitempty"`
	IsDefault       bool                     `json:"isDefault"`
	PluginName      string                   `json:"pluginName"`
	PluginInfo      *PaymentMethodPluginInfo `json:"pluginInfo,omitempty"`
}

// AccountPaymentMethods returns the payment methods of an account.
func (c *Client) AccountPaymentMethods(ctx context.Context, creds TenantCredentials, accountID string) ([]PaymentMethod, error) {
	var out []PaymentMethod
	if err := c.getJSON(ctx, "get payment methods", "/1.0/kb/accounts/"+url.PathEscape(accountID)+"/paymentMethods", creds, &out); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].PluginInfo = nil
	}
	return out, nil
}

// PaymentMethod returns a payment method, or ErrNotFound.
func (c *Client) PaymentMethod(ctx context.Context, creds TenantCredentials, id string) (PaymentMethod, error) {
	var out PaymentMethod
	if err := c.getJSON(ctx, "get payment method", "/1.0/kb/paymentMethods/"+url.PathEscape(id), creds, &out); err != nil {
		return PaymentMethod{}, err
	}
	out.PluginInfo = nil
	return out, nil
}

// AddPaymentMethod adds a payment method to an account through its plugin
// and returns its ID.
func (c *Client) AddPaymentMethod(ctx context.Context, creds TenantCredentials, cc CallContext, accountID string, pm PaymentMethod, isDefault bool) (string, error) {
	pm.AccountID, pm.PaymentMethodID = accountID, ""
	if pm.PluginInfo == nil {
		pm.PluginInfo = &PaymentMethodPluginInfo{}
	}
	if pm.PluginInfo.Properties == nil {
		pm.PluginInfo.Properties = []PluginProperty{}
	}
	q := url.Values{"isDefault": {fmt.Sprint(isDefault)}}
	resp, err := c.sendJSON(ctx, "add payment method", http.MethodPost, "/1.0/kb/accounts/"+url.PathEscape(accountID)+"/paymentMethods?"+q.Encode(), creds, cc, pm)
	if err != nil {
		return "", err
	}
	id := locationID(resp)
	if id == "" || id == "paymentMethods" {
		return "", fmt.Errorf("killbill add payment method: unexpected response")
	}
	return id, nil
}

// SetDefaultPaymentMethod makes a payment method the default of its account.
func (c *Client) SetDefaultPaymentMethod(ctx context.Context, creds TenantCredentials, cc CallContext, accountID, paymentMethodID string) error {
	_, err := c.do(ctx, request{op: "set default payment method", method: http.MethodPut,
		path:   "/1.0/kb/accounts/" + url.PathEscape(accountID) + "/paymentMethods/" + url.PathEscape(paymentMethodID) + "/setDefault",
		tenant: &creds, call: &cc})
	return err
}

// DeletePaymentMethod deletes a payment method. Kill Bill refuses to delete
// the default payment method.
func (c *Client) DeletePaymentMethod(ctx context.Context, creds TenantCredentials, cc CallContext, paymentMethodID string) error {
	_, err := c.do(ctx, request{op: "delete payment method", method: http.MethodDelete,
		path: "/1.0/kb/paymentMethods/" + url.PathEscape(paymentMethodID), tenant: &creds, call: &cc})
	return err
}

// FormDescriptor is what a payment plugin returns to send a customer to its
// hosted payment page: a URL and method, and the fields to send. A plugin
// that has no hosted page returns an empty descriptor.
type FormDescriptor struct {
	FormMethod string         `json:"formMethod"`
	FormURL    string         `json:"formUrl"`
	FormFields map[string]any `json:"formFields"`
}

// hostedPage is the body of the combined hosted payment page call. The
// account carries its ID only: Kill Bill 0.24.22 refuses an empty currency.
type hostedPage struct {
	Account                       accountRef       `json:"account"`
	PaymentMethod                 PaymentMethod    `json:"paymentMethod"`
	HostedPaymentPageFields       hostedFields     `json:"hostedPaymentPageFields"`
	PaymentMethodPluginProperties []PluginProperty `json:"paymentMethodPluginProperties"`
}

type accountRef struct {
	AccountID string `json:"accountId"`
}

type hostedFields struct {
	FormFields []PluginProperty `json:"formFields"`
}

// HostedPaymentPage adds a payment method of the plugin to the account and
// asks the plugin for the form of its hosted payment page, in one call.
func (c *Client) HostedPaymentPage(ctx context.Context, creds TenantCredentials, cc CallContext, accountID string, pm PaymentMethod, fields, properties []PluginProperty) (FormDescriptor, error) {
	if fields == nil {
		fields = []PluginProperty{}
	}
	if properties == nil {
		properties = []PluginProperty{}
	}
	pm.AccountID, pm.PaymentMethodID, pm.PluginInfo = "", "", nil
	body := hostedPage{Account: accountRef{AccountID: accountID}, PaymentMethod: pm,
		HostedPaymentPageFields: hostedFields{FormFields: fields}, PaymentMethodPluginProperties: properties}
	resp, err := c.sendJSON(ctx, "hosted payment page", http.MethodPost, "/1.0/kb/paymentGateways/hosted/form", creds, cc, body)
	if err != nil {
		return FormDescriptor{}, err
	}
	var out FormDescriptor
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return FormDescriptor{}, fmt.Errorf("killbill hosted payment page: unexpected response")
	}
	return out, nil
}

// NotificationCallbacks returns the push notification callback URLs of the
// tenant.
func (c *Client) NotificationCallbacks(ctx context.Context, creds TenantCredentials) ([]string, error) {
	var kv keyValues
	if err := c.getJSON(ctx, "get notification callback", "/1.0/kb/tenants/registerNotificationCallback", creds, &kv); err != nil {
		return nil, err
	}
	return kv.Values, nil
}

// RegisterNotificationCallback sets the push notification callback of the
// tenant. Kill Bill 0.24.22 keeps one callback per tenant: registering
// replaces the one before.
func (c *Client) RegisterNotificationCallback(ctx context.Context, creds TenantCredentials, cc CallContext, callback string) error {
	q := url.Values{"cb": {callback}}
	_, err := c.do(ctx, request{op: "register notification callback", method: http.MethodPost,
		path: "/1.0/kb/tenants/registerNotificationCallback?" + q.Encode(), tenant: &creds, call: &cc})
	return err
}

// PluginInfo is a bundle or plugin running on a Kill Bill node. PluginKey is
// empty for the platform bundles.
type PluginInfo struct {
	PluginKey  string `json:"pluginKey"`
	PluginName string `json:"pluginName"`
	Version    string `json:"version"`
	State      string `json:"state"`
}

// NodeInfo is one Kill Bill node.
type NodeInfo struct {
	NodeName    string       `json:"nodeName"`
	KBVersion   string       `json:"kbVersion"`
	PluginsInfo []PluginInfo `json:"pluginsInfo"`
}

// NodesInfo returns every Kill Bill node with its plugins. It needs the admin
// credentials only.
func (c *Client) NodesInfo(ctx context.Context) ([]NodeInfo, error) {
	resp, err := c.do(ctx, request{op: "get nodes info", method: http.MethodGet, path: "/1.0/kb/nodesInfo"})
	if err != nil {
		return nil, err
	}
	var out []NodeInfo
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("killbill get nodes info: unexpected response")
	}
	return out, nil
}

// Node command types Core sends through Kill Bill. KPM handles installation;
// the OSGi listener handles plugin lifecycle commands.
const (
	NodeCommandInstallPlugin = "INSTALL_PLUGIN"
	NodeCommandStopPlugin    = "STOP_PLUGIN"
	NodeCommandStartPlugin   = "START_PLUGIN"
	NodeCommandRestartPlugin = "RESTART_PLUGIN"
)

// NodeCommandProperty is a key and value of a node command.
type NodeCommandProperty struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// NodeCommand is a command broadcast through the Kill Bill nodesInfo API.
type NodeCommand struct {
	NodeCommandType       string                `json:"nodeCommandType"`
	IsSystemCommandType   bool                  `json:"isSystemCommandType"`
	NodeCommandProperties []NodeCommandProperty `json:"nodeCommandProperties"`
}

// SendNodeCommand sends a node command. Kill Bill answers 202 and runs it
// asynchronously; callers poll NodesInfo for the result.
func (c *Client) SendNodeCommand(ctx context.Context, cc CallContext, cmd NodeCommand) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, request{op: "node command " + strings.ToLower(cmd.NodeCommandType), method: http.MethodPost,
		path: "/1.0/kb/nodesInfo", call: &cc, contentType: "application/json", body: data})
	return err
}
