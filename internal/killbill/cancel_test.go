package killbill_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
)

const (
	cancelAccount = "11111111-1111-4111-8111-111111111111"
	cancelSub     = "22222222-2222-4222-8222-222222222222"
)

// cancelServer serves the bundles of cancelAccount, which hold cancelSub, and
// answers every DELETE of a subscription with answer. It records the requests.
type cancelServer struct {
	mu      sync.Mutex
	gets    int
	deletes []*http.Request
}

func newCancelServer(t *testing.T, answer http.HandlerFunc) (*killbill.Client, *cancelServer) {
	t.Helper()
	cs := &cancelServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		switch {
		case r.Method == http.MethodGet && strings.EqualFold(r.URL.Path, "/1.0/kb/accounts/"+cancelAccount+"/bundles"):
			cs.gets++
			cs.mu.Unlock()
			_ = json.NewEncoder(w).Encode([]killbill.Bundle{{AccountID: cancelAccount, BundleID: "b1",
				Subscriptions: []killbill.Subscription{{AccountID: cancelAccount, SubscriptionID: cancelSub, ProductCategory: "BASE", State: "ACTIVE"}}}})
		case r.Method == http.MethodDelete:
			cs.deletes = append(cs.deletes, r.Clone(context.Background()))
			cs.mu.Unlock()
			answer(w, r)
		default:
			cs.mu.Unlock()
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return kb, cs
}

func (cs *cancelServer) deleteCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.deletes)
}

var immediate = killbill.CancelHow{Policy: killbill.PolicyImmediate}

// The client sends nothing unless it is told exactly how to cancel and which
// account the subscription belongs to.
func TestCancelSubscriptionOnceRefusesWhatItCannotSend(t *testing.T) {
	kb, cs := newCancelServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for name, tc := range map[string]struct {
		ctx     context.Context
		cc      killbill.CallContext
		account string
		sub     string
		how     killbill.CancelHow
	}{
		"no policy and no date":  {ctx, cc, cancelAccount, cancelSub, killbill.CancelHow{}},
		"both policy and date":   {ctx, cc, cancelAccount, cancelSub, killbill.CancelHow{Policy: killbill.PolicyImmediate, RequestedDate: "2026-12-01"}},
		"unknown policy":         {ctx, cc, cancelAccount, cancelSub, killbill.CancelHow{Policy: "SOON"}},
		"malformed date":         {ctx, cc, cancelAccount, cancelSub, killbill.CancelHow{RequestedDate: "12/01/2026"}},
		"a timestamp, not a day": {ctx, cc, cancelAccount, cancelSub, killbill.CancelHow{RequestedDate: "2026-12-01T00:00:00Z"}},
		"no audit author":        {ctx, killbill.CallContext{}, cancelAccount, cancelSub, immediate},
		"no account":             {ctx, cc, "", cancelSub, immediate},
		"no subscription":        {ctx, cc, cancelAccount, "", immediate},
		"cancelled context":      {cancelled, cc, cancelAccount, cancelSub, immediate},
	} {
		res, err := kb.CancelSubscriptionOnce(tc.ctx, creds, tc.cc, tc.account, tc.sub, tc.how)
		if err == nil || res.Outcome != killbill.NotCancelled {
			t.Errorf("%s: %+v %v, want NotCancelled and an error", name, res, err)
		}
	}
	if cs.deleteCount() != 0 {
		t.Fatalf("a refused call sent %d DELETEs", cs.deleteCount())
	}
}

