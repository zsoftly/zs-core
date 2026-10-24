//go:build integration

package tenant_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

var reviewActor = events.Actor{Type: events.ActorStaff, ID: "sub:support"}

// closingWithLegacyUsage is a closing tenant waiting for finance that holds
// usage accepted before dispatch protocol 1: one event that was never rolled
// up and one rollup whose send history is unknown. The rows are returned by
// ID so a test can prove they were never touched.
func closingWithLegacyUsage(t *testing.T, e *env, events, rollups bool) (tenant.Tenant, idgen.UUID, idgen.UUID) {
	t.Helper()
	ctx := context.Background()
	p := e.person("owner@example.test")
	tn := e.mustOrg(p, "Tenant A")
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at, status)
VALUES ($1, $2, now(), 'finance_pending')`, tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	var eventID, rollupID idgen.UUID
	if events {
		meterID := idgen.MustNewV7()
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO meters (id, brand_id, slug, unit, killbill_unit) VALUES ($1, $2, 'legacy-meter', 'count', 'api-call')`, meterID, tn.BrandID); err != nil {
			t.Fatal(err)
		}
		eventID = idgen.MustNewV7()
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_events (id, brand_id, tenant_id, meter_id, event_id, killbill_unit, quantity_micro,
    dimensions, dimensions_key, occurred_at, received_at) VALUES ($1, $2, $3, $4, 'legacy-a', 'api-call', 7, '{}'::jsonb, '{}', now(), now())`,
			eventID, tn.BrandID, tn.ID, meterID); err != nil {
			t.Fatal(err)
		}
	}
	if rollups {
		rollupID = idgen.MustNewV7()
		if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollups (id, brand_id, tenant_id, killbill_unit, day, period_start, record_at,
    quantity_micro, events, tracking_id) VALUES ($1, $2, $3, 'api-call', current_date, now(), now(), 5, 1, 'legacy-closure-rollup')`,
			rollupID, tn.BrandID, tn.ID); err != nil {
			t.Fatal(err)
		}
	}
	return tn, eventID, rollupID
}

func complete(e *env, tn tenant.Tenant) error {
	_, err := e.db.Pool.Exec(context.Background(), `UPDATE tenant_closure_settlements SET status = 'completed', completed_at = now()
WHERE brand_id = $1 AND tenant_id = $2`, tn.BrandID, tn.ID)
	return err
}

// Legacy usage that was never forwarded is recorded as a required manual
// review item, once, with an audit row, and is never touched.
func TestClosureRecordsRequiredReviewForLegacyUsage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, eventID, rollupID := closingWithLegacyUsage(t, e, true, true)
	result, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr-review")
	if err != nil || result.Recorded != 2 {
		t.Fatalf("record = %+v %v", result, err)
	}
	items, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %+v %v", items, err)
	}
	kinds := map[string]tenant.ClosureReview{}
	for _, it := range items {
		kinds[it.Kind] = it
		if it.Resolved || it.Reason == "" || it.Count != 1 {
			t.Fatalf("item = %+v", it)
		}
	}
	if kinds["legacy_usage_events"].Reason != "legacy_usage_unbilled" || kinds["legacy_usage_rollups"].Reason != "legacy_rollup_unsent" {
		t.Fatalf("kinds = %+v", kinds)
	}
	var audits int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_review_required'`, tn.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit rows = %d, %v", audits, err)
	}
	// A second pass records nothing more.
	if again, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr-review-2"); err != nil || again.Recorded != 0 {
		t.Fatalf("second pass = %+v %v", again, err)
	}
	// The legacy rows are exactly as they were: not rolled up, not sent, not billed.
	var n int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE id = $1 AND rollup_id IS NULL AND forwarded_at IS NULL AND acceptance_protocol = 0`, eventID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("legacy event changed: %d %v", n, err)
	}
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_rollups WHERE id = $1 AND status = 'pending' AND send_protocol = 0 AND forwarded_at IS NULL AND send_provenance = 'unknown'`, rollupID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("legacy rollup changed: %d %v", n, err)
	}
}

