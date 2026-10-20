package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/pkg/identity"
)

func TestApplyGateNeedsRecentMFAAndAReason(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	fields := []string{"identity.mfa"}
	recent := identity.Principal{Kind: identity.KindStaff, MFA: true, AuthTime: now.Add(-5 * time.Minute)}
	if err := applyGate(recent, "rotate the staff secret", clock)(fields); err != nil {
		t.Fatalf("recent MFA and a reason: %v", err)
	}
	cases := []struct {
		name   string
		p      identity.Principal
		reason string
		want   string
	}{
		{"no MFA", identity.Principal{Kind: identity.KindStaff, AuthTime: now.Add(-time.Minute)}, "why", "recent_mfa_required"},
		{"MFA too old", identity.Principal{Kind: identity.KindStaff, MFA: true, AuthTime: now.Add(-16 * time.Minute)}, "why", "recent_mfa_required"},
		{"no reason", recent, "", "reason_required"},
		{"blank reason", recent, "  ", "reason_required"},
	}
	for _, c := range cases {
		if got := errName(applyGate(c.p, c.reason, clock)(fields)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestApplyContextOutlivesTheClient(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/brands/x/apply", nil).WithContext(parent)
	ctx, stop := applyContext(req)
	defer stop()
	cancel()
	if ctx.Err() != nil {
		t.Fatal("a client disconnect cancelled the apply")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) < ApplyTimeout-time.Minute || time.Until(deadline) > ApplyTimeout {
		t.Fatalf("deadline in %v, want %v", time.Until(deadline), ApplyTimeout)
	}
}

// The route answers after the server's write timeout because it extends its
// own write deadline, through the middleware's response writer.
func TestExtendWriteDeadline(t *testing.T) {
	s := &server{log: discardLogger(), now: time.Now}
	slow := func(extend bool) http.Handler {
		return s.requestContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if extend && !extendWriteDeadline(w, time.Second) {
				t.Error("write deadline not supported through the middleware")
			}
			time.Sleep(150 * time.Millisecond)
			_, _ = io.WriteString(w, "applied")
		}))
	}
	for _, extend := range []bool{true, false} {
		srv := httptest.NewUnstartedServer(slow(extend))
		srv.Config.WriteTimeout = 50 * time.Millisecond
		srv.Start()
		resp, err := http.Get(srv.URL)
		var body []byte
		if err == nil {
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		srv.Close()
		if extend && (err != nil || string(body) != "applied") {
			t.Fatalf("extended: %q, %v", body, err)
		}
		if !extend && err == nil && string(body) == "applied" {
			t.Fatal("the control case answered after the write timeout; the test proves nothing")
		}
	}
}
