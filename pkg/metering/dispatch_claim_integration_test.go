//go:build integration

package metering_test

import (
	"context"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
)

// A claim is the durable record that a send may follow: it is taken once
// from the unsent state, never taken over, renewed only while live, and
// never reaches a legacy protocol 0 rollup.
func TestClaimTakesUnsentProtocolOneRollupOnceAndRenewsIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-claim")
	tn := e.tenant(b, "Claim Co")
	sc := tn.Scope()
	e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "c-a", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	// A legacy protocol 0 rollup that is due must stay untouched.
	legacy := idgen.MustNewV7()
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollups (id, brand_id, tenant_id, killbill_unit, day, period_start, record_at,
    quantity_micro, events, tracking_id, next_attempt_at) VALUES ($1, $2, $3, 'api-call', current_date, now(), now(), 1, 1, 'legacy-claim', now() - interval '1 hour')`,
		legacy, b.ID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.RollUp(ctx, sc); err != nil || n != 1 {
		t.Fatalf("rollup = %d %v", n, err)
	}
	due, err := e.svc.ClaimDue(ctx, &sc)
	if err != nil || len(due) != 1 || due[0].ID() == legacy {
		t.Fatalf("claimed = %d %v", len(due), err)
	}
	var provenance string
	var fromNever, token, started bool
	read := func(id idgen.UUID) {
		if err := e.db.Pool.QueryRow(ctx, `SELECT send_provenance, claim_from_never, claim_token IS NOT NULL AND claimed_until IS NOT NULL,
send_started_at IS NOT NULL FROM usage_rollups WHERE id = $1`, id).Scan(&provenance, &fromNever, &token, &started); err != nil {
			t.Fatal(err)
		}
	}
	read(due[0].ID())
	if provenance != "possibly_attempted" || !fromNever || !token || started {
		t.Fatalf("claimed rollup = %q fromNever=%v token=%v started=%v", provenance, fromNever, token, started)
	}
	read(legacy)
	if provenance != "unknown" || fromNever || token {
		t.Fatalf("legacy rollup changed = %q fromNever=%v token=%v", provenance, fromNever, token)
	}
	if again, err := e.svc.ClaimDue(ctx, &sc); err != nil || len(again) != 0 {
		t.Fatalf("a claimed rollup was claimed again = %d %v", len(again), err)
	}
	var before, after time.Time
	if err := e.db.Pool.QueryRow(ctx, `SELECT claimed_until FROM usage_rollups WHERE id = $1`, due[0].ID()).Scan(&before); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(30 * time.Second)
	if held, err := e.svc.RenewClaim(ctx, due[0]); err != nil || !held {
		t.Fatalf("renew = %v %v", held, err)
	}
	if err := e.db.Pool.QueryRow(ctx, `SELECT claimed_until FROM usage_rollups WHERE id = $1`, due[0].ID()).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.After(before) {
		t.Fatalf("claimed_until did not move forward: %v -> %v", before, after)
	}
}

// The target is bound once, before any send, from what Kill Bill says the
// tenant's subscription belongs to.
func TestBindTargetStoresEndpointTenantAndAccountBeforeAnySend(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-bind")
	tn := e.tenant(b, "Bind Co")
	sc := tn.Scope()
	sub := e.subscribe(b, tn, enginetest.PlanMetered)
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "b-a", QuantityMicro: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.RollUp(ctx, sc); err != nil || n != 1 {
		t.Fatalf("rollup = %d %v", n, err)
	}
	due, err := e.svc.ClaimDue(ctx, &sc)
	if err != nil || len(due) != 1 {
		t.Fatalf("claimed = %d %v", len(due), err)
	}
	if bound, err := e.svc.BindTarget(ctx, due[0], sub); err != nil || !bound {
		t.Fatalf("bind = %v %v", bound, err)
	}
	if bound, err := e.svc.BindTarget(ctx, due[0], sub); err != nil || !bound {
		t.Fatalf("a second identical bind = %v %v", bound, err)
	}
	var endpoint, account string
	var subscription *string
	var started bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT target_endpoint_fingerprint, target_killbill_account_id::text, subscription_id::text,
send_started_at IS NOT NULL FROM usage_rollups WHERE id = $1`, due[0].ID()).Scan(&endpoint, &account, &subscription, &started); err != nil {
		t.Fatal(err)
	}
	if want := e.svc.Billing.KillBill.EndpointFingerprint(); endpoint != want {
		t.Fatalf("endpoint = %q, want %q", endpoint, want)
	}
	if account != tn.KillBillAccountID || subscription == nil || *subscription != sub || started {
		t.Fatalf("account = %q subscription = %v started = %v", account, subscription, started)
	}
}
