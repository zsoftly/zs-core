package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/pkg/keycloakaudit"
)

func keycloakAuditRequest(t *testing.T, body []byte, secret []byte) *http.Request {
	t.Helper()
	timestamp := "1760000000"
	req := httptest.NewRequest(http.MethodPost, keycloakaudit.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ZS-Keycloak-Realm", "realm")
	req.Header.Set("X-ZS-Keycloak-Timestamp", timestamp)
	req.Header.Set("X-ZS-Keycloak-Signature", keycloakaudit.DeliverySignature(secret, timestamp, body))
	return req
}

func TestKeycloakAuditAuthenticationAndValidation(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	body := []byte(`{"version":1,"source_id":"keycloak","event_id":"event-1","realm_id":"realm-id","realm_name":"realm","kind":"user","occurred_at_ms":1760000000000,"failed":false,"event_type":"LOGIN"}`)
	h := New(Options{KeycloakAuditSecrets: map[string][]byte{"realm": secret}, KeycloakAuditSourceID: "keycloak", Now: func() time.Time { return time.Unix(1760000000, 0) }})
	if rec := do(t, h, keycloakAuditRequest(t, body, secret)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authenticated receiver without DB = %d %s", rec.Code, rec.Body)
	}
	req := keycloakAuditRequest(t, append(append([]byte(nil), body...), ' '), secret)
	req.Header.Set("X-ZS-Keycloak-Signature", keycloakaudit.DeliverySignature(secret, "1760000000", body))
	if rec := do(t, h, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body = %d %s", rec.Code, rec.Body)
	}
	req = keycloakAuditRequest(t, body, secret)
	req.Header.Set("X-ZS-Keycloak-Realm", "foreign")
	if rec := do(t, h, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("foreign realm = %d %s", rec.Code, rec.Body)
	}
	assertCommonHeaders(t, do(t, h, keycloakAuditRequest(t, body, secret)))
}
