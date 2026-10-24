//go:build integration

package metering_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/metering"
)

// Kill Bill answering 4xx after the send started proves nothing was
// recorded, but the guard has no way back from a started send, so the
// rollup is kept as unconfirmed and never sent again.
func TestNotSentAfterStartIsTransportUnconfirmedAndNeverResent(t *testing.T) {
	e, slug, tenantID := setupOneRollup(t, "zs-transport")
	ctx := context.Background()
	e.fake.FailWrites("/1.0/kb/usages", 1, 400)
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Unconfirmed != 1 || report.Forwarded != 0 || report.Failed != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var reason string
	if err := e.db.Pool.QueryRow(ctx, `SELECT closure_unresolved_reason FROM usage_rollups WHERE tenant_id = $1`, tenantID).Scan(&reason); err != nil || reason != "usage_transport_unconfirmed" {
		t.Fatalf("reason = %q %v", reason, err)
	}
	posts := usagePosts(e)
	if _, err := e.svc.Forward(ctx); err != nil || usagePosts(e) != posts || len(e.fake.UsageRecords(slug)) != 0 {
		t.Fatalf("a second POST was made: %d -> %d %v", posts, usagePosts(e), err)
	}
}

// The bytes Kill Bill received are exactly the body the rollup stored before
// the send. jsonb reads back with its own key order, so the stored value is
// parsed and written out again, which is how the body was built.
func TestStoredPayloadIsExactlyTheBodyKillBillReceived(t *testing.T) {
	e, slug, tenantID := setupOneRollup(t, "zs-wire")
	ctx := context.Background()
	if report, err := e.svc.Forward(ctx); err != nil || report.Forwarded != 1 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var payload []byte
	if err := e.db.Pool.QueryRow(ctx, `SELECT send_payload FROM usage_rollups WHERE tenant_id = $1`, tenantID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	stored, err := killbill.ParseUsagePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	want, err := stored.Payload()
	if err != nil {
		t.Fatal(err)
	}
	bodies := e.fake.UsageBodies(slug)
	if len(bodies) != 1 || !bytes.Equal(bodies[0], want) {
		t.Fatalf("Kill Bill received %q, the stored send is %q", bodies, want)
	}
}

// ForwardTenant claims and sends only that tenant's usage.
func TestForwardTenantTouchesOnlyThatTenant(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-only")
	a, c := e.tenant(b, "A Co"), e.tenant(b, "C Co")
	e.subscribe(b, a, enginetest.PlanMetered)
	e.subscribe(b, c, enginetest.PlanMetered)
	for _, tn := range []string{"a", "c"} {
		sc := a.Scope()
		if tn == "c" {
			sc = c.Scope()
		}
		if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "o-" + tn, QuantityMicro: 1_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	sc := a.Scope()
	if report, err := e.svc.ForwardTenant(ctx, sc); err != nil || report.Forwarded != 1 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollups WHERE tenant_id = $1`, c.ID); n != 0 {
		t.Fatalf("another tenant's rollups = %d", n)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND (rollup_id IS NOT NULL OR forwarded_at IS NOT NULL)`, c.ID); n != 0 {
		t.Fatal("another tenant's events were touched")
	}
	if usagePosts(e) != 1 {
		t.Fatalf("posts = %d", usagePosts(e))
	}
}

// A forwarder stopped after its first send gives back the rollups it
// claimed and never started, so the next pass sends them.
func TestCancelledForwarderGivesBackRollupsItNeverStarted(t *testing.T) {
	e := newEnv(t)
	b := e.brand("zs-cancel")
	for _, name := range []string{"P Co", "Q Co", "R Co"} {
		tn := e.tenant(b, name)
		e.subscribe(b, tn, enginetest.PlanMetered)
		if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "x-1", QuantityMicro: 1_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	cctx, cancel := context.WithCancel(context.Background())
	e.fake.AfterWrite(func(method, path string) { cancel() })
	_, _ = e.svc.Forward(cctx)
	e.fake.AfterWrite(nil)
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollups WHERE status = 'pending' AND (claim_token IS NOT NULL OR claim_from_never OR send_provenance <> 'never_attempted')`); n != 0 {
		t.Fatalf("rollups left claimed by the stopped forwarder = %d", n)
	}
	if report, err := e.svc.Forward(context.Background()); err != nil || report.Failed != 0 || len(e.fake.UsageRecords(b.Slug)) != 3 {
		t.Fatalf("forward after the restart = %+v %v, records %d", report, err, len(e.fake.UsageRecords(b.Slug)))
	}
}

// A rollup already bound to one Kill Bill target is never rebound to another.
func TestBindTargetToAnotherTargetIsAMismatchNotALostClaim(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-mismatch")
	tn := e.tenant(b, "Mismatch Co")
	sc := tn.Scope()
	sub := e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "m-a", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.RollUp(ctx, sc); err != nil || n != 1 {
		t.Fatalf("rollup = %d %v", n, err)
	}
	due, err := e.svc.ClaimDue(ctx, &sc)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim = %d %v", len(due), err)
	}
	acct, err := e.svc.Billing.TenantAccount(ctx, e.db.Pool, sc)
	if err != nil {
		t.Fatal(err)
	}
	target, err := acct.UsageTarget(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	if bound, mismatch, err := e.svc.BindTargetTo(ctx, due[0], sub, target); err != nil || !bound || mismatch {
		t.Fatalf("first bind = %v %v %v", bound, mismatch, err)
	}
	other := target
	other.EndpointFingerprint = otherFingerprint(target.EndpointFingerprint)
	if bound, mismatch, err := e.svc.BindTargetTo(ctx, due[0], sub, other); err != nil || bound || !mismatch {
		t.Fatalf("bind to another target = %v %v %v, want a mismatch", bound, mismatch, err)
	}
}

// A rollup bound to a Kill Bill target that is no longer the configured one
// is held, not sent and not skipped silently: staff see an audit row.
func TestTargetMismatchWaitsAndLeavesAnAuditRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-moved")
	tn := e.tenant(b, "Moved Co")
	sc := tn.Scope()
	sub := e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "mv-a", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.RollUp(ctx, sc); err != nil || n != 1 {
		t.Fatalf("rollup = %d %v", n, err)
	}
	due, err := e.svc.ClaimDue(ctx, &sc)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim = %d %v", len(due), err)
	}
	acct, err := e.svc.Billing.TenantAccount(ctx, e.db.Pool, sc)
	if err != nil {
		t.Fatal(err)
	}
	target, err := acct.UsageTarget(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	// The rollup was bound while Kill Bill lived at another address.
	moved := target
	moved.EndpointFingerprint = otherFingerprint(target.EndpointFingerprint)
	if bound, _, err := e.svc.BindTargetTo(ctx, due[0], sub, moved); err != nil || !bound {
		t.Fatalf("bind = %v %v", bound, err)
	}
	if err := e.svc.ReleaseClaim(ctx, due[0]); err != nil {
		t.Fatal(err)
	}
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Failed != 1 || report.Forwarded != 0 || report.Skipped != 0 || usagePosts(e) != 0 {
		t.Fatalf("forward = %+v %v posts=%d", report, err, usagePosts(e))
	}
	var lastErr string
	if err := e.db.Pool.QueryRow(ctx, `SELECT last_error FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&lastErr); err != nil || lastErr != "usage_target_unconfirmed" {
		t.Fatalf("last_error = %q %v", lastErr, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'metering.usage.target_mismatch'`, tn.ID); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
}

// otherFingerprint is a valid fingerprint that differs from fp in its first
// digit, so it can never equal fp whatever port the fake listens on.
func otherFingerprint(fp string) string {
	if fp[0] == '0' {
		return "1" + fp[1:]
	}
	return "0" + fp[1:]
}

// A forwarder stopped while its POST is in flight still sees the answer, and
// the rollup is forwarded, not left unconfirmed.
func TestStopDuringThePostStillForwardsTheRollup(t *testing.T) {
	e, slug, tenantID := setupOneRollup(t, "zs-midpost")
	ctx := context.Background()
	e.fake.SetWriteDelay("/1.0/kb/usages", 1500*time.Millisecond)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for cctx.Err() == nil {
			var started bool
			if err := e.db.Pool.QueryRow(ctx, `SELECT bool_or(send_started_at IS NOT NULL) FROM usage_rollups WHERE tenant_id = $1`, tenantID).Scan(&started); err == nil && started {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	report, _ := e.svc.Forward(cctx)
	if report.Forwarded != 1 || report.Unconfirmed != 0 {
		t.Fatalf("forward = %+v", report)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollups WHERE tenant_id = $1 AND status = 'forwarded' AND closure_unresolved_reason IS NULL`, tenantID); n != 1 {
		t.Fatalf("forwarded rollups = %d, want 1", n)
	}
	if recs := e.fake.UsageRecords(slug); len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
}