// Closure cannot complete while legacy usage has no resolved review, and
// completes once staff resolve it, with the usage still unbilled.
func TestClosureCannotCompleteUntilLegacyUsageIsResolved(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, eventID, _ := closingWithLegacyUsage(t, e, true, true)
	refused := func(stage string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if err := complete(e, tn); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: completion = %v, want a check violation", stage, err)
		}
	}
	refused("before any review is recorded")
	if _, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr"); err != nil {
		t.Fatal(err)
	}
	refused("with unresolved reviews")
	items, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %+v %v", items, err)
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), items[0].ID, reviewActor, "", "corr-resolve"); err == nil {
		t.Fatal("a review was resolved without a note")
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), items[0].ID, reviewActor, "staff checked the usage with finance", "corr-resolve"); err != nil {
		t.Fatal(err)
	}
	refused("with one review resolved")
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), items[1].ID, reviewActor, "staff checked the rollup with finance", "corr-resolve"); err != nil {
		t.Fatal(err)
	}
	if err := complete(e, tn); err != nil {
		t.Fatalf("completion after both reviews were resolved = %v", err)
	}
	resolved, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(resolved) != 2 || !resolved[0].Resolved || !resolved[1].Resolved || resolved[0].ResolvedBy == "" || resolved[0].Note == "" {
		t.Fatalf("resolved items = %+v %v", resolved, err)
	}
	var audits int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id = $1 AND action = 'tenant.closure_review_resolved'`, tn.ID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("resolution audit rows = %d, %v", audits, err)
	}
	// Resolving does not bill, send or discard anything.
	var n int
	if err := e.db.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE id = $1 AND rollup_id IS NULL AND forwarded_at IS NULL`, eventID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("legacy event changed by the resolution: %d %v", n, err)
	}
}

// A tenant with no legacy usage completes without any review.
func TestClosureCompletesWithoutReviewWhenThereIsNoLegacyUsage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, _, _ := closingWithLegacyUsage(t, e, false, false)
	if result, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr"); err != nil || result.Recorded != 0 {
		t.Fatalf("record = %+v %v", result, err)
	}
	if err := complete(e, tn); err != nil {
		t.Fatalf("completion without legacy usage = %v", err)
	}
}

// Review items and their resolutions are immutable, each with its own message.
func TestClosureReviewRecordsAreImmutable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, _, _ := closingWithLegacyUsage(t, e, true, false)
	if _, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr"); err != nil {
		t.Fatal(err)
	}
	items, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v %v", items, err)
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), items[0].ID, reviewActor, "reviewed", "corr-resolve"); err != nil {
		t.Fatal(err)
	}
	// The items table is referenced by the resolutions table, so Postgres only
	// lets a truncate of it through to the guard when both are named, and the
	// first one named answers.
	const item, resolution = "closure review item is immutable", "closure review resolution is immutable"
	for _, tc := range []struct{ name, statement, message string }{
		{"update item", `UPDATE tenant_closure_review_items SET reason = reason`, item},
		{"delete item", `DELETE FROM tenant_closure_review_items`, item},
		{"truncate both, item first", `TRUNCATE tenant_closure_review_items, tenant_closure_review_resolutions`, item},
		{"update resolution", `UPDATE tenant_closure_review_resolutions SET note = 'changed'`, resolution},
		{"delete resolution", `DELETE FROM tenant_closure_review_resolutions`, resolution},
		{"truncate resolution", `TRUNCATE tenant_closure_review_resolutions`, resolution},
		{"truncate both, resolution first", `TRUNCATE tenant_closure_review_resolutions, tenant_closure_review_items`, resolution},
	} {
		_, err := e.db.Pool.Exec(ctx, tc.statement)
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.Message != tc.message {
			t.Fatalf("%s = %v, want %q", tc.name, err, tc.message)
		}
	}
}

