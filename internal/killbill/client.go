// Package killbill is a small client for the Kill Bill 0.24 REST API.
//
// It covers what brand apply, tenants and billing need: tenant lookup and
// creation, node information and KPM node commands with the admin
// credentials, and, with a tenant's API key and secret, per-tenant plugin
// configuration, user key-values, catalogue upload and read, overdue upload,
// push notification callbacks, accounts (creation, lookup, balance, blocking
// states, overdue state and closure), subscriptions, invoices, credits,
// external charges, payments, refunds, payment methods, usage records and
// rolled-up usage. Every endpoint was checked against Kill Bill 0.24.22.
//
// Every write carries the X-Killbill-CreatedBy, X-Killbill-Reason and
// X-Killbill-Comment headers from a CallContext, so Kill Bill's own audit log
// matches Core's. Secrets are sent only in headers and bodies and are never
// part of an error.
package killbill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/zsoftly/zs-core/internal/upstream"
)

// Header names.
const (
	HeaderAPIKey    = "X-Killbill-ApiKey"
	HeaderAPISecret = "X-Killbill-ApiSecret"
	HeaderCreatedBy = "X-Killbill-CreatedBy"
	HeaderReason    = "X-Killbill-Reason"
	HeaderComment   = "X-Killbill-Comment"
)

// CallContext is the audit context Kill Bill records with a write.
type CallContext struct {
	// CreatedBy is the Core principal, such as "staff:<subject>" or
	// "zs-core-cli". Required on writes.
	CreatedBy string
	// Reason is the caller's reason, when one was given.
	Reason string
	// Comment carries the correlation ID.
	Comment string
}

// Apply sets the X-Killbill-* audit headers on h. Header values are limited
// to one line of at most 255 characters.
func (cc CallContext) Apply(h http.Header) {
	set := func(name, value string) {
		value = strings.Join(strings.Fields(value), " ")
		if len(value) > 255 {
			value = value[:255]
		}
		if value != "" {
			h.Set(name, value)
		}
	}
	set(HeaderCreatedBy, cc.CreatedBy)
	set(HeaderReason, cc.Reason)
	set(HeaderComment, cc.Comment)
}

// TenantCredentials are the API key and secret of one Kill Bill tenant.
type TenantCredentials struct {
	APIKey    string
	APISecret string
}

// Apply sets the tenant headers on h.
func (t TenantCredentials) Apply(h http.Header) {
	h.Set(HeaderAPIKey, t.APIKey)
	h.Set(HeaderAPISecret, t.APISecret)
}

// Config configures a Client.
type Config struct {
	URL           string
	AdminUser     string
	AdminPassword string
	HTTPClient    *http.Client
}

// Client talks to Kill Bill.
type Client struct {
	base string
	cfg  Config
	http *http.Client
}

// Errors.
var (
	ErrNotFound = errors.New("killbill: not found")
	// ErrCredentialsRejected means Kill Bill refused the admin or tenant
	// credentials.
	ErrCredentialsRejected = errors.New("killbill: credentials rejected")
	ErrConflict            = errors.New("killbill: conflict")
)

// Error is a request Kill Bill rejected with a 4xx status. Code is Kill
// Bill's numeric error code when it sent one.
type Error struct {
	Op     string
	Status int
	Code   int
}

func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("killbill %s: HTTP %d, error code %d", e.Op, e.Status, e.Code)
	}
	return fmt.Sprintf("killbill %s: HTTP %d", e.Op, e.Status)
}

// Unavailable wraps transport failures and server errors. Status is the
// HTTP status of a server error, 0 for a transport failure.
type Unavailable struct {
	Op     string
	Status int
	Err    error
}

func (e *Unavailable) Error() string { return "killbill " + e.Op + " unavailable: " + e.Err.Error() }
func (e *Unavailable) Unwrap() error { return e.Err }

// IsUnavailable reports whether err means Kill Bill could not be reached.
func IsUnavailable(err error) bool {
	var u *Unavailable
	return errors.As(err, &u)
}

// New returns a client. URL and the admin credentials are required.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("killbill: URL is required")
	}
	if cfg.AdminUser == "" || cfg.AdminPassword == "" {
		return nil, errors.New("killbill: admin credentials are required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = upstream.NewClient()
	}
	return &Client{base: strings.TrimRight(cfg.URL, "/"), cfg: cfg, http: hc}, nil
}

type request struct {
	op          string
	method      string
	path        string
	tenant      *TenantCredentials
	call        *CallContext
	contentType string
	accept      string
	body        []byte
}

