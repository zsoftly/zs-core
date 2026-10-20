// Package upstream holds the small HTTP helpers shared by the Keycloak and
// Kill Bill clients and by `zs-core doctor`.
//
// Responses are read up to MaxBodyBytes. Nothing here logs: callers decide
// what of a response is safe to keep, and request bodies, which may hold
// secrets, never appear in an error.
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxBodyBytes caps how much of a response body is read.
const MaxBodyBytes = 4 << 20

// DefaultTimeout is the timeout of NewClient.
const DefaultTimeout = 20 * time.Second

// NewClient returns an HTTP client with DefaultTimeout that does not follow
// redirects, so credentials are never replayed to another location.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: DefaultTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Response is a fully read response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// OK reports a 2xx status.
func (r Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Do sends req and reads the body. A transport error is returned as is; a
// non-2xx status is not an error here.
func Do(client *http.Client, req *http.Request) (Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes))
	if err != nil {
		return Response{Status: resp.StatusCode, Header: resp.Header}, fmt.Errorf("read response: %w", err)
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// DecodeJSON decodes body into out.
func DecodeJSON(body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// Snippet returns at most 200 characters of body on one line, for an error
// that is logged. Callers must only use it on bodies that cannot hold
// secrets, such as upstream error messages.
func Snippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// Temporary reports whether status means the upstream is down or overloaded
// rather than that it rejected the request.
func Temporary(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout || status == http.StatusTooManyRequests
}
