//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/migrations"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

func TestUsageDispatchMigrationDefaultsLegacyRowsAndGuardsTrustedRows(t *testing.T) {
	db, brandID, tenantID := usageDispatchScope(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	trustedID := idgen.MustNewV7()
	legacyID := idgen.MustNewV7()
	trustedFingerprint := strings.Repeat("a", 64)

	if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_rollups (
        id, brand_id, tenant_id, killbill_unit, day, period_start, record_at,
        quantity_micro, events, tracking_id, send_protocol, send_provenance,
        event_set_fingerprint
    ) VALUES ($1, $2, $3, 'api-call', $4, $5, $5, 1, 1, 'trusted-dispatch', 1, 'never_attempted', $6)`,
		trustedID, brandID, tenantID, now.Format(time.DateOnly), now, trustedFingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET quantity_micro = 2 WHERE id = $1`, trustedID); err == nil {
		t.Fatal("trusted evidence mutation succeeded")
	} else {
		assertCheckViolation(t, err, "usage_rollup_update_forbidden")
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = status WHERE id = $1`, trustedID); err != nil {
		t.Fatalf("trusted no-op update = %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_provenance = 'possibly_attempted',
        claim_from_never = true, claim_token = decode('00000000000000000000000000000000', 'hex'),
        claimed_until = clock_timestamp() + interval '1 minute' WHERE id = $1`, trustedID); err != nil {
		t.Fatalf("trusted claim = %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET claimed_until = claimed_until + interval '1 minute' WHERE id = $1`, trustedID); err != nil {
		t.Fatalf("trusted claim renewal = %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_provenance = 'never_attempted',
        claim_from_never = false, claim_token = NULL, claimed_until = NULL WHERE id = $1`, trustedID); err != nil {
		t.Fatalf("trusted pre-send release = %v", err)
	}
	for _, sql := range []string{
		`UPDATE usage_rollups SET send_protocol = 0 WHERE id = $1`,
		`UPDATE usage_rollups SET send_payload = '{}'::jsonb WHERE id = $1`,
		`UPDATE usage_rollups SET target_endpoint_fingerprint = '` + strings.Repeat("f", 64) + `' WHERE id = $1`,
	} {
		if _, err := db.Pool.Exec(ctx, sql, trustedID); err == nil {
			t.Fatalf("forbidden trusted update succeeded: %s", sql)
		} else {
			assertCheckViolation(t, err, "usage_rollup_update_forbidden")
		}
	}

	if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_rollups (
        id, brand_id, tenant_id, killbill_unit, day, period_start, record_at,
        quantity_micro, events, tracking_id
    ) VALUES ($1, $2, $3, 'api-call', $4, $5, $5, 1, 1, 'legacy-dispatch')`,
		legacyID, brandID, tenantID, now.Format(time.DateOnly), now); err != nil {
		t.Fatal(err)
	}
	var protocol int
	var provenance string
	if err := db.Pool.QueryRow(ctx, `SELECT send_protocol, send_provenance FROM usage_rollups WHERE id = $1`, legacyID).Scan(&protocol, &provenance); err != nil {
		t.Fatal(err)
	}
	if protocol != 0 || provenance != "unknown" {
		t.Fatalf("legacy dispatch protocol = %d %q", protocol, provenance)
	}
}

func TestUsageDispatchMigrationStoresDurableSweepCursorKinds(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sweep_cursor (kind) VALUES ('creation'), ('closure')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO sweep_cursor (kind) VALUES ('unknown')`); err == nil {
		t.Fatal("unknown sweep cursor kind succeeded")
	} else {
		assertCheckViolation(t, err, "")
	}
}

func TestIssue14MigrationsUpgradeReleasedTwentyOneWithoutBackfill(t *testing.T) {
	db := postgrestest.NewEmpty(t)
	ctx := context.Background()
	throughTwentyOne := fstest.MapFS{}
	loaded, err := database.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if migration.Version > 21 {
			break
		}
		name := fmt.Sprintf("%06d_%s.up.sql", migration.Version, migration.Name)
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		throughTwentyOne[name] = &fstest.MapFile{Data: body}
	}
	if _, err := database.Migrate(ctx, db.Pool, throughTwentyOne); err != nil {
		t.Fatal(err)
	}
	brandID, tenantID := usageDispatchScopeOnDB(t, db)
	legacyID := idgen.MustNewV7()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_rollups (
        id, brand_id, tenant_id, killbill_unit, day, period_start, record_at,
        quantity_micro, events, tracking_id
    ) VALUES ($1, $2, $3, 'api-call', current_date, now(), now(), 1, 1, 'upgrade-legacy')`, legacyID, brandID, tenantID); err != nil {
		t.Fatal(err)
	}
	applied, err := database.Migrate(ctx, db.Pool, migrations.FS)
	if err != nil || len(applied) != 2 {
		t.Fatalf("upgrade = %v %v", applied, err)
	}
	if applied, err := database.Migrate(ctx, db.Pool, migrations.FS); err != nil || len(applied) != 0 {
		t.Fatalf("upgrade noop = %v %v", applied, err)
	}
	var protocol int
	var provenance string
	if err := db.Pool.QueryRow(ctx, `SELECT send_protocol, send_provenance FROM usage_rollups WHERE id = $1`, legacyID).Scan(&protocol, &provenance); err != nil {
		t.Fatal(err)
	}
	if protocol != 0 || provenance != "unknown" {
		t.Fatalf("upgraded legacy = %d %q", protocol, provenance)
	}
}

func TestCreditSourceAttemptsKeepLegacyOriginalTargetsNull(t *testing.T) {
	db, brandID, tenantID := usageDispatchScope(t)
	ctx := context.Background()
	for _, table := range []string{"credit_code_redemptions", "staff_credits"} {
		for _, column := range []string{
			"original_endpoint_kind", "original_killbill_tenant_id", "original_killbill_account_id",
			"original_currency", "original_amount_minor", "original_description",
		} {
			var found bool
			if err := db.Pool.QueryRow(ctx, `SELECT EXISTS (
                SELECT 1 FROM information_schema.columns
                WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2
            )`, table, column).Scan(&found); err != nil || !found {
				t.Fatalf("%s.%s = %v %v", table, column, found, err)
			}
		}
	}
	creditID := idgen.MustNewV7()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO staff_credits (
        id, brand_id, tenant_id, idempotency_key, fingerprint, amount_minor,
        currency, reason, actor_type, actor_id, correlation_id
    ) VALUES ($1, $2, $3, 'original-target-legacy', $4, 100, 'CAD', 'legacy target',
        'staff', 'sub:operator', 'original-target-legacy')`,
		creditID, brandID, tenantID, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	var endpointKind *string
	var originalTenantID *idgen.UUID
	var originalAccountID *idgen.UUID
	var currency *string
	var amountMinor *int64
	var description *string
	if err := db.Pool.QueryRow(ctx, `SELECT original_endpoint_kind, original_killbill_tenant_id,
        original_killbill_account_id, original_currency, original_amount_minor, original_description
		FROM staff_credits WHERE brand_id = $1 AND tenant_id = $2 AND id = $3`,
		brandID, tenantID, creditID).Scan(&endpointKind, &originalTenantID, &originalAccountID, &currency, &amountMinor, &description); err != nil {
		t.Fatal(err)
	}
	if endpointKind != nil || originalTenantID != nil || originalAccountID != nil || currency != nil || amountMinor != nil || description != nil {
		t.Fatalf("legacy original target = %v %v %v %v %v %v", endpointKind, originalTenantID, originalAccountID, currency, amountMinor, description)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE staff_credits SET original_endpoint_kind = 'grant' WHERE id = $1`, creditID); err == nil {
		t.Fatal("legacy original target promotion succeeded")
	} else {
		assertCheckViolation(t, err, "")
	}
}