func (c *Client) do(ctx context.Context, r request) (upstream.Response, error) {
	var reader io.Reader
	if r.body != nil {
		reader = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.base+r.path, reader)
	if err != nil {
		return upstream.Response{}, err
	}
	req.SetBasicAuth(c.cfg.AdminUser, c.cfg.AdminPassword)
	accept := r.accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if r.contentType != "" {
		req.Header.Set("Content-Type", r.contentType)
	}
	if r.tenant != nil {
		r.tenant.Apply(req.Header)
	}
	if r.call != nil {
		if r.call.CreatedBy == "" {
			return upstream.Response{}, fmt.Errorf("killbill %s: CreatedBy is required on writes", r.op)
		}
		r.call.Apply(req.Header)
	}
	resp, err := upstream.Do(c.http, req)
	if err != nil {
		return resp, &Unavailable{Op: r.op, Err: err}
	}
	switch {
	case resp.OK():
		return resp, nil
	case resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden:
		return resp, fmt.Errorf("killbill %s: %w", r.op, ErrCredentialsRejected)
	case resp.Status == http.StatusNotFound:
		return resp, fmt.Errorf("killbill %s: %w", r.op, ErrNotFound)
	case resp.Status == http.StatusConflict:
		return resp, fmt.Errorf("killbill %s: %w", r.op, ErrConflict)
	case resp.Status >= 500 || upstream.Temporary(resp.Status) || resp.Status == http.StatusRequestTimeout:
		// A server error says nothing about the request; it is reported as
		// unavailability, never as a rejection of the manifest. Kill Bill
		// answers 408 when a call that waits for completion runs out of
		// time, after it accepted the change.
		return resp, &Unavailable{Op: r.op, Status: resp.Status, Err: fmt.Errorf("HTTP %d", resp.Status)}
	default:
		return resp, &Error{Op: r.op, Status: resp.Status, Code: errorCode(resp.Body)}
	}
}

// getJSON reads path with the tenant credentials and decodes the JSON
// answer into out.
func (c *Client) getJSON(ctx context.Context, op, path string, creds TenantCredentials, out any) error {
	resp, err := c.do(ctx, request{op: op, method: http.MethodGet, path: path, tenant: &creds})
	if err != nil {
		return err
	}
	if err := upstream.DecodeJSON(resp.Body, out); err != nil {
		return fmt.Errorf("killbill %s: unexpected response", op)
	}
	return nil
}

// sendJSON sends body as JSON with the tenant credentials and the audit
// headers, and returns the response.
func (c *Client) sendJSON(ctx context.Context, op, method, path string, creds TenantCredentials, cc CallContext, body any) (upstream.Response, error) {
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			return upstream.Response{}, err
		}
	}
	r := request{op: op, method: method, path: path, tenant: &creds, call: &cc}
	if data != nil {
		r.contentType, r.body = "application/json", data
	}
	return c.do(ctx, r)
}

// locationID returns the last path segment of the Location header, the ID
// of the object Kill Bill created, or "".
func locationID(resp upstream.Response) string {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	return path.Base(strings.TrimRight(u.Path, "/"))
}

func errorCode(body []byte) int {
	var e struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &e) != nil || e.Code == nil {
		return 0
	}
	return *e.Code
}

// Tenant is a Kill Bill tenant. The API secret is never returned.
type Tenant struct {
	TenantID    string `json:"tenantId"`
	ExternalKey string `json:"externalKey"`
	APIKey      string `json:"apiKey"`
}

// TenantByAPIKey looks a tenant up by API key with the admin credentials.
//
// Kill Bill 0.24.22 answers an unknown API key with HTTP 500 and the message
// "TenantCacheLoader cannot find value for key", not 404. That answer is
// mapped to ErrNotFound here; any other 500 is unavailability.
func (c *Client) TenantByAPIKey(ctx context.Context, apiKey string) (Tenant, error) {
	q := url.Values{"apiKey": {apiKey}}
	resp, err := c.do(ctx, request{op: "get tenant", method: http.MethodGet, path: "/1.0/kb/tenants?" + q.Encode()})
	if err != nil {
		var u *Unavailable
		if errors.As(err, &u) && u.Status == http.StatusInternalServerError && bytes.Contains(resp.Body, []byte("cannot find value for key")) {
			return Tenant{}, fmt.Errorf("killbill get tenant: %w", ErrNotFound)
		}
		return Tenant{}, err
	}
	var t Tenant
	if err := upstream.DecodeJSON(resp.Body, &t); err != nil || t.TenantID == "" {
		return Tenant{}, fmt.Errorf("killbill get tenant: unexpected response")
	}
	return t, nil
}