// A subscription that is not in the bundles of the account it is cancelled
// for is never sent to Kill Bill: it could belong to another Kill Bill tenant,
// which Kill Bill answers with a plain server error.
func TestCancelSubscriptionOnceChecksTheAccountFirst(t *testing.T) {
	kb, cs := newCancelServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ctx := context.Background()
	res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, cancelAccount, "33333333-3333-4333-8333-333333333333", immediate)
	if err == nil || res.Outcome != killbill.NotCancelled || cs.deleteCount() != 0 || cs.gets != 1 {
		t.Fatalf("foreign subscription = %+v %v deletes=%d bundle reads=%d", res, err, cs.deleteCount(), cs.gets)
	}
	// An account whose bundles cannot be read proves nothing about the
	// subscription, so nothing is sent either.
	res, err = kb.CancelSubscriptionOnce(ctx, creds, cc, "44444444-4444-4444-8444-444444444444", cancelSub, immediate)
	if err == nil || res.Outcome != killbill.NotCancelled || cs.deleteCount() != 0 {
		t.Fatalf("unreadable account = %+v %v deletes=%d", res, err, cs.deleteCount())
	}
	res, err = kb.CancelSubscriptionOnce(ctx, creds, cc, cancelAccount, cancelSub, immediate)
	if err != nil || res.Outcome != killbill.Cancelled || cs.deleteCount() != 1 {
		t.Fatalf("own subscription = %+v %v deletes=%d", res, err, cs.deleteCount())
	}
	// The account ID is compared without regard to case.
	res, err = kb.CancelSubscriptionOnce(ctx, creds, cc, strings.ToUpper(cancelAccount), cancelSub, immediate)
	if err != nil || res.Outcome != killbill.Cancelled || cs.deleteCount() != 2 {
		t.Fatalf("repeat = %+v %v deletes=%d", res, err, cs.deleteCount())
	}
}

// What the DELETE looks like on the wire: the policy, or the requested date
// with billing aligned to it, and the audit headers of a write.
func TestCancelSubscriptionOnceWire(t *testing.T) {
	kb, cs := newCancelServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	ctx := context.Background()
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, cancelAccount, cancelSub, killbill.CancelHow{Policy: killbill.PolicyEndOfTerm}); err != nil || res.Outcome != killbill.Cancelled || res.Status != http.StatusNoContent {
		t.Fatalf("policy cancel = %+v %v", res, err)
	}
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, cancelAccount, cancelSub, killbill.CancelHow{RequestedDate: "2026-12-01"}); err != nil || res.Outcome != killbill.Cancelled {
		t.Fatalf("date cancel = %+v %v", res, err)
	}
	if len(cs.deletes) != 2 {
		t.Fatalf("deletes = %d", len(cs.deletes))
	}
	p, d := cs.deletes[0], cs.deletes[1]
	if p.URL.Path != "/1.0/kb/subscriptions/"+cancelSub || p.URL.Query().Get("entitlementPolicy") != "END_OF_TERM" || p.URL.Query().Get("billingPolicy") != "END_OF_TERM" || p.URL.Query().Has("requestedDate") {
		t.Errorf("policy request = %s", p.URL)
	}
	if d.URL.Query().Get("requestedDate") != "2026-12-01" || d.URL.Query().Get("useRequestedDateForBilling") != "true" || d.URL.Query().Has("entitlementPolicy") || d.URL.Query().Has("billingPolicy") {
		t.Errorf("date request = %s", d.URL)
	}
	for _, r := range cs.deletes {
		if r.Header.Get(killbill.HeaderAPIKey) != creds.APIKey || r.Header.Get("X-Killbill-CreatedBy") != cc.CreatedBy || r.Header.Get("X-Killbill-Reason") != cc.Reason || r.Header.Get("X-Killbill-Comment") != cc.Comment {
			t.Errorf("headers = %v", r.Header)
		}
	}
}