// Another tenant can neither list nor resolve this tenant's review items.
func TestClosureReviewsAreScopedToTheirTenant(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, _, _ := closingWithLegacyUsage(t, e, true, false)
	if _, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr"); err != nil {
		t.Fatal(err)
	}
	items, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v %v", items, err)
	}
	other := e.mustOrg(e.person("other@example.test"), "Tenant B")
	if listed, err := tenant.ListClosureReviews(ctx, e.db.Pool, other.Scope()); err != nil || len(listed) != 0 {
		t.Fatalf("another tenant lists %+v %v", listed, err)
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, other.Scope(), items[0].ID, reviewActor, "not mine", "corr-other"); !zserrors.IsCode(err, zserrors.CodeNotFound) {
		t.Fatalf("another tenant resolves = %v, want not found", err)
	}
	if again, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope()); err != nil || len(again) != 1 || again[0].Resolved {
		t.Fatalf("the item changed: %+v %v", again, err)
	}
}

// A rollup of protocol 0 that was rejected is unbilled usage too: it blocks
// completion until staff review it.
func TestClosureRejectedLegacyRollupBlocksCompletion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, _, _ := closingWithLegacyUsage(t, e, false, false)
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_rollups (id, brand_id, tenant_id, killbill_unit, day, period_start, record_at,
    quantity_micro, events, tracking_id, status, reject_reason, rejected_at)
VALUES ($1, $2, $3, 'api-call', current_date, now(), now(), 9, 1, 'legacy-rejected', 'rejected', 'before_subscription_start', now())`,
		idgen.MustNewV7(), tn.BrandID, tn.ID); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if err := complete(e, tn); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("completion with a rejected legacy rollup = %v, want a check violation", err)
	}
	if result, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr"); err != nil || result.Recorded != 1 {
		t.Fatalf("record = %+v %v", result, err)
	}
	items, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 1 || items[0].Kind != "legacy_usage_rollups" || items[0].Count != 1 {
		t.Fatalf("items = %+v %v", items, err)
	}
	if err := complete(e, tn); err == nil {
		t.Fatal("completion with an unresolved review succeeded")
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), items[0].ID, reviewActor, "rejected usage reviewed with finance", "corr-resolve"); err != nil {
		t.Fatal(err)
	}
	if err := complete(e, tn); err != nil {
		t.Fatalf("completion after the review = %v", err)
	}
}

// A review pins what was reviewed. Legacy usage that appears after it needs
// another review, for the larger count.
func TestClosureLegacyUsageThatGrowsAfterReviewNeedsAnotherReview(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn, _, _ := closingWithLegacyUsage(t, e, true, false)
	if _, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr"); err != nil {
		t.Fatal(err)
	}
	items, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 1 || items[0].Count != 1 {
		t.Fatalf("items = %+v %v", items, err)
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), items[0].ID, reviewActor, "one event reviewed", "corr-resolve"); err != nil {
		t.Fatal(err)
	}
	var meterID idgen.UUID
	if err := e.db.Pool.QueryRow(ctx, `SELECT id FROM meters WHERE brand_id = $1 AND slug = 'legacy-meter'`, tn.BrandID).Scan(&meterID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool.Exec(ctx, `INSERT INTO usage_events (id, brand_id, tenant_id, meter_id, event_id, killbill_unit, quantity_micro,
    dimensions, dimensions_key, occurred_at, received_at) VALUES ($1, $2, $3, $4, 'legacy-b', 'api-call', 3, '{}'::jsonb, '{}', now(), now())`,
		idgen.MustNewV7(), tn.BrandID, tn.ID, meterID); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if err := complete(e, tn); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("completion after the usage grew = %v, want a check violation", err)
	}
	if result, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr-2"); err != nil || result.Recorded != 1 {
		t.Fatalf("second record = %+v %v", result, err)
	}
	if again, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr-3"); err != nil || again.Recorded != 0 {
		t.Fatalf("a third record = %+v %v, want none", again, err)
	}
	items, err = tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %+v %v", items, err)
	}
	var fresh tenant.ClosureReview
	for _, it := range items {
		if it.Count == 2 {
			fresh = it
		}
	}
	if fresh.Count != 2 || fresh.Resolved {
		t.Fatalf("items = %+v", items)
	}
	if err := complete(e, tn); err == nil {
		t.Fatal("completion before the new review was resolved succeeded")
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), fresh.ID, reviewActor, "both events reviewed", "corr-resolve-2"); err != nil {
		t.Fatal(err)
	}
	if err := complete(e, tn); err != nil {
		t.Fatalf("completion after the second review = %v", err)
	}
}
