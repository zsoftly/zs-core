//go:build integration

package metering_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
)

// Raw usage is accepted as dispatch protocol 1 and rolled up into an unsent
// protocol 1 rollup that names exactly the events it holds. Migration 22's
// guard only lets such events be linked to such a rollup.
func TestRollUpCreatesUnsentProtocolOneRollupAndLinksProtocolOneEvents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-proto1")
	tn := e.tenant(b, "Proto One Co")
	sc := tn.Scope()
	if _, _, err := e.record(sc,
		metering.Event{Meter: "api-calls", EventID: "p1-a", QuantityMicro: 1_000_000},
		metering.Event{Meter: "api-calls", EventID: "p1-b", QuantityMicro: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE tenant_id = $1 AND acceptance_protocol = 1`, tn.ID); n != 2 {
		t.Fatalf("protocol 1 events = %d, want 2", n)
	}
	rolled, err := e.svc.RollUp(ctx, sc)
	if err != nil || rolled != 1 {
		t.Fatalf("rollup = %d %v", rolled, err)
	}
	var (
		rollupID    idgen.UUID
		protocol    int
		provenance  string
		status      string
		fingerprint string
		unit        string
		subscr      *string
		day         string
		started     bool
		payload     bool
		claimed     bool
		fromNever   bool
	)
	if err := e.db.Pool.QueryRow(ctx, `SELECT id, send_protocol, send_provenance, status, event_set_fingerprint, killbill_unit,
requested_subscription_id::text, day::text, send_started_at IS NOT NULL, send_payload IS NOT NULL,
claim_token IS NOT NULL OR claimed_until IS NOT NULL, claim_from_never
FROM usage_rollups WHERE tenant_id = $1`, tn.ID).Scan(&rollupID, &protocol, &provenance, &status, &fingerprint, &unit, &subscr, &day,
		&started, &payload, &claimed, &fromNever); err != nil {
		t.Fatal(err)
	}
	if protocol != 1 || provenance != "never_attempted" || status != "pending" || started || payload || claimed || fromNever {
		t.Fatalf("rollup = protocol %d %q %q started=%v payload=%v claimed=%v fromNever=%v", protocol, provenance, status, started, payload, claimed, fromNever)
	}
	rows, err := e.db.Pool.Query(ctx, `SELECT id::text FROM usage_events WHERE tenant_id = $1 AND rollup_id = $2`, tn.ID, rollupID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatalf("linked events = %d, want 2", len(ids))
	}
	// The fingerprint is durable evidence: sha256 of a fixed domain, the
	// scope, the rollup key and the sorted event ids, each followed by NUL.
	slices.Sort(ids)
	h := sha256.New()
	sub := ""
	if subscr != nil {
		sub = *subscr
	}
	for _, part := range append([]string{"zs-core.usage-event-set.v1", sc.Brand.String(), sc.Tenant.String(), unit, sub, day}, ids...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	if want := hex.EncodeToString(h.Sum(nil)); fingerprint != want {
		t.Fatalf("event_set_fingerprint = %q, want %q", fingerprint, want)
	}
}

// A raw event accepted before protocol 1 is read-only under the guard. The
// forwarder must neither roll it up nor let it poison a batch with protocol
// 1 events, and the unbilled legacy usage must stay countable.
func TestRollUpLeavesLegacyProtocolZeroEventsUntouchedAndCounted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-legacy0")
	tn := e.tenant(b, "Legacy Zero Co")
	sc := tn.Scope()
	var meterID idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM meters WHERE brand_id = $1 AND slug = 'api-calls'`, b.ID).Scan(&meterID); err != nil {
		t.Fatal(err)
	}
	legacyID := idgen.MustNewV7()
	at := e.clock.Now().Add(-time.Minute)
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_events (id, brand_id, tenant_id, meter_id, event_id, killbill_unit, quantity_micro,
    dimensions, dimensions_key, occurred_at, received_at) VALUES ($1, $2, $3, $4, 'legacy-a', 'api-call', 7, '{}'::jsonb, '{}', $5, $5)`,
		legacyID, b.ID, tn.ID, meterID, at); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.LegacyUnrolled(ctx, sc); err != nil || n != 1 {
		t.Fatalf("legacy unrolled = %d %v, want 1", n, err)
	}
	if _, _, err := e.record(sc, metering.Event{Meter: "api-calls", EventID: "new-a", QuantityMicro: 1_000_000, OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
	rolled, err := e.svc.RollUp(ctx, sc)
	if err != nil || rolled != 1 {
		t.Fatalf("rollup beside a legacy event = %d %v", rolled, err)
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_events WHERE id = $1 AND rollup_id IS NULL AND acceptance_protocol = 0 AND quantity_micro = 7`, legacyID); n != 1 {
		t.Fatal("the legacy event was changed or rolled up")
	}
	if n := count(t, e.db, `SELECT count(*) FROM usage_rollups WHERE tenant_id = $1 AND events = 1 AND quantity_micro = 1000000`, tn.ID); n != 1 {
		t.Fatalf("rollups holding only the new event = %d, want 1", n)
	}
	if n, err := e.svc.LegacyUnrolled(ctx, sc); err != nil || n != 1 {
		t.Fatalf("legacy unrolled after rollup = %d %v, want 1", n, err)
	}
}