// What each answer proves about the cancel. Kill Bill 0.24.22 answers a
// refusal because of the subscription's state with a 500 that carries error
// code 1031, so only that code is a proven refusal; any other 5xx is unknown.
func TestCancelSubscriptionOnceClassifiesAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   killbill.CancelOutcome
		code   int
	}{
		{"cancelled", http.StatusNoContent, "", killbill.Cancelled, 0},
		{"ok with a body", http.StatusOK, "{}", killbill.Cancelled, 0},
		{"refused by state", http.StatusInternalServerError, `{"className":"org.killbill.billing.entitlement.api.EntitlementApiException","code":1031,"message":"Subscription x is in state CANCELLED: Failed to cancel"}`, killbill.CancelRefused, 1031},
		{"server error without a code", http.StatusInternalServerError, `{"className":"java.lang.IllegalStateException","code":null,"message":"boom"}`, killbill.CancelUnknown, 0},
		{"server error without a body", http.StatusInternalServerError, "", killbill.CancelUnknown, 0},
		{"another subscription's tenant", http.StatusInternalServerError, `{"className":"java.lang.IllegalStateException","code":null,"message":"Object id=x type=SUBSCRIPTION doesn't belong to tenant id=y"}`, killbill.CancelUnknown, 0},
		{"bad gateway", http.StatusBadGateway, "", killbill.CancelUnknown, 0},
		{"unavailable", http.StatusServiceUnavailable, "", killbill.CancelUnknown, 0},
		{"request timeout", http.StatusRequestTimeout, "", killbill.CancelUnknown, 0},
		{"too many requests", http.StatusTooManyRequests, "", killbill.CancelUnknown, 0},
		{"redirect", http.StatusFound, "", killbill.CancelUnknown, 0},
		{"unknown subscription", http.StatusNotFound, `{"className":"org.killbill.billing.util.callcontext.InternalCallContextFactory$ObjectDoesNotExist","code":null,"message":"Object id=x type=SUBSCRIPTION doesn't exist!"}`, killbill.CancelNotFound, 0},
		{"not found page", http.StatusNotFound, "<!doctype html>", killbill.CancelNotFound, 0},
		{"requested date in the past", http.StatusBadRequest, `{"className":"org.killbill.billing.entitlement.api.EntitlementApiException","code":1001,"message":"Requested date is prior to the previous transition"}`, killbill.NotCancelled, 1001},
		{"unexpected bad request", http.StatusBadRequest, `{"className":"java.lang.NullPointerException","code":null,"message":null}`, killbill.NotCancelled, 0},
		{"credentials rejected", http.StatusUnauthorized, "<!doctype html>", killbill.NotCancelled, 0},
		{"forbidden", http.StatusForbidden, "", killbill.NotCancelled, 0},
		{"conflict", http.StatusConflict, "", killbill.NotCancelled, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kb, cs := newCancelServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			res, err := kb.CancelSubscriptionOnce(context.Background(), creds, cc, cancelAccount, cancelSub, immediate)
			if res.Outcome != tc.want || res.Status != tc.status || res.Code != tc.code {
				t.Fatalf("result = %+v, want outcome %v status %d code %d (err %v)", res, tc.want, tc.status, tc.code, err)
			}
			if tc.want == killbill.Cancelled != (err == nil) {
				t.Fatalf("error = %v for outcome %v", err, res.Outcome)
			}
			if cs.deleteCount() != 1 {
				t.Fatalf("sent %d DELETEs, want exactly 1 and no retry", cs.deleteCount())
			}
			if err != nil && (strings.Contains(err.Error(), creds.APISecret) || strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "doesn't belong")) {
				t.Fatalf("error carries credentials or Kill Bill's text: %v", err)
			}
		})
	}
}