func TestUsageEventsMigrationGuardsTrustedAcceptance(t *testing.T) {
	db, brandID, tenantID := usageDispatchScope(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	meterID := idgen.MustNewV7()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO meters (id, brand_id, slug, unit, killbill_unit)
        VALUES ($1, $2, 'dispatch-events', 'count', 'api-call')`, meterID, brandID); err != nil {
		t.Fatal(err)
	}
	eventID := idgen.MustNewV7()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_events (id, brand_id, tenant_id, meter_id, event_id,
        killbill_unit, quantity_micro, dimensions, dimensions_key, occurred_at, received_at, acceptance_protocol)
        VALUES ($1, $2, $3, $4, 'trusted-event', 'api-call', 1, '{}'::jsonb, '{}', $5, $5, 1)`,
		eventID, brandID, tenantID, meterID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE usage_events SET rollup_id = rollup_id WHERE occurred_at = $1 AND id = $2`, now, eventID); err != nil {
		t.Fatalf("trusted event no-op update = %v", err)
	}
	for _, sql := range []string{
		`UPDATE usage_events SET acceptance_protocol = 0 WHERE occurred_at = $1 AND id = $2`,
		`UPDATE usage_events SET quantity_micro = 2 WHERE occurred_at = $1 AND id = $2`,
	} {
		if _, err := db.Pool.Exec(ctx, sql, now, eventID); err == nil {
			t.Fatalf("forbidden trusted event update succeeded: %s", sql)
		} else {
			assertCheckViolation(t, err, "usage_event_update_forbidden")
		}
	}
}

func TestClosureHoldMigrationKeepsHoldAndResolutionImmutable(t *testing.T) {
	db, brandID, tenantID := usageDispatchScope(t)
	ctx := context.Background()
	cutoff := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	if _, err := db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, brandID, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (
        brand_id, tenant_id, cutoff_at
    ) VALUES ($1, $2, $3)`, brandID, tenantID, cutoff); err != nil {
		t.Fatal(err)
	}
	holdID := idgen.MustNewV7()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO tenant_closure_bundle_holds (
        brand_id, tenant_id, id, cutoff_at, killbill_tenant_id, killbill_account_id,
        bundle_id, base_subscription_id, reason, evidence, correlation_id
    ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'already_cancelled_base', '{}'::jsonb, 'hold-test')`,
		brandID, tenantID, holdID, cutoff, idgen.MustNewV7(), idgen.MustNewV7(), idgen.MustNewV7(), idgen.MustNewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE tenant_closure_bundle_holds SET evidence = '{"changed":true}'::jsonb WHERE id = $1`, holdID); err == nil {
		t.Fatal("closure hold evidence update succeeded")
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO closure_hold_resolution (
        hold_id, decision, resolved_by, upstream_readback_sha256
    ) VALUES ($1, 'release', 'sub:operator', $2)`, holdID, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE closure_hold_resolution SET decision = 'manual_forfeit' WHERE hold_id = $1`, holdID); err == nil {
		t.Fatal("closure hold resolution update succeeded")
	}
	if _, err := db.Pool.Exec(ctx, `TRUNCATE closure_hold_resolution`); err == nil {
		t.Fatal("closure hold resolution truncate succeeded")
	}
}

