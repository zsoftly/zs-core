package killbill_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
)

func usageRecord(sub string) killbill.SubscriptionUsageRecord {
	return killbill.SubscriptionUsageRecord{SubscriptionID: sub, TrackingID: "zs-usg-abc", UnitUsageRecords: []killbill.UnitUsageRecords{
		{UnitType: enginetest.UnitAPICall, UsageRecords: []killbill.UsageRecord{{RecordDate: "2026-10-20", Amount: "2.5"}}}}}
}

func usagePayload(t *testing.T, sub string) []byte {
	t.Helper()
	p, err := usageRecord(sub).Payload()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func serverWith(t *testing.T, h http.HandlerFunc) *killbill.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return kb
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

var creds = killbill.TenantCredentials{APIKey: "zs-test", APISecret: "secret"}

const subID = "11111111-2222-3333-4444-555555555555"

// The bytes a caller stores as send_payload are the bytes Kill Bill gets.
func TestUsagePayloadIsTheWireBody(t *testing.T) {
	var got []byte
	var path, ct string
	kb := serverWith(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		path, ct = r.URL.Path, r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusCreated)
	})
	payload := usagePayload(t, subID)
	again, _ := usageRecord(subID).Payload()
	if string(payload) != string(again) {
		t.Fatalf("payload is not deterministic: %s vs %s", payload, again)
	}
	want := `{"subscriptionId":"` + subID + `","trackingId":"zs-usg-abc","unitUsageRecords":[{"unitType":"api-call","usageRecords":[{"recordDate":"2026-10-20","amount":2.5}]}]}`
	if string(payload) != want {
		t.Fatalf("payload = %s", payload)
	}
	res, err := kb.SendUsage(context.Background(), creds, cc, payload)
	if err != nil || res.Outcome != killbill.UsageSent || res.Status != http.StatusCreated {
		t.Fatalf("send = %+v %v", res, err)
	}
	if string(got) != string(payload) || path != "/1.0/kb/usages" || ct != "application/json" {
		t.Fatalf("wire = %s %s %s", got, path, ct)
	}
}

func TestUsagePayloadRejectsIncompleteRecords(t *testing.T) {
	bad := map[string]func(*killbill.SubscriptionUsageRecord){
		"no subscription": func(r *killbill.SubscriptionUsageRecord) { r.SubscriptionID = "" },
		"no tracking id":  func(r *killbill.SubscriptionUsageRecord) { r.TrackingID = "" },
		"no units":        func(r *killbill.SubscriptionUsageRecord) { r.UnitUsageRecords = nil },
		"no unit type":    func(r *killbill.SubscriptionUsageRecord) { r.UnitUsageRecords[0].UnitType = "" },
		"no records":      func(r *killbill.SubscriptionUsageRecord) { r.UnitUsageRecords[0].UsageRecords = nil },
		"no date":         func(r *killbill.SubscriptionUsageRecord) { r.UnitUsageRecords[0].UsageRecords[0].RecordDate = "" },
		"empty amount":    func(r *killbill.SubscriptionUsageRecord) { r.UnitUsageRecords[0].UsageRecords[0].Amount = "" },
		"zero amount":     func(r *killbill.SubscriptionUsageRecord) { r.UnitUsageRecords[0].UsageRecords[0].Amount = "0" },
		"negative amount": func(r *killbill.SubscriptionUsageRecord) { r.UnitUsageRecords[0].UsageRecords[0].Amount = "-1" },
		"non numeric":     func(r *killbill.SubscriptionUsageRecord) { r.UnitUsageRecords[0].UsageRecords[0].Amount = "1e9999x" },
		"too many decimals": func(r *killbill.SubscriptionUsageRecord) {
			r.UnitUsageRecords[0].UsageRecords[0].Amount = "0.0000000001"
		},
	}
	for name, mutate := range bad {
		rec := usageRecord(subID)
		mutate(&rec)
		if p, err := rec.Payload(); err == nil {
			t.Errorf("%s: payload %s", name, p)
		}
	}
}