// Only a failure before the request was written proves nothing was sent.
func TestCancelSubscriptionOnceTransportFailures(t *testing.T) {
	t.Run("connection reset after the request", func(t *testing.T) {
		kb, cs := newCancelServer(t, func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
		res, err := kb.CancelSubscriptionOnce(context.Background(), creds, cc, cancelAccount, cancelSub, immediate)
		if err == nil || res.Outcome != killbill.CancelUnknown || res.Status != 0 || cs.deleteCount() != 1 {
			t.Fatalf("reset = %+v %v deletes=%d", res, err, cs.deleteCount())
		}
	})
	t.Run("timeout while Kill Bill works", func(t *testing.T) {
		release := make(chan struct{})
		kb, _ := newCancelServer(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, cancelAccount, cancelSub, immediate)
		if err == nil || res.Outcome != killbill.CancelUnknown {
			t.Fatalf("timeout = %+v %v", res, err)
		}
	})
	t.Run("connection refused for the cancel after the bundles were read", func(t *testing.T) {
		// The bundles read succeeds and then the listener closes, so only the
		// DELETE finds the connection refused. Keep-alives are off so the
		// cancel opens a new connection instead of reusing the read's.
		var srv *httptest.Server
		var mu sync.Mutex
		deletes := 0
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				mu.Lock()
				deletes++
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = srv.Listener.Close()
			_ = json.NewEncoder(w).Encode([]killbill.Bundle{{AccountID: cancelAccount, BundleID: "b1",
				Subscriptions: []killbill.Subscription{{AccountID: cancelAccount, SubscriptionID: cancelSub, State: "ACTIVE"}}}})
		}))
		t.Cleanup(srv.Close)
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw", HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		res, err := kb.CancelSubscriptionOnce(context.Background(), creds, cc, cancelAccount, cancelSub, immediate)
		mu.Lock()
		defer mu.Unlock()
		if err == nil || res.Outcome != killbill.NotCancelled || res.Status != 0 || deletes != 0 {
			t.Fatalf("refused cancel = %+v %v deletes=%d, want NotCancelled, status 0 and no DELETE seen", res, err, deletes)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		kb, err := killbill.New(killbill.Config{URL: url, AdminUser: "admin", AdminPassword: "pw"})
		if err != nil {
			t.Fatal(err)
		}
		res, err := kb.CancelSubscriptionOnce(context.Background(), creds, cc, cancelAccount, cancelSub, immediate)
		if err == nil || res.Outcome != killbill.NotCancelled {
			t.Fatalf("refused = %+v %v", res, err)
		}
	})
}

