//go:build integration

package metering_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/pkg/metering"
)

func usagePosts(e *env) int {
	n := 0
	for _, w := range e.fake.Writes() {
		if strings.HasPrefix(w.Path, "/1.0/kb/usages") {
			n++
		}
	}
	return n
}

// The send is written down before it happens: the stored body is the body
// Kill Bill got, and the rollup and its events are marked forwarded only
// from that stored send.
func TestSendStoresStartAndBodyThenForwardsRollupAndEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-send")
	tn := e.tenant(b, "Send Co")
	sc := tn.Scope()
	sub := e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "s-a", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Forwarded != 1 || report.Failed != 0 || report.Unconfirmed != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var (
		status, provenance, trackingID string
		fromNever, claimed             bool
		started                        bool
		payload                        []byte
	)
	if err := e.db.Pool.QueryRow(ctx, `SELECT status, send_provenance, tracking_id, claim_from_never, claim_token IS NOT NULL OR claimed_until IS NOT NULL,
send_started_at IS NOT NULL, send_payload FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&status, &provenance, &trackingID, &fromNever, &claimed, &started, &payload); err != nil {
		t.Fatal(err)
	}
	if status != "forwarded" || provenance != "possibly_attempted" || fromNever || claimed || !started {
		t.Fatalf("rollup = %q %q fromNever=%v claimed=%v started=%v", status, provenance, fromNever, claimed, started)
	}
	var body struct {
		SubscriptionID string `json:"subscriptionId"`
		TrackingID     string `json:"trackingId"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.TrackingID != trackingID || body.SubscriptionID != sub {
		t.Fatalf("stored body = %s %v", payload, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND forwarded_at IS NOT NULL AND killbill_tracking_id = $2`, tn.ID, trackingID); n != 1 {
		t.Fatalf("forwarded events = %d, want 1", n)
	}
	if recs := e.fake.UsageRecords(b.Slug); len(recs) != 1 || usagePosts(e) != 1 {
		t.Fatalf("records = %d posts = %d, want 1 each", len(recs), usagePosts(e))
	}
}

// An answer that does not say whether Kill Bill recorded the usage is never
// sent again: the rollup keeps its stored send and waits for staff.
func TestUnknownSendOutcomeIsStickyAndNeverRetried(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-unknown")
	tn := e.tenant(b, "Unknown Co")
	sc := tn.Scope()
	e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "u-a", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	e.fake.FailWrites("/1.0/kb/usages", 1, 500)
	report, err := e.svc.Forward(ctx)
	if err != nil || report.Unconfirmed != 1 || report.Forwarded != 0 || report.Failed != 0 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	var status, reason, lastErr string
	var started, payload bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT status, closure_unresolved_reason, last_error, send_started_at IS NOT NULL, send_payload IS NOT NULL
FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&status, &reason, &lastErr, &started, &payload); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || reason != "usage_send_unconfirmed" || lastErr != reason || !started || !payload {
		t.Fatalf("rollup = %q %q %q started=%v payload=%v", status, reason, lastErr, started, payload)
	}
	posts := usagePosts(e)
	e.clock.advance(24 * time.Hour)
	again, err := e.svc.Forward(ctx)
	if err != nil || again.Forwarded != 0 || again.Unconfirmed != 0 || usagePosts(e) != posts {
		t.Fatalf("second forward = %+v %v, posts %d -> %d", again, err, posts, usagePosts(e))
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND forwarded_at IS NOT NULL`, tn.ID); n != 0 {
		t.Fatal("events of an unconfirmed send were marked forwarded")
	}
}