// send_payload is stored as jsonb, which reorders keys; reading it back must
// give the same record.
func TestParseUsagePayload(t *testing.T) {
	want := usagePayload(t, subID)
	reordered := `{"unitUsageRecords":[{"usageRecords":[{"amount":2.5,"recordDate":"2026-10-20"}],"unitType":"api-call"}],"trackingId":"zs-usg-abc","subscriptionId":"` + subID + `"}`
	rec, err := killbill.ParseUsagePayload([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := rec.Payload(); string(p) != string(want) {
		t.Fatalf("round trip = %s", p)
	}
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(string(want), `"trackingId"`, `"extra":1,"trackingId"`, 1),
		"trailing data": string(want) + `{}`,
		"not an object": `[]`,
		"incomplete":    `{"subscriptionId":"` + subID + `"}`,
	} {
		if _, err := killbill.ParseUsagePayload([]byte(raw)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestEndpointFingerprint(t *testing.T) {
	mk := func(url string) *killbill.Client {
		kb, err := killbill.New(killbill.Config{URL: url, AdminUser: "admin", AdminPassword: "pw"})
		if err != nil {
			t.Fatal(err)
		}
		return kb
	}
	a := mk("http://killbill.internal:8080").EndpointFingerprint()
	if len(a) != 64 {
		t.Fatalf("fingerprint = %q", a)
	}
	if _, err := hex.DecodeString(a); err != nil || a != strings.ToLower(a) {
		t.Fatalf("fingerprint is not lower hex: %q", a)
	}
	for _, same := range []string{"http://killbill.internal:8080/", "HTTP://KillBill.Internal:8080", "http://admin:pw@killbill.internal:8080"} {
		if got := mk(same).EndpointFingerprint(); got != a {
			t.Errorf("%s: fingerprint changed", same)
		}
	}
	for _, other := range []string{"http://killbill.internal:8081", "https://killbill.internal:8080", "http://killbill-2.internal:8080", "http://killbill.internal:8080/kb"} {
		if got := mk(other).EndpointFingerprint(); got == a {
			t.Errorf("%s: same fingerprint", other)
		}
	}
	if mk("https://kb.internal").EndpointFingerprint() != mk("https://kb.internal:443").EndpointFingerprint() {
		t.Error("default port changes the fingerprint")
	}
	sum := sha256.Sum256([]byte("pw"))
	if strings.Contains(a, hex.EncodeToString(sum[:4])) {
		t.Error("fingerprint carries a secret")
	}
}

// The enginetest fake serves no admin tenant lookup, so the target is read
// from a server that answers the two requests it makes.
func TestUsageTarget(t *testing.T) {
	const tenantID, accountID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "99999999-8888-7777-6666-555555555555"
	kb := serverWith(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		switch {
		case r.URL.Path == "/1.0/kb/tenants" && ok && user == "admin" && pass == "pw" && r.URL.Query().Get("apiKey") == "zs-test":
			answer(200, `{"tenantId":"`+tenantID+`","apiKey":"zs-test"}`)(w, r)
		case r.URL.Path == "/1.0/kb/subscriptions/"+subID && r.Header.Get(killbill.HeaderAPIKey) == "zs-test":
			answer(200, `{"subscriptionId":"`+subID+`","accountId":"`+accountID+`"}`)(w, r)
		default:
			answer(404, `{"code":9999}`)(w, r)
		}
	})
	ctx := context.Background()
	got, err := kb.UsageTarget(ctx, creds, subID)
	if err != nil {
		t.Fatal(err)
	}
	if got != (killbill.UsageTarget{EndpointFingerprint: kb.EndpointFingerprint(), TenantID: tenantID, AccountID: accountID}) {
		t.Fatalf("target = %+v", got)
	}
	if _, err := kb.UsageTarget(ctx, creds, "00000000-0000-0000-0000-000000000001"); !errors.Is(err, killbill.ErrNotFound) {
		t.Fatalf("unknown subscription: %v", err)
	}
	other := killbill.TenantCredentials{APIKey: "zs-other", APISecret: "secret"}
	if _, err := kb.UsageTarget(ctx, other, subID); !errors.Is(err, killbill.ErrNotFound) {
		t.Fatalf("subscription of another tenant: %v", err)
	}
	empty := serverWith(t, answer(200, `{}`))
	if _, err := empty.UsageTarget(ctx, creds, subID); err == nil {
		t.Fatal("empty answers accepted")
	}
}

func TestSendUsageClassifiesAnswers(t *testing.T) {
	payload := usagePayload(t, subID)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   killbill.UsageSendOutcome
	}{
		{"created", 201, ``, killbill.UsageSent},
		{"no content", 204, ``, killbill.UsageSent},
		{"tracking id held", 400, `{"code":50000,"message":"duplicate"}`, killbill.UsageHeld},
		{"validation", 400, `{"code":2000,"message":"bad"}`, killbill.UsageNotSent},
		{"validation without code", 400, ``, killbill.UsageNotSent},
		{"unauthorized", 401, ``, killbill.UsageNotSent},
		{"forbidden", 403, ``, killbill.UsageNotSent},
		{"unknown subscription", 404, `{"code":9999}`, killbill.UsageNotSent},
		{"conflict", 409, ``, killbill.UsageNotSent},
		{"unprocessable", 422, ``, killbill.UsageNotSent},
		{"request timeout", 408, ``, killbill.UsageSendUnknown},
		{"too many requests", 429, ``, killbill.UsageSendUnknown},
		{"server error", 500, `{"code":1}`, killbill.UsageSendUnknown},
		{"bad gateway", 502, ``, killbill.UsageSendUnknown},
		{"unavailable", 503, ``, killbill.UsageSendUnknown},
		{"gateway timeout", 504, ``, killbill.UsageSendUnknown},
		{"redirect", 302, ``, killbill.UsageSendUnknown},
	} {
		kb := serverWith(t, answer(tc.status, tc.body))
		res, err := kb.SendUsage(context.Background(), creds, cc, payload)
		if res.Outcome != tc.want || res.Status != tc.status {
			t.Errorf("%s: %+v %v", tc.name, res, err)
		}
		if (tc.want == killbill.UsageSent || tc.want == killbill.UsageHeld) != (err == nil) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: error leaks a secret: %v", tc.name, err)
		}
	}
}

// The zero value is the unsafe-to-retry answer.
func TestUsageSendOutcomeZeroValueIsUnknown(t *testing.T) {
	var res killbill.UsageSendResult
	if res.Outcome != killbill.UsageSendUnknown {
		t.Fatalf("zero outcome = %v", res.Outcome)
	}
}

func TestSendUsageTransportFailures(t *testing.T) {
	payload := usagePayload(t, subID)

	t.Run("refused connection was never sent", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		kb, _ := killbill.New(killbill.Config{URL: url, AdminUser: "admin", AdminPassword: "pw"})
		res, err := kb.SendUsage(context.Background(), creds, cc, payload)
		if err == nil || res.Outcome != killbill.UsageNotSent || res.Status != 0 {
			t.Fatalf("refused = %+v %v", res, err)
		}
	})

	t.Run("unknown host was never sent", func(t *testing.T) {
		kb, _ := killbill.New(killbill.Config{URL: "http://kb.invalid:8080", AdminUser: "admin", AdminPassword: "pw"})
		res, err := kb.SendUsage(context.Background(), creds, cc, payload)
		if err == nil || res.Outcome != killbill.UsageNotSent {
			t.Fatalf("unknown host = %+v %v", res, err)
		}
	})

	t.Run("untrusted certificate was never sent", func(t *testing.T) {
		hit := false
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
		defer srv.Close()
		kb, _ := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
		res, err := kb.SendUsage(context.Background(), creds, cc, payload)
		if err == nil || res.Outcome != killbill.UsageNotSent || hit {
			t.Fatalf("untrusted = %+v %v hit=%v", res, err, hit)
		}
	})

	t.Run("cancelled before sending", func(t *testing.T) {
		hit := false
		kb := serverWith(t, func(w http.ResponseWriter, r *http.Request) { hit = true })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, err := kb.SendUsage(ctx, creds, cc, payload)
		if err == nil || res.Outcome != killbill.UsageNotSent || hit {
			t.Fatalf("cancelled = %+v %v hit=%v", res, err, hit)
		}
	})

	t.Run("connection reset after the body is unknown", func(t *testing.T) {
		kb := serverWith(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
		res, err := kb.SendUsage(context.Background(), creds, cc, payload)
		if err == nil || res.Outcome != killbill.UsageSendUnknown {
			t.Fatalf("reset = %+v %v", res, err)
		}
	})

	t.Run("no answer before the timeout is unknown", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			<-release
		}))
		defer srv.Close()
		defer close(release)
		kb, _ := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw",
			HTTPClient: &http.Client{Timeout: 150 * time.Millisecond}})
		res, err := kb.SendUsage(context.Background(), creds, cc, payload)
		if err == nil || res.Outcome != killbill.UsageSendUnknown {
			t.Fatalf("timeout = %+v %v", res, err)
		}
	})

	t.Run("cancelled while waiting for the answer is unknown", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			close(started)
			<-release
		}))
		defer srv.Close()
		defer close(release)
		kb, _ := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-started; cancel() }()
		res, err := kb.SendUsage(ctx, creds, cc, payload)
		if err == nil || res.Outcome != killbill.UsageSendUnknown {
			t.Fatalf("cancel = %+v %v", res, err)
		}
	})
}