func TestCancelEffectiveDate(t *testing.T) {
	for _, tc := range []struct {
		name, cancelled, want string
		wantErr               bool
	}{
		{"timestamp", "2026-11-24T20:47:15.000Z", "2026-11-24", false},
		{"timestamp with offset", "2026-11-24T23:30:00.000-05:00", "2026-11-25", false},
		{"date only", "2026-11-03", "2026-11-03", false},
		{"empty", "", "", true},
		{"garbage", "soon", "", true},
	} {
		got, err := killbill.CancelEffectiveDate(killbill.Subscription{CancelledDate: tc.cancelled})
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s: %q %v, want %q (error %v)", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

// The fake behaves as the real Kill Bill 0.24.22 stack does in the cases
// TestSubscriptionCancelOnTheStack checks.
func TestFakeCancelFollowsKillBill(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, acct := fakeTenant(t)
	base, err := kb.CreateSubscription(ctx, creds, cc, acct, enginetest.PlanUsage, "zs-base")
	if err != nil {
		t.Fatal(err)
	}
	baseSub, err := kb.Subscription(ctx, creds, base)
	if err != nil {
		t.Fatal(err)
	}
	addOn, err := kb.CreateAddOn(ctx, creds, cc, acct, baseSub.BundleID, enginetest.PlanUsage, "zs-addon")
	if err != nil || addOn == "" {
		t.Fatalf("add-on = %q %v", addOn, err)
	}
	bundles, err := kb.AccountBundles(ctx, creds, acct)
	if err != nil || len(bundles) != 1 || len(bundles[0].Subscriptions) != 2 {
		t.Fatalf("bundles = %+v %v", bundles, err)
	}
	categories := map[string]string{}
	for _, s := range bundles[0].Subscriptions {
		categories[s.SubscriptionID] = s.ProductCategory
		if s.State != "ACTIVE" || s.CancelledDate != "" || s.BundleID != baseSub.BundleID {
			t.Fatalf("new subscription = %+v", s)
		}
	}
	if categories[base] != "BASE" || categories[addOn] != "ADD_ON" {
		t.Fatalf("categories = %v", categories)
	}
	// An add-on needs a bundle of the account.
	if _, err := kb.CreateAddOn(ctx, creds, cc, acct, "99999999-9999-4999-8999-999999999999", enginetest.PlanUsage, "zs-orphan"); err == nil {
		t.Fatal("an add-on on an unknown bundle was created")
	}

	// Cancelling the base cancels its add-on with the same date.
	res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, base, immediate)
	if err != nil || res.Outcome != killbill.Cancelled {
		t.Fatalf("cancel = %+v %v", res, err)
	}
	for _, id := range []string{base, addOn} {
		s, err := kb.Subscription(ctx, creds, id)
		day, dayErr := killbill.CancelEffectiveDate(s)
		if err != nil || s.State != "CANCELLED" || dayErr != nil || day != enginetest.CancelDay {
			t.Fatalf("after the base cancel %s = %+v (%q %v) %v", id, s, day, dayErr, err)
		}
	}
	// A repeat, and an add-on whose base is cancelled, are state refusals
	// with code 1031, not unknown outcomes.
	for _, id := range []string{base, addOn} {
		res, err = kb.CancelSubscriptionOnce(ctx, creds, cc, acct, id, immediate)
		if err == nil || res.Outcome != killbill.CancelRefused || res.Code != 1031 || res.Status != http.StatusInternalServerError {
			t.Fatalf("repeat cancel of %s = %+v %v", id, res, err)
		}
	}

	// Another tenant's subscription is a server error in Kill Bill; the client
	// refuses it before sending, and the fake answers it as Kill Bill does.
	f.AddKillBillTenant("zs-other", "secret")
	other := killbill.TenantCredentials{APIKey: "zs-other", APISecret: "secret"}
	_, err = kb.Subscription(ctx, other, base)
	var unavailable *killbill.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Status != http.StatusInternalServerError {
		t.Fatalf("foreign read = %v", err)
	}
	if res, err := kb.CancelSubscriptionOnce(ctx, other, cc, acct, base, immediate); err == nil || res.Outcome != killbill.NotCancelled {
		t.Fatalf("foreign cancel = %+v %v", res, err)
	}
}

func TestFakeScheduledCancels(t *testing.T) {
	ctx := context.Background()
	_, kb, creds, acct := fakeTenant(t)
	newSub := func(key string) string {
		id, err := kb.CreateSubscription(ctx, creds, cc, acct, enginetest.PlanUsage, key)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// END_OF_TERM leaves the subscription active and can be repeated; an
	// immediate cancel then ends it.
	eot := newSub("zs-eot")
	for i := 0; i < 2; i++ {
		if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, eot, killbill.CancelHow{Policy: killbill.PolicyEndOfTerm}); err != nil || res.Outcome != killbill.Cancelled {
			t.Fatalf("end of term %d = %+v %v", i, res, err)
		}
	}
	if s, err := kb.Subscription(ctx, creds, eot); err != nil || s.State != "ACTIVE" || s.CancelledDate == "" {
		t.Fatalf("scheduled = %+v %v", s, err)
	}
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, eot, immediate); err != nil || res.Outcome != killbill.Cancelled {
		t.Fatalf("immediate after scheduled = %+v %v", res, err)
	}
	if s, _ := kb.Subscription(ctx, creds, eot); s.State != "CANCELLED" {
		t.Fatalf("state = %q", s.State)
	}

	// A requested date alone cancels the entitlement on that day.
	dated := newSub("zs-dated")
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, dated, killbill.CancelHow{RequestedDate: "2026-12-15"}); err != nil || res.Outcome != killbill.Cancelled {
		t.Fatalf("dated = %+v %v", res, err)
	}
	s, err := kb.Subscription(ctx, creds, dated)
	day, _ := killbill.CancelEffectiveDate(s)
	if err != nil || s.State != "ACTIVE" || day != "2026-12-15" {
		t.Fatalf("dated read = %+v (%q) %v", s, day, err)
	}
	// The same day again is accepted, an earlier day moves the cancel earlier,
	// and a later day is refused because the cancel is already pending.
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, dated, killbill.CancelHow{RequestedDate: "2026-12-15"}); err != nil || res.Outcome != killbill.Cancelled {
		t.Fatalf("repeated dated cancel = %+v %v", res, err)
	}
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, dated, killbill.CancelHow{RequestedDate: "2026-12-20"}); err == nil || res.Outcome != killbill.CancelRefused || res.Code != 1031 {
		t.Fatalf("later dated cancel = %+v %v", res, err)
	}
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, dated, killbill.CancelHow{RequestedDate: "2026-12-10"}); err != nil || res.Outcome != killbill.Cancelled {
		t.Fatalf("earlier dated cancel = %+v %v", res, err)
	}
	if s, _ := kb.Subscription(ctx, creds, dated); s.State != "ACTIVE" {
		t.Fatalf("state = %q", s.State)
	} else if day, _ := killbill.CancelEffectiveDate(s); day != "2026-12-10" {
		t.Fatalf("moved cancel day = %q", day)
	}
	// A date before today is refused with code 1001.
	past := newSub("zs-past")
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, past, killbill.CancelHow{RequestedDate: "2026-01-02"}); err == nil || res.Outcome != killbill.NotCancelled || res.Code != 1001 {
		t.Fatalf("past date = %+v %v", res, err)
	}
	// A date of today cancels at once.
	today := newSub("zs-today")
	if res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, today, killbill.CancelHow{RequestedDate: enginetest.CancelDay}); err != nil || res.Outcome != killbill.Cancelled {
		t.Fatalf("today = %+v %v", res, err)
	}
	if s, _ := kb.Subscription(ctx, creds, today); s.State != "CANCELLED" {
		t.Fatalf("state = %q", s.State)
	}
}