// CreateTenant creates a tenant and returns it. A tenant that already exists
// with that API key returns ErrConflict.
func (c *Client) CreateTenant(ctx context.Context, cc CallContext, creds TenantCredentials, externalKey string) (Tenant, error) {
	body, err := json.Marshal(map[string]string{
		"apiKey":      creds.APIKey,
		"apiSecret":   creds.APISecret,
		"externalKey": externalKey,
	})
	if err != nil {
		return Tenant{}, err
	}
	resp, err := c.do(ctx, request{op: "create tenant", method: http.MethodPost, path: "/1.0/kb/tenants",
		call: &cc, contentType: "application/json", body: body})
	if err != nil {
		return Tenant{}, err
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil {
			return Tenant{TenantID: path.Base(u.Path), ExternalKey: externalKey, APIKey: creds.APIKey}, nil
		}
	}
	return c.TenantByAPIKey(ctx, creds.APIKey)
}

// CheckTenantCredentials makes a read with the tenant credentials and returns
// ErrCredentialsRejected when Kill Bill refuses them.
func (c *Client) CheckTenantCredentials(ctx context.Context, creds TenantCredentials) error {
	_, err := c.UserKeyValues(ctx, creds, "zs-core.credential-check")
	return err
}

type keyValues struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// PluginConfig returns the per-tenant configuration of a plugin, or "" when
// none is stored.
func (c *Client) PluginConfig(ctx context.Context, creds TenantCredentials, pluginName string) (string, bool, error) {
	resp, err := c.do(ctx, request{op: "get plugin config", method: http.MethodGet,
		path: "/1.0/kb/tenants/uploadPluginConfig/" + url.PathEscape(pluginName), tenant: &creds})
	if err != nil {
		return "", false, err
	}
	var kv keyValues
	if err := upstream.DecodeJSON(resp.Body, &kv); err != nil {
		return "", false, fmt.Errorf("killbill get plugin config: unexpected response")
	}
	if len(kv.Values) == 0 {
		return "", false, nil
	}
	return kv.Values[len(kv.Values)-1], true, nil
}

// UploadPluginConfig replaces the per-tenant configuration of a plugin.
func (c *Client) UploadPluginConfig(ctx context.Context, creds TenantCredentials, cc CallContext, pluginName, config string) error {
	_, err := c.do(ctx, request{op: "upload plugin config", method: http.MethodPost,
		path: "/1.0/kb/tenants/uploadPluginConfig/" + url.PathEscape(pluginName), tenant: &creds, call: &cc,
		contentType: "text/plain", body: []byte(config)})
	return err
}

// UserKeyValues returns the values stored under a tenant user key, oldest
// first. Kill Bill keeps every value inserted under a user key.
func (c *Client) UserKeyValues(ctx context.Context, creds TenantCredentials, key string) ([]string, error) {
	resp, err := c.do(ctx, request{op: "get user key value", method: http.MethodGet,
		path: "/1.0/kb/tenants/userKeyValue/" + url.PathEscape(key), tenant: &creds})
	if err != nil {
		return nil, err
	}
	var kv keyValues
	if err := upstream.DecodeJSON(resp.Body, &kv); err != nil {
		return nil, fmt.Errorf("killbill get user key value: unexpected response")
	}
	return kv.Values, nil
}

// SetUserKeyValue replaces every value of a user key with value.
func (c *Client) SetUserKeyValue(ctx context.Context, creds TenantCredentials, cc CallContext, key, value string) error {
	p := "/1.0/kb/tenants/userKeyValue/" + url.PathEscape(key)
	if _, err := c.do(ctx, request{op: "delete user key value", method: http.MethodDelete, path: p, tenant: &creds, call: &cc}); err != nil {
		return err
	}
	_, err := c.do(ctx, request{op: "set user key value", method: http.MethodPost, path: p, tenant: &creds, call: &cc,
		contentType: "text/plain", body: []byte(value)})
	return err
}

// UploadCatalogue adds a catalogue version. Kill Bill validates it and keeps
// every version; a version whose effective date already exists is rejected.
func (c *Client) UploadCatalogue(ctx context.Context, creds TenantCredentials, cc CallContext, xml []byte) error {
	_, err := c.do(ctx, request{op: "upload catalogue", method: http.MethodPost, path: "/1.0/kb/catalog/xml",
		tenant: &creds, call: &cc, contentType: "text/xml", body: xml})
	return err
}

// UploadOverdue replaces the overdue configuration. Kill Bill 0.24.22 answers
// an invalid file with HTTP 500, so an invalid overdue file is reported as
// unavailability like every other server error.
func (c *Client) UploadOverdue(ctx context.Context, creds TenantCredentials, cc CallContext, xml []byte) error {
	_, err := c.do(ctx, request{op: "upload overdue", method: http.MethodPost, path: "/1.0/kb/overdue/xml",
		tenant: &creds, call: &cc, contentType: "text/xml", body: xml})
	return err
}
