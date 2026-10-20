package killbill

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func clientFor(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var creds = TenantCredentials{APIKey: "zs-reference", APISecret: "s"}

func TestServerErrorsAreUnavailable(t *testing.T) {
	ctx := context.Background()
	cc := CallContext{CreatedBy: "test"}
	for _, status := range []int{500, 501, 502, 503, 504, 599, http.StatusTooManyRequests} {
		err := clientFor(t, status, `{"code":1}`).UploadOverdue(ctx, creds, cc, []byte("<overdue/>"))
		if !IsUnavailable(err) {
			t.Errorf("HTTP %d: %v", status, err)
		}
		var kbErr *Error
		if errors.As(err, &kbErr) {
			t.Errorf("HTTP %d is also a rejection", status)
		}
	}
}

func TestClientErrorsAreRejections(t *testing.T) {
	ctx := context.Background()
	cc := CallContext{CreatedBy: "test"}
	err := clientFor(t, http.StatusBadRequest, `{"code":2021,"message":"x"}`).UploadCatalogue(ctx, creds, cc, []byte("<catalog/>"))
	var kbErr *Error
	if !errors.As(err, &kbErr) || kbErr.Status != 400 || kbErr.Code != 2021 || IsUnavailable(err) {
		t.Fatalf("400: %v", err)
	}
	if err := clientFor(t, http.StatusUnauthorized, "").CheckTenantCredentials(ctx, creds); !errors.Is(err, ErrCredentialsRejected) {
		t.Fatalf("401: %v", err)
	}
	if _, err := clientFor(t, http.StatusNotFound, "").UserKeyValues(ctx, creds, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
}

// Kill Bill 0.24.22 answers an unknown tenant API key with a 500 that names
// the tenant cache. That one 500 is not found; any other is unavailable.
func TestTenantByAPIKeyUnknownTenant(t *testing.T) {
	ctx := context.Background()
	unknown := `{"message":"TenantCacheLoader cannot find value for key zs-x"}`
	if _, err := clientFor(t, 500, unknown).TenantByAPIKey(ctx, "zs-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	if _, err := clientFor(t, 500, `{"message":"database down"}`).TenantByAPIKey(ctx, "zs-x"); !IsUnavailable(err) {
		t.Fatalf("other 500: %v", err)
	}
}