// The fake can apply a cancel and then lose or drop the answer, and it keeps
// every cancel request with its query and headers.
func TestFakeCancelAnswerLossAndCapture(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, acct := fakeTenant(t)
	id, err := kb.CreateSubscription(ctx, creds, cc, acct, enginetest.PlanUsage, "zs-lost")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.CancelRequests("zs-test"); len(got) != 0 {
		t.Fatalf("requests before any cancel = %+v", got)
	}
	f.LoseAnswers("/1.0/kb/subscriptions", 1)
	res, err := kb.CancelSubscriptionOnce(ctx, creds, cc, acct, id, immediate)
	if err == nil || res.Outcome != killbill.CancelUnknown || res.Status != http.StatusServiceUnavailable {
		t.Fatalf("lost answer = %+v %v", res, err)
	}
	if s, _ := kb.Subscription(ctx, creds, id); s.State != "CANCELLED" {
		t.Fatalf("a lost answer must leave the cancel applied, state = %q", s.State)
	}
	second, err := kb.CreateSubscription(ctx, creds, cc, acct, enginetest.PlanUsage, "zs-dropped")
	if err != nil {
		t.Fatal(err)
	}
	f.DropAnswers("/1.0/kb/subscriptions", 1)
	res, err = kb.CancelSubscriptionOnce(ctx, creds, cc, acct, second, killbill.CancelHow{RequestedDate: "2026-12-20"})
	if err == nil || res.Outcome != killbill.CancelUnknown || res.Status != 0 {
		t.Fatalf("dropped answer = %+v %v", res, err)
	}
	if s, _ := kb.Subscription(ctx, creds, second); s.CancelledDate == "" {
		t.Fatal("a dropped answer must leave the cancel applied")
	}
	got := f.CancelRequests("zs-test")
	if len(got) != 2 {
		t.Fatalf("captured = %+v", got)
	}
	if got[0].Path != "/1.0/kb/subscriptions/"+id || got[0].Query.Get("entitlementPolicy") != "IMMEDIATE" || got[0].Header.Get("X-Killbill-CreatedBy") != cc.CreatedBy {
		t.Errorf("first capture = %+v", got[0])
	}
	if got[1].Query.Get("requestedDate") != "2026-12-20" || got[1].Query.Get("useRequestedDateForBilling") != "true" {
		t.Errorf("second capture = %+v", got[1])
	}
	// The caller gets copies, and each Kill Bill tenant has its own.
	got[0].Query.Set("entitlementPolicy", "changed")
	if again := f.CancelRequests("zs-test"); again[0].Query.Get("entitlementPolicy") != "IMMEDIATE" {
		t.Fatal("CancelRequests returned the fake's own data")
	}
	if other := f.CancelRequests("nobody"); len(other) != 0 {
		t.Fatalf("another tenant's captures = %+v", other)
	}
}
