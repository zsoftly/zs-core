package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// noDB fails every call; the paths under test refuse before they read.
type noDB struct{ failingDB }

func (noDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("unexpected exec")
}

func (noDB) QueryRow(context.Context, string, ...any) pgx.Row { panic("unexpected query") }

func billingServer(t *testing.T) (http.Handler, []byte) {
	t.Helper()
	kb, err := killbill.New(killbill.Config{URL: "http://127.0.0.1:1", AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(strings.Repeat("s", 32))
	svc := &billing.Service{DB: noDB{}, KillBill: kb, Secrets: secrets.Default{}, WebhookSecret: secret}
	return New(Options{DB: failingDB{}, Billing: svc}), secret
}

// Without the Kill Bill settings, billing routes answer not_ready. The
// webhook and the proxy authenticate before anything else.
func TestBillingRoutesRefuseEarly(t *testing.T) {
	h := New(Options{})
	brandID := idgen.Format(idgen.Brand, idgen.MustNewV7())
	rec := do(t, h, httptest.NewRequest(http.MethodPost, "/v1/billing/webhooks/killbill/"+brandID+"/"+strings.Repeat("0", 64), strings.NewReader(`{}`)))
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusServiceUnavailable || code != "not_ready" {
		t.Fatalf("webhook without billing = %d %s", rec.Code, rec.Body)
	}

	h, secret := billingServer(t)
	b := idgen.MustNewV7()
	for _, path := range []string{
		"/v1/billing/webhooks/killbill/" + idgen.Format(idgen.Brand, b) + "/" + strings.Repeat("0", 64),
		"/v1/billing/webhooks/killbill/" + idgen.Format(idgen.Brand, idgen.MustNewV7()) + "/" + brand.WebhookToken(secret, b),
		"/v1/billing/webhooks/killbill/not-a-brand/" + brand.WebhookToken(secret, b),
	} {
		rec := do(t, h, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"eventType":"INVOICE_CREATION"}`)))
		if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusNotFound || code != "not_found" {
			t.Fatalf("webhook %s = %d %s", path, rec.Code, rec.Body)
		}
		assertCommonHeaders(t, rec)
	}

	tenantPath := "/v1/tenants/" + idgen.Format(idgen.Tenant, idgen.MustNewV7()) + "/billing/proxy/1.0/kb/accounts/x"
	for name, auth := range map[string]string{"none": "", "api key": "Bearer zs_brand_abc", "jwt": "Bearer eyJ.x.y"} {
		req := httptest.NewRequest(http.MethodGet, tenantPath, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := do(t, h, req)
		if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusUnauthorized || code != "unauthenticated" {
			t.Fatalf("proxy with %s = %d %s", name, rec.Code, rec.Body)
		}
	}
	req := httptest.NewRequest(http.MethodGet, tenantPath, nil)
	req.Header.Set("Authorization", "Bearer "+billing.CredentialPrefix+"forged.sig")
	rec = do(t, h, req)
	if code, _ := decodeEnvelope(t, rec); rec.Code != http.StatusUnauthorized || code != "billing_token_invalid" ||
		!strings.Contains(rec.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Fatalf("proxy with a forged credential = %d %s", rec.Code, rec.Body)
	}
}