func usageDispatchScope(t *testing.T) (*postgrestest.DB, idgen.UUID, idgen.UUID) {
	t.Helper()
	db := postgrestest.New(t)
	brandID, tenantID := usageDispatchScopeOnDB(t, db)
	return db, brandID, tenantID
}

func usageDispatchScopeOnDB(t *testing.T, db *postgrestest.DB) (idgen.UUID, idgen.UUID) {
	t.Helper()
	fake := enginetest.New(t)
	brand := fake.InsertBrand(t, db.Pool, "usage-dispatch")
	person := enginetest.InsertPerson(t, db.Pool, brand, "usage-dispatch@example.test")
	tenantID := idgen.MustNewV7()
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO tenants (
        id, brand_id, kind, slug, name, activation, keycloak_organization_id,
        killbill_account_id, created_by_identity_id
    ) VALUES ($1, $2, 'organization', 'usage-dispatch', 'Usage Dispatch', 'approved',
        'usage-dispatch-org', $3, $4)`, tenantID, brand.ID, idgen.MustNewV7(), person.Identity.ID); err != nil {
		t.Fatal(err)
	}
	return brand.ID, tenantID
}

func assertCheckViolation(t *testing.T, err error, message string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || (message != "" && pgErr.Message != message) {
		t.Fatalf("check violation = %v", err)
	}
}
