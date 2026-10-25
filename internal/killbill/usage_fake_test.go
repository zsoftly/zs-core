package killbill_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
)

// meteredSubscription is a subscription that prices the API call unit and the
// payload for one record of 2.5 units on it.
func meteredSubscription(t *testing.T) (*enginetest.Fake, *killbill.Client, killbill.TenantCredentials, string, []byte) {
	t.Helper()
	f, kb, creds, acct := fakeTenant(t)
	sub, err := kb.CreateSubscription(context.Background(), creds, cc, acct, enginetest.PlanMetered, "zs-sub-fake")
	if err != nil {
		t.Fatal(err)
	}
	return f, kb, creds, sub, usagePayload(t, sub)
}

// The fake keeps the exact bytes of every usage POST it receives, so a test
// can compare what went on the wire with what a caller stored.
func TestFakeCapturesRawUsageBodies(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, sub, payload := meteredSubscription(t)
	if got := f.UsageBodies("zs-test"); len(got) != 0 {
		t.Fatalf("bodies before any POST = %q", got)
	}
	if res, err := kb.SendUsage(ctx, creds, cc, payload); err != nil || res.Outcome != killbill.UsageSent {
		t.Fatalf("send = %+v %v", res, err)
	}
	// A POST the fake refuses is captured too: it reached the fake.
	f.FailWrites("/1.0/kb/usages", 1, http.StatusServiceUnavailable)
	if res, _ := kb.SendUsage(ctx, creds, cc, payload); res.Outcome != killbill.UsageSendUnknown {
		t.Fatalf("refused send = %+v", res)
	}
	other, err := killbill.ParseUsagePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	other.TrackingID = "zs-usg-second"
	second, err := other.Payload()
	if err != nil {
		t.Fatal(err)
	}
	if err := kb.RecordUsage(ctx, creds, cc, other); err != nil {
		t.Fatal(err)
	}
	got := f.UsageBodies("zs-test")
	if len(got) != 3 || !bytes.Equal(got[0], payload) || !bytes.Equal(got[1], payload) || !bytes.Equal(got[2], second) {
		t.Fatalf("bodies = %q\nwant 3 in order: %s %s %s", got, payload, payload, second)
	}
	if n := len(f.UsageRecords("zs-test")); n != 2 {
		t.Fatalf("records = %d, want 2 (the refused POST recorded nothing)", n)
	}
	// Bodies are per Kill Bill tenant, and the caller gets copies.
	f.AddKillBillTenant("zs-other", "secret")
	if n := len(f.UsageBodies("zs-other")); n != 0 {
		t.Fatalf("another tenant sees %d bodies", n)
	}
	got[0][0] = 'X'
	if again := f.UsageBodies("zs-test"); !bytes.Equal(again[0], payload) {
		t.Fatal("the returned bodies alias the fake's own")
	}
	// Reads and other writes are not usage POSTs.
	if _, err := kb.RolledUpUsage(ctx, creds, sub, "2026-10-01", "2026-11-01"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.UsageBodies("zs-test")); n != 3 {
		t.Fatalf("bodies after a read = %d", n)
	}
}

// LoseAnswers: Kill Bill records the usage and the answer is a 503. The
// client cannot know, and the tracking ID proves it afterwards.
func TestFakeLostAnswerRecordsThenAnswers503(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, _, payload := meteredSubscription(t)
	f.LoseAnswers("/1.0/kb/usages", 1)
	res, err := kb.SendUsage(ctx, creds, cc, payload)
	if err == nil || res.Outcome != killbill.UsageSendUnknown || res.Status != http.StatusServiceUnavailable {
		t.Fatalf("lost answer = %+v %v", res, err)
	}
	if n := len(f.UsageRecords("zs-test")); n != 1 {
		t.Fatalf("records after a lost answer = %d, want 1", n)
	}
	if res, err := kb.SendUsage(ctx, creds, cc, payload); err != nil || res.Outcome != killbill.UsageHeld {
		t.Fatalf("send after a lost answer = %+v %v", res, err)
	}
	if n := len(f.UsageRecords("zs-test")); n != 1 {
		t.Fatalf("records after the held send = %d, want 1", n)
	}
	if got := f.UsageBodies("zs-test"); len(got) != 2 || !bytes.Equal(got[0], got[1]) || !bytes.Equal(got[0], payload) {
		t.Fatalf("bodies = %q", got)
	}
}

// DropAnswers: Kill Bill records the usage and the connection is closed
// without an answer, as when it resets after the write.
func TestFakeDroppedAnswerRecordsThenClosesTheConnection(t *testing.T) {
	ctx := context.Background()
	f, kb, creds, _, payload := meteredSubscription(t)
	f.DropAnswers("/1.0/kb/usages", 2)
	for i := 0; i < 2; i++ {
		res, err := kb.SendUsage(ctx, creds, cc, payload)
		if err == nil || res.Outcome != killbill.UsageSendUnknown || res.Status != 0 {
			t.Fatalf("dropped answer %d = %+v %v", i, res, err)
		}
		if n := len(f.UsageRecords("zs-test")); n != 1 {
			t.Fatalf("records after dropped answer %d = %d, want 1", i, n)
		}
	}
	// Only the next two were dropped: the third is answered, as held.
	if res, err := kb.SendUsage(ctx, creds, cc, payload); err != nil || res.Outcome != killbill.UsageHeld {
		t.Fatalf("third send = %+v %v", res, err)
	}
	if got := f.UsageBodies("zs-test"); len(got) != 3 {
		t.Fatalf("bodies = %d, want 3", len(got))
	}
	// Other writes are untouched by a drop on the usage path.
	f.DropAnswers("/1.0/kb/usages", 1)
	if _, err := kb.CreateAccount(ctx, creds, cc, killbill.Account{ExternalKey: "tnt_drop", Name: "D", Currency: "CAD"}); err != nil {
		t.Fatalf("account create under a usage drop: %v", err)
	}
}
