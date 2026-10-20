package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

type failingDB struct{}

func (failingDB) Ping(context.Context) error {
	return errors.New("dial tcp 10.9.8.7:5432: connect: connection refused (user=zs_admin)")
}
func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (failingDB) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeEnvelope asserts the exact error envelope shape and returns its code.
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) (string, bool) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	var raw map[string]map[string]any
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("decode envelope: %v: %s", err, rec.Body)
	}
	if len(raw) != 1 || len(raw["error"]) != 3 {
		t.Fatalf("envelope shape = %s", rec.Body)
	}
	code, _ := raw["error"]["code"].(string)
	retryable, _ := raw["error"]["retryable"].(bool)
	if _, ok := raw["error"]["message"].(string); !ok {
		t.Fatalf("message missing: %s", rec.Body)
	}
	return code, retryable
}

func assertCommonHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if rec.Header().Get(headerRequestID) == "" {
		t.Error("X-Request-ID missing")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options missing")
	}
}

func TestHealthz(t *testing.T) {
	h := New(Options{})
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body)
	}
	assertCommonHeaders(t, rec)
}

func TestReadyzHidesDatabaseError(t *testing.T) {
	h := New(Options{DB: failingDB{}, Migrations: migrations.FS})
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	code, retryable := decodeEnvelope(t, rec)
	if code != "database_unavailable" || !retryable {
		t.Fatalf("code = %s retryable = %v", code, retryable)
	}
	for _, leak := range []string{"10.9.8.7", "zs_admin", "refused"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("response leaked %q: %s", leak, rec.Body)
		}
	}
	assertCommonHeaders(t, rec)
}

func TestReadyzWithoutDatabase(t *testing.T) {
	rec := do(t, New(Options{}), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusServiceUnavailable || code != "not_ready" {
		t.Fatalf("readyz = %d %s", rec.Code, rec.Body)
	}
}

func TestNotFoundAndMethodNotAllowedUseEnvelope(t *testing.T) {
	h := New(Options{})
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusNotFound || code != "not_found" {
		t.Fatalf("404 = %d %s", rec.Code, rec.Body)
	}
	assertCommonHeaders(t, rec)

	rec = do(t, h, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusMethodNotAllowed || code != "method_not_allowed" {
		t.Fatalf("405 = %d %s", rec.Code, rec.Body)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow = %q", allow)
	}
	assertCommonHeaders(t, rec)
}

func TestBodyLimit(t *testing.T) {
	h := New(Options{})
	body := bytes.Repeat([]byte("x"), int(MaxBodyBytes)+1)
	rec := do(t, h, httptest.NewRequest(http.MethodPost, "/healthz", bytes.NewReader(body)))
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusRequestEntityTooLarge || code != "payload_too_large" {
		t.Fatalf("413 = %d %s", rec.Code, rec.Body)
	}

	// A body without a declared length is capped while it is read.
	s := &server{log: discardLogger()}
	var readErr error
	capped := s.limitBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = r.Body.Read(make([]byte, MaxBodyBytes+10))
		for readErr == nil {
			_, readErr = r.Body.Read(make([]byte, 1024))
		}
		s.writeError(w, r, readErr)
	}))
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.ContentLength = -1
	rec = do(t, capped, req)
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusRequestEntityTooLarge || code != "payload_too_large" {
		t.Fatalf("streamed 413 = %d %s (%v)", rec.Code, rec.Body, readErr)
	}
}

func TestRequestIDEchoAndGeneration(t *testing.T) {
	h := New(Options{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(headerRequestID, "client-req-123")
	if got := do(t, h, req).Header().Get(headerRequestID); got != "client-req-123" {
		t.Fatalf("echoed request id = %q", got)
	}
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(headerRequestID, "bad id with spaces\n")
	got := do(t, h, req).Header().Get(headerRequestID)
	if _, err := idgen.ParseUUID(got); err != nil {
		t.Fatalf("invalid inbound id not replaced with a uuid: %q", got)
	}
}

func TestTraceContext(t *testing.T) {
	s := &server{log: discardLogger()}
	var seen string
	h := s.requestContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = events.TraceparentFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	do(t, h, req)
	if !strings.HasPrefix(seen, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Fatalf("inbound trace not continued: %q", seen)
	}

	do(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
	if !events.ValidTraceparent(seen) || strings.Contains(seen, "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("no local trace started: %q", seen)
	}
}

func TestPanicBecomesInternalError(t *testing.T) {
	s := &server{log: discardLogger()}
	h := s.requestContext(securityHeaders(s.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret detail: card 4242")
	}))))
	rec := do(t, h, httptest.NewRequest(http.MethodGet, "/", nil))
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusInternalServerError || code != "internal_error" {
		t.Fatalf("panic = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "4242") {
		t.Fatal("panic value leaked")
	}
	assertCommonHeaders(t, rec)
}

func TestAuditRouteWithoutTrustedHeadersIsUnauthenticated(t *testing.T) {
	h := New(Options{})
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/events", nil)
	brand, _ := idgen.New(idgen.Brand)
	tenant, _ := idgen.New(idgen.Tenant)
	req.Header.Set(headerBrand, brand)
	req.Header.Set(headerTenant, tenant)
	rec := do(t, h, req)
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusUnauthorized || code != "unauthenticated" {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
}

func TestAuditRouteScopeAndParameterErrors(t *testing.T) {
	h := New(Options{TrustedScopeHeaders: true})
	brand, _ := idgen.New(idgen.Brand)
	tenant, _ := idgen.New(idgen.Tenant)
	cases := []struct {
		name, brand, tenant, query, want string
	}{
		{"missing tenant", brand, "", "", "scope_required"},
		{"missing both", "", "", "", "scope_required"},
		{"swapped", tenant, brand, "", "scope_invalid"},
		{"bad limit", brand, tenant, "?limit=0", "invalid_limit"},
		{"huge limit", brand, tenant, "?limit=1000", "invalid_limit"},
		{"text limit", brand, tenant, "?limit=ten", "invalid_limit"},
		{"bad time", brand, tenant, "?occurred_after=yesterday", "validation_failed"},
		{"repeated", brand, tenant, "?action=a.b&action=c.d", "validation_failed"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/v1/audit/events"+c.query, nil)
		if c.brand != "" {
			req.Header.Set(headerBrand, c.brand)
		}
		if c.tenant != "" {
			req.Header.Set(headerTenant, c.tenant)
		}
		rec := do(t, h, req)
		if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusBadRequest || code != c.want {
			t.Errorf("%s: %d %s, want %s", c.name, rec.Code, rec.Body, c.want)
		}
	}
}