func TestSendUsageRefusesWhatItCannotSend(t *testing.T) {
	hit := false
	kb := serverWith(t, func(w http.ResponseWriter, r *http.Request) { hit = true })
	for name, tc := range map[string]struct {
		payload []byte
		cc      killbill.CallContext
	}{
		"not json":       {[]byte(`nope`), cc},
		"incomplete":     {[]byte(`{"subscriptionId":"` + subID + `"}`), cc},
		"no audit actor": {usagePayload(t, subID), killbill.CallContext{}},
	} {
		res, err := kb.SendUsage(context.Background(), creds, tc.cc, tc.payload)
		if err == nil || res.Outcome != killbill.UsageNotSent || hit {
			t.Errorf("%s: %+v %v hit=%v", name, res, err, hit)
		}
	}
}

// Against the Kill Bill fake: the first send is recorded, the second meets
// the tracking ID and records nothing.
func TestSendUsageAgainstTheFake(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, acct := fakeTenant(t)
	sub, err := kb.CreateSubscription(ctx, creds, cc, acct, enginetest.PlanMetered, "zs-sub-send")
	if err != nil {
		t.Fatal(err)
	}
	payload := usagePayload(t, sub)
	if res, err := kb.SendUsage(ctx, creds, cc, payload); err != nil || res.Outcome != killbill.UsageSent {
		t.Fatalf("first = %+v %v", res, err)
	}
	if res, err := kb.SendUsage(ctx, creds, cc, payload); err != nil || res.Outcome != killbill.UsageHeld {
		t.Fatalf("second = %+v %v", res, err)
	}
	if n := len(f.UsageRecords("zs-test")); n != 1 {
		t.Fatalf("records = %d", n)
	}
	missing := usagePayload(t, "00000000-0000-0000-0000-000000000001")
	if res, err := kb.SendUsage(ctx, creds, cc, missing); !errors.Is(err, killbill.ErrNotFound) || res.Outcome != killbill.UsageNotSent {
		t.Fatalf("unknown subscription = %+v %v", res, err)
	}
}
