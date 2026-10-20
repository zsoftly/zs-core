// Package keycloak is a small client for the Keycloak 26.8 Admin REST API and
// a per-realm cache of OIDC discovery and signing keys.
//
// It covers what brand apply and token verification need and nothing more:
// realms, clients and their protocol mappers, client secrets, identity
// providers, authentication flows, executions and their configuration, and
// required actions. Every endpoint was checked against Keycloak 26.8.1.
//
// The admin token comes from the client credentials grant when an admin
// client ID and secret are configured, otherwise from the password grant of
// the admin-cli client in the master realm. Tokens and secrets are never part
// of an error.
package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zsoftly/zs-core/internal/upstream"
)

// AdminRealm is the realm the admin token is obtained from.
const AdminRealm = "master"

// Config configures a Client.
type Config struct {
	URL               string
	AdminUser         string
	AdminPassword     string
	AdminClientID     string
	AdminClientSecret string
	HTTPClient        *http.Client
}

// Client talks to the Admin REST API.
type Client struct {
	base string
	cfg  Config
	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// ErrNotFound is returned when Keycloak answers 404.
var ErrNotFound = errors.New("keycloak: not found")

// ErrConflict is returned when Keycloak answers 409.
var ErrConflict = errors.New("keycloak: conflict")

// Error is a non-2xx answer. Message is Keycloak's own error text, which
// never contains a request value that Core marks secret.
type Error struct {
	Op      string
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("keycloak %s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("keycloak %s: HTTP %d: %s", e.Op, e.Status, e.Message)
}

// Unavailable wraps transport failures and 5xx answers.
type Unavailable struct {
	Op  string
	Err error
}

func (e *Unavailable) Error() string { return "keycloak " + e.Op + " unavailable: " + e.Err.Error() }
func (e *Unavailable) Unwrap() error { return e.Err }

// IsUnavailable reports whether err means Keycloak could not be reached or
// failed on its side.
func IsUnavailable(err error) bool {
	var u *Unavailable
	return errors.As(err, &u)
}

// New returns a client. URL is required, and either the admin user and
// password or the admin client ID and secret.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("keycloak: URL is required")
	}
	hasClient := cfg.AdminClientID != "" && cfg.AdminClientSecret != ""
	hasUser := cfg.AdminUser != "" && cfg.AdminPassword != ""
	if !hasClient && !hasUser {
		return nil, errors.New("keycloak: admin credentials are required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = upstream.NewClient()
	}
	return &Client{base: strings.TrimRight(cfg.URL, "/"), cfg: cfg, http: hc}, nil
}

// adminToken returns a cached admin access token, refreshing it 30 seconds
// before it expires.
func (c *Client) adminToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{}
	if c.cfg.AdminClientID != "" && c.cfg.AdminClientSecret != "" {
		form.Set("grant_type", "client_credentials")
		form.Set("client_id", c.cfg.AdminClientID)
		form.Set("client_secret", c.cfg.AdminClientSecret)
	} else {
		form.Set("grant_type", "password")
		form.Set("client_id", "admin-cli")
		form.Set("username", c.cfg.AdminUser)
		form.Set("password", c.cfg.AdminPassword)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/realms/"+AdminRealm+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := upstream.Do(c.http, req)
	if err != nil {
		return "", &Unavailable{Op: "admin login", Err: err}
	}
	if resp.Status >= 500 {
		return "", &Unavailable{Op: "admin login", Err: fmt.Errorf("HTTP %d", resp.Status)}
	}
	if resp.Status != http.StatusOK {
		// The body can echo the grant type and client but never the
		// password; it is still left out.
		return "", &Error{Op: "admin login", Status: resp.Status}
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := upstream.DecodeJSON(resp.Body, &tok); err != nil || tok.AccessToken == "" {
		return "", &Error{Op: "admin login", Status: resp.Status, Message: "no access token in response"}
	}
	lifetime := time.Duration(tok.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 60 * time.Second
	}
	c.token = tok.AccessToken
	c.expires = time.Now().Add(lifetime - 30*time.Second)
	return c.token, nil
}

func (c *Client) dropToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// call sends an admin request. body is JSON-encoded when not nil; out is
// decoded from a 2xx answer when not nil. It returns the response so callers
// can read headers such as Location.
func (c *Client) call(ctx context.Context, op, method, path string, body, out any) (upstream.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return upstream.Response{}, fmt.Errorf("keycloak %s: encode: %w", op, err)
		}
	}
	for attempt := 0; ; attempt++ {
		token, err := c.adminToken(ctx)
		if err != nil {
			return upstream.Response{}, err
		}
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
		if err != nil {
			return upstream.Response{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := upstream.Do(c.http, req)
		if err != nil {
			return resp, &Unavailable{Op: op, Err: err}
		}
		if resp.Status == http.StatusUnauthorized && attempt == 0 {
			c.dropToken()
			continue
		}
		switch {
		case resp.OK():
			if out != nil && len(resp.Body) > 0 {
				if err := upstream.DecodeJSON(resp.Body, out); err != nil {
					return resp, fmt.Errorf("keycloak %s: %w", op, err)
				}
			}
			return resp, nil
		case resp.Status == http.StatusNotFound:
			return resp, fmt.Errorf("keycloak %s: %w", op, ErrNotFound)
		case resp.Status == http.StatusConflict:
			return resp, fmt.Errorf("keycloak %s: %w: %s", op, ErrConflict, errorMessage(resp.Body))
		case resp.Status >= 500 || upstream.Temporary(resp.Status):
			return resp, &Unavailable{Op: op, Err: fmt.Errorf("HTTP %d", resp.Status)}
		default:
			return resp, &Error{Op: op, Status: resp.Status, Message: errorMessage(resp.Body)}
		}
	}
}

// errorMessage extracts Keycloak's error text from an error body.
func errorMessage(body []byte) string {
	var e struct {
		Error            string `json:"error"`
		ErrorMessage     string `json:"errorMessage"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	for _, m := range []string{e.ErrorMessage, e.ErrorDescription, e.Error} {
		if m != "" {
			return upstream.Snippet([]byte(m))
		}
	}
	return ""
}

func realmPath(realm string) string { return "/admin/realms/" + url.PathEscape(realm) }

// ServerVersion returns the Keycloak version from the server info.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var info struct {
		SystemInfo struct {
			Version string `json:"version"`
		} `json:"systemInfo"`
	}
	if _, err := c.call(ctx, "server info", http.MethodGet, "/admin/serverinfo", nil, &info); err != nil {
		return "", err
	}
	if info.SystemInfo.Version == "" {
		return "", errors.New("keycloak server info: no version")
	}
	return info.SystemInfo.Version, nil
}
