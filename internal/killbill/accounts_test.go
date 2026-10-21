package killbill

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSetAccountEmailSendsOnlyExplicitEmail(t *testing.T) {
	for _, email := range []string{"current-owner@example.test", ""} {
		name := "set"
		if email == "" {
			name = "clear"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPut || r.URL.Path != "/1.0/kb/accounts/account-a" || r.URL.RawQuery != "treatNullAsReset=false" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				user, password, ok := r.BasicAuth()
				if !ok || user != "admin" || password != "test-password" {
					t.Error("missing expected admin authentication")
				}
				for header, want := range map[string]string{
					HeaderAPIKey: "brand-a", HeaderAPISecret: "tenant-secret",
					HeaderCreatedBy: "recipient-test", HeaderReason: "recipient reconciliation", HeaderComment: "correlation-a",
					"Content-Type": "application/json",
				} {
					if r.Header.Get(header) != want {
						t.Errorf("unexpected %s header", header)
					}
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode account update: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var got string
				value, present := body["email"]
				if !present || len(body) != 1 || string(value) == "null" || json.Unmarshal(value, &got) != nil || got != email {
					t.Errorf("email update must contain exactly one explicit email string: %s", value)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(srv.Close)
			client, err := New(Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "test-password"})
			if err != nil {
				t.Fatal(err)
			}
			err = client.SetAccountEmail(context.Background(), TenantCredentials{APIKey: "brand-a", APISecret: "tenant-secret"},
				CallContext{CreatedBy: "recipient-test", Reason: "recipient reconciliation", Comment: "correlation-a"}, "account-a", email)
			if err != nil || calls.Load() != 1 {
				t.Fatalf("set email: calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}

func TestAccountEmailsValidatesScopedReadOnlyCollection(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		status, count int
		bad           bool
	}{
		{name: "empty", body: `[]`, count: 0},
		{name: "populated", body: `[{"accountId":"account-a","email":"private-cc@example.test"}]`, count: 1},
		{name: "null", body: `null`, bad: true},
		{name: "object", body: `{}`, bad: true},
		{name: "malformed", body: `[`, bad: true},
		{name: "missing email", body: `[{"accountId":"account-a"}]`, bad: true},
		{name: "null email", body: `[{"accountId":"account-a","email":null}]`, bad: true},
		{name: "blank email", body: `[{"accountId":"account-a","email":"  "}]`, bad: true},
		{name: "wrong account", body: `[{"accountId":"account-b","email":"private-cc@example.test"}]`, bad: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, body: `private-cc@example.test`, bad: true},
		{name: "credentials", status: http.StatusUnauthorized, body: `private-cc@example.test`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/1.0/kb/accounts/account-a/emails" || r.URL.RawQuery != "" {
					t.Errorf("unexpected collection request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get(HeaderAPIKey) != creds.APIKey || r.Header.Get(HeaderAPISecret) != creds.APISecret || r.Header.Get(HeaderCreatedBy) != "" {
					t.Error("collection read has incorrect scope or write audit context")
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			client, err := New(Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.AccountEmails(context.Background(), creds, "account-a")
			if (err != nil) != tc.bad || (!tc.bad && (got == nil || len(got) != tc.count)) || calls.Load() != 1 {
				t.Fatalf("collection result: count=%d calls=%d error=%v", len(got), calls.Load(), err)
			}
			if err != nil && strings.Contains(err.Error(), "private-cc") {
				t.Fatal("error discloses address")
			}
			if tc.status == http.StatusUnauthorized && !errors.Is(err, ErrCredentialsRejected) {
				t.Fatalf("credential error: %v", err)
			}
			if tc.status == http.StatusServiceUnavailable && !IsUnavailable(err) {
				t.Fatalf("availability error: %v", err)
			}
		})
	}
}

func TestSetAccountEmailReportsUpstreamFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		want        error
		unavailable bool
	}{
		{name: "missing account", status: http.StatusNotFound, want: ErrNotFound},
		{name: "rejected credentials", status: http.StatusUnauthorized, want: ErrCredentialsRejected},
		{name: "unavailable", status: http.StatusServiceUnavailable, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := clientFor(t, tc.status, `{}`).SetAccountEmail(context.Background(), creds, CallContext{CreatedBy: "recipient-test"}, "account-a", "")
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || IsUnavailable(err) != tc.unavailable {
				t.Fatalf("HTTP %d: %v", tc.status, err)
			}
		})
	}
}
