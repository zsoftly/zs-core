//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

func TestUsageDispatchRuleMatrix(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"R02/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := idgen.MustNewV7()
			if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_rollups (id, brand_id, tenant_id, killbill_unit, day,
                period_start, record_at, quantity_micro, events, tracking_id) VALUES ($1, $2, $3, 'api-call',
                current_date, clock_timestamp(), clock_timestamp(), 1, 1, 'r02-positive')`, rollupID, brandID, tenantID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET closure_unresolved_reason = 'legacy_usage_send_unconfirmed'
                WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R02 legacy diagnostic = %v", err)
			}
		}},
		{"R02/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := idgen.MustNewV7()
			if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_rollups (id, brand_id, tenant_id, killbill_unit, day,
                period_start, record_at, quantity_micro, events, tracking_id) VALUES ($1, $2, $3, 'api-call',
                current_date, clock_timestamp(), clock_timestamp(), 1, 1, 'r02-forbidden')`, rollupID, brandID, tenantID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_protocol = 1 WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R02 legacy promotion succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R04/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r04-positive")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET subscription_id = $2, target_endpoint_fingerprint = $3,
                target_killbill_tenant_id = $4, target_killbill_account_id = $5 WHERE id = $1`,
				rollupID, idgen.MustNewV7(), strings.Repeat("1", 64), idgen.MustNewV7(), idgen.MustNewV7()); err != nil {
				t.Fatalf("R04 bind = %v", err)
			}
		}},
		{"R04/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r04-forbidden")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET target_endpoint_fingerprint = $2 WHERE id = $1`,
				rollupID, strings.Repeat("2", 64)); err == nil {
				t.Fatal("R04 partial target bind succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R05/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r05-positive")
			usageDispatchClaim(t, db, rollupID, "15151515151515151515151515151515")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET subscription_id = $2, target_endpoint_fingerprint = $3,
                target_killbill_tenant_id = $4, target_killbill_account_id = $5 WHERE id = $1`,
				rollupID, idgen.MustNewV7(), strings.Repeat("3", 64), idgen.MustNewV7(), idgen.MustNewV7()); err != nil {
				t.Fatalf("R05 live claim bind = %v", err)
			}
		}},
		{"R05/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r05-forbidden")
			usageDispatchClaim(t, db, rollupID, "16161616161616161616161616161616")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET target_killbill_account_id = $2 WHERE id = $1`,
				rollupID, idgen.MustNewV7()); err == nil {
				t.Fatal("R05 partial live target bind succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R06/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchStartedRollup(t, db, brandID, tenantID, "r06-positive")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_payload = send_payload WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R06 started no-op = %v", err)
			}
		}},
		{"R06/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchStartedRollup(t, db, brandID, tenantID, "r06-forbidden")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_payload = '{"usage":2}'::jsonb WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R06 body rewrite succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R09/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r09-positive")
			usageDispatchClaim(t, db, rollupID, "10101010101010101010101010101010")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_provenance = 'never_attempted',
                claim_from_never = false, claim_token = NULL, claimed_until = NULL, attempts = attempts + 1,
                next_attempt_at = clock_timestamp() + interval '1 minute', last_error = 'usage_payload_not_ready'
                WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R09 retry = %v", err)
			}
		}},
		{"R09/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r09-forbidden")
			usageDispatchClaim(t, db, rollupID, "12121212121212121212121212121212")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_provenance = 'never_attempted',
                claim_from_never = false, claim_token = NULL, claimed_until = NULL, attempts = attempts + 2,
                next_attempt_at = clock_timestamp() + interval '1 minute', last_error = 'usage_payload_not_ready'
                WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R09 multi-attempt retry succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R10/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r10-positive")
			usageDispatchClaim(t, db, rollupID, "13131313131313131313131313131313")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_provenance = 'never_attempted',
                claim_from_never = false, claim_token = NULL, claimed_until = NULL,
                next_attempt_at = clock_timestamp() + interval '1 minute' WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R10 active scheduling = %v", err)
			}
		}},
		{"R10/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r10-forbidden")
			usageDispatchClaim(t, db, rollupID, "14141414141414141414141414141414")
			if _, err := db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, brandID, tenantID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_provenance = 'never_attempted',
                claim_from_never = false, claim_token = NULL, claimed_until = NULL,
                next_attempt_at = clock_timestamp() + interval '1 minute' WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R10 closing scheduling succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R11/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r11-positive")
			usageDispatchClaim(t, db, rollupID, "44444444444444444444444444444444")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_provenance = 'never_attempted',
                claim_from_never = false, claim_token = NULL, claimed_until = NULL, status = 'rejected',
                reject_reason = 'before_subscription_start', rejected_at = clock_timestamp() WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R11 pre-send rejection = %v", err)
			}
		}},
		{"R11/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r11-forbidden")
			usageDispatchClaim(t, db, rollupID, "55555555555555555555555555555555")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET send_provenance = 'never_attempted',
                claim_from_never = false, claim_token = NULL, claimed_until = NULL, status = 'rejected',
                reject_reason = 'unknown', rejected_at = clock_timestamp() WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R11 unknown rejection succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R12/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r12-positive")
			usageDispatchClaim(t, db, rollupID, "66666666666666666666666666666666")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'rejected',
			    reject_reason = 'subscription_cancelled', rejected_at = clock_timestamp(),
			    send_provenance = 'never_attempted', claim_from_never = false,
			    claim_token = NULL, claimed_until = NULL WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R12 fixture rejection = %v", err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'pending', subscription_id = $2,
                reject_reason = NULL, rejected_at = NULL, attempts = 0, last_error = NULL,
                next_attempt_at = clock_timestamp() + interval '1 minute' WHERE id = $1`, rollupID, idgen.MustNewV7()); err != nil {
				t.Fatalf("R12 reassignment = %v", err)
			}
		}},
		{"R12/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r12-forbidden")
			usageDispatchClaim(t, db, rollupID, "77777777777777777777777777777777")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'rejected',
			    reject_reason = 'subscription_cancelled', rejected_at = clock_timestamp(),
			    send_provenance = 'never_attempted', claim_from_never = false,
			    claim_token = NULL, claimed_until = NULL WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R12 fixture rejection = %v", err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'pending', subscription_id = $2,
                reject_reason = NULL, rejected_at = NULL, attempts = 1, last_error = NULL,
                next_attempt_at = clock_timestamp() + interval '1 minute' WHERE id = $1`, rollupID, idgen.MustNewV7()); err == nil {
				t.Fatal("R12 reassignment with retained attempt succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R13/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r13-positive")
			usageDispatchClaim(t, db, rollupID, "88888888888888888888888888888888")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'rejected',
			    reject_reason = 'subscription_cancelled', rejected_at = clock_timestamp(),
			    send_provenance = 'never_attempted', claim_from_never = false,
			    claim_token = NULL, claimed_until = NULL WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R13 fixture rejection = %v", err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'discarded',
                discarded_at = clock_timestamp() WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R13 discard = %v", err)
			}
		}},
		{"R13/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r13-forbidden")
			usageDispatchClaim(t, db, rollupID, "99999999999999999999999999999999")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'rejected',
			    reject_reason = 'subscription_cancelled', rejected_at = clock_timestamp(),
			    send_provenance = 'never_attempted', claim_from_never = false,
			    claim_token = NULL, claimed_until = NULL WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R13 fixture rejection = %v", err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'discarded',
                discarded_at = clock_timestamp(), last_error = 'changed' WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R13 discard with mutation succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
	} {
		t.Run(test.name, test.run)
	}
}

func TestUsageDispatchStartedRuleMatrix(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"R15/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchStartedRollup(t, db, brandID, tenantID, "r15-positive")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'forwarded', forwarded_at = clock_timestamp(),
                claim_token = NULL, claimed_until = NULL, claim_from_never = false, last_error = NULL WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R15 original positive forward = %v", err)
			}
		}},
		{"R15/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchStartedRollup(t, db, brandID, tenantID, "r15-forbidden")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET status = 'forwarded', forwarded_at = clock_timestamp(),
                claim_token = NULL, claimed_until = NULL, claim_from_never = false, last_error = 'changed' WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R15 forward with error succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R16/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchStartedRollup(t, db, brandID, tenantID, "r16-positive")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET closure_unresolved_reason = 'usage_transport_unconfirmed',
                last_error = 'usage_transport_unconfirmed', claim_from_never = false WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R16 transport uncertainty = %v", err)
			}
		}},
		{"R16/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchStartedRollup(t, db, brandID, tenantID, "r16-forbidden")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET closure_unresolved_reason = 'usage_transport_unconfirmed',
                last_error = 'different_error', claim_from_never = false WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R16 mismatched uncertainty error succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
	} {
		t.Run(test.name, test.run)
	}
}

func TestUsageDispatchExpiredClaimRuleMatrix(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"R17/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r17-positive")
			usageDispatchClaimFor(t, db, rollupID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "1 millisecond")
			if _, err := db.Pool.Exec(ctx, `SELECT pg_sleep(0.02)`); err != nil {
				t.Fatalf("R17 bounded expiry wait = %v", err)
			}
			var expired bool
			if err := db.Pool.QueryRow(ctx, `SELECT claimed_until <= clock_timestamp() FROM usage_rollups WHERE id = $1`, rollupID).Scan(&expired); err != nil || !expired {
				t.Fatalf("R17 expiry predicate = %v %v", expired, err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET closure_unresolved_reason = 'usage_claim_unconfirmed',
                last_error = 'usage_claim_unconfirmed', claim_from_never = false WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R17 expired claim uncertainty = %v", err)
			}
		}},
		{"R17/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r17-forbidden")
			usageDispatchClaimFor(t, db, rollupID, "cccccccccccccccccccccccccccccccc", "1 millisecond")
			if _, err := db.Pool.Exec(ctx, `SELECT pg_sleep(0.02)`); err != nil {
				t.Fatalf("R17 bounded expiry wait = %v", err)
			}
			var expired bool
			if err := db.Pool.QueryRow(ctx, `SELECT claimed_until <= clock_timestamp() FROM usage_rollups WHERE id = $1`, rollupID).Scan(&expired); err != nil || !expired {
				t.Fatalf("R17 expiry predicate = %v %v", expired, err)
			}
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_rollups SET claim_token = NULL, claimed_until = NULL,
                claim_from_never = false, send_provenance = 'never_attempted' WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R17 expired claim release succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
	} {
		t.Run(test.name, test.run)
	}
}

func TestUsageDispatchEventRuleMatrix(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"E01/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "e01-positive")
			eventID, occurredAt := usageDispatchTrustedEvent(t, db, brandID, tenantID, "e01-positive")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_events SET rollup_id = $3 WHERE occurred_at = $1 AND id = $2`, occurredAt, eventID, rollupID); err != nil {
				t.Fatalf("E01 link = %v", err)
			}
		}},
		{"E01/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			ctx := context.Background()
			legacyID := idgen.MustNewV7()
			if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_rollups (id, brand_id, tenant_id, killbill_unit, day,
                period_start, record_at, quantity_micro, events, tracking_id) VALUES ($1, $2, $3, 'api-call',
                current_date, clock_timestamp(), clock_timestamp(), 1, 1, 'e01-legacy')`, legacyID, brandID, tenantID); err != nil {
				t.Fatal(err)
			}
			eventID, occurredAt := usageDispatchTrustedEvent(t, db, brandID, tenantID, "e01-forbidden")
			if _, err := db.Pool.Exec(ctx, `UPDATE usage_events SET rollup_id = $3 WHERE occurred_at = $1 AND id = $2`, occurredAt, eventID, legacyID); err == nil {
				t.Fatal("E01 legacy parent link succeeded")
			} else {
				assertCheckViolation(t, err, "usage_event_update_forbidden")
			}
		}},
	} {
		t.Run(test.name, test.run)
	}
}

func TestUsageDispatchClosureRuleMatrix(t *testing.T) {
	cutoff := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	pre := cutoff.Add(-time.Hour)
	outside := cutoff.Add(time.Hour)
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"R01/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r01-positive")
			preID, preAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "r01-pre", pre)
			outsideID, outsideAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "r01-outside", outside)
			usageDispatchLinkEvent(t, db, preID, preAt, rollupID)
			usageDispatchLinkEvent(t, db, outsideID, outsideAt, rollupID)
			usageDispatchStartRollup(t, db, rollupID)
			usageDispatchForwardRollup(t, db, rollupID)
			usageDispatchClosingCutoff(t, db, brandID, tenantID, cutoff)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET closure_unresolved_reason = 'mixed_cutoff_send_unconfirmed' WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R01 mixed forwarded diagnostic = %v", err)
			}
		}},
		{"R01/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			rollupID := usageDispatchStartedRollup(t, db, brandID, tenantID, "r01-forbidden")
			usageDispatchForwardRollup(t, db, rollupID)
			usageDispatchClosingCutoff(t, db, brandID, tenantID, cutoff)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET closure_unresolved_reason = 'mixed_cutoff_send_unconfirmed' WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R01 nonmixed forwarded diagnostic succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R14/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r14-positive")
			eventID, eventAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "r14-outside", outside)
			usageDispatchLinkEvent(t, db, eventID, eventAt, rollupID)
			usageDispatchClosingCutoff(t, db, brandID, tenantID, cutoff)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET status = 'superseded' WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R14 outside split = %v", err)
			}
		}},
		{"R14/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r14-forbidden")
			eventID, eventAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "r14-pre", pre)
			usageDispatchLinkEvent(t, db, eventID, eventAt, rollupID)
			usageDispatchClosingCutoff(t, db, brandID, tenantID, cutoff)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET status = 'superseded' WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R14 within-cutoff supersede succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
		{"R18/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "r18-positive")
			preID, preAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "r18-pre", pre)
			outsideID, outsideAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "r18-outside", outside)
			usageDispatchLinkEvent(t, db, preID, preAt, rollupID)
			usageDispatchLinkEvent(t, db, outsideID, outsideAt, rollupID)
			usageDispatchStartRollup(t, db, rollupID)
			usageDispatchClosingCutoff(t, db, brandID, tenantID, cutoff)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET closure_unresolved_reason = 'mixed_cutoff_send_unconfirmed' WHERE id = $1`, rollupID); err != nil {
				t.Fatalf("R18 mixed started hold = %v", err)
			}
		}},
		{"R18/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			rollupID := usageDispatchStartedRollup(t, db, brandID, tenantID, "r18-forbidden")
			usageDispatchClosingCutoff(t, db, brandID, tenantID, cutoff)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET closure_unresolved_reason = 'mixed_cutoff_send_unconfirmed' WHERE id = $1`, rollupID); err == nil {
				t.Fatal("R18 nonmixed hold succeeded")
			} else {
				assertCheckViolation(t, err, "usage_rollup_update_forbidden")
			}
		}},
	} {
		t.Run(test.name, test.run)
	}
}

func TestUsageDispatchEventTransitionRuleMatrix(t *testing.T) {
	cutoff := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	pre := cutoff.Add(-time.Hour)
	outside := cutoff.Add(time.Hour)
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"E02/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			parentID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "e02-parent")
			childID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "e02-child")
			eventID, eventAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "e02-pre", pre)
			usageDispatchLinkEvent(t, db, eventID, eventAt, parentID)
			usageDispatchClosingCutoff(t, db, brandID, tenantID, cutoff)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_events SET rollup_id = $3 WHERE occurred_at = $1 AND id = $2`, eventAt, eventID, childID); err != nil {
				t.Fatalf("E02 pre-cutoff move = %v", err)
			}
		}},
		{"E02/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			parentID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "e02-forbidden-parent")
			childID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "e02-forbidden-child")
			eventID, eventAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "e02-outside", outside)
			usageDispatchLinkEvent(t, db, eventID, eventAt, parentID)
			usageDispatchClosingCutoff(t, db, brandID, tenantID, cutoff)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_events SET rollup_id = $3 WHERE occurred_at = $1 AND id = $2`, eventAt, eventID, childID); err == nil {
				t.Fatal("E02 outside move succeeded")
			} else {
				assertCheckViolation(t, err, "usage_event_update_forbidden")
			}
		}},
		{"E03/positive", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			trackingID := "e03-positive"
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, trackingID)
			eventID, eventAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "e03-event", pre)
			usageDispatchLinkEvent(t, db, eventID, eventAt, rollupID)
			usageDispatchStartRollup(t, db, rollupID)
			usageDispatchForwardRollup(t, db, rollupID)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_events SET forwarded_at = clock_timestamp(), killbill_tracking_id = $3 WHERE occurred_at = $1 AND id = $2`, eventAt, eventID, trackingID); err != nil {
				t.Fatalf("E03 receipt metadata = %v", err)
			}
		}},
		{"E03/forbidden", func(t *testing.T) {
			db, brandID, tenantID := usageDispatchScope(t)
			rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, "e03-forbidden")
			eventID, eventAt := usageDispatchTrustedEventAt(t, db, brandID, tenantID, "e03-wrong", pre)
			usageDispatchLinkEvent(t, db, eventID, eventAt, rollupID)
			usageDispatchStartRollup(t, db, rollupID)
			usageDispatchForwardRollup(t, db, rollupID)
			if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_events SET forwarded_at = clock_timestamp(), killbill_tracking_id = 'wrong' WHERE occurred_at = $1 AND id = $2`, eventAt, eventID); err == nil {
				t.Fatal("E03 wrong receipt metadata succeeded")
			} else {
				assertCheckViolation(t, err, "usage_event_update_forbidden")
			}
		}},
	} {
		t.Run(test.name, test.run)
	}
}

func TestIssue14MigrationsUpgradeReleasedTwentyOneWithoutBackfill(t *testing.T) {
	db := postgrestest.NewEmpty(t)
	ctx := context.Background()
	throughTwentyOne := fstest.MapFS{}
	var wantApplied []int64
	loaded, err := database.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range loaded {
		if migration.Version > 21 {
			wantApplied = append(wantApplied, migration.Version)
			continue
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
	if err != nil || !slices.Equal(applied, wantApplied) {
		t.Fatalf("upgrade = %v %v, want %v", applied, err, wantApplied)
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

func usageDispatchTrustedRollup(t *testing.T, db *postgrestest.DB, brandID, tenantID idgen.UUID, trackingID string) idgen.UUID {
	t.Helper()
	rollupID := idgen.MustNewV7()
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO usage_rollups (
        id, brand_id, tenant_id, killbill_unit, day, period_start, record_at,
        quantity_micro, events, tracking_id, send_protocol, send_provenance,
        event_set_fingerprint
    ) VALUES ($1, $2, $3, 'api-call', $4, $5, $5, 1, 1, $6, 1, 'never_attempted', $7)`,
		rollupID, brandID, tenantID, now.Format(time.DateOnly), now, trackingID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	return rollupID
}

func usageDispatchClaim(t *testing.T, db *postgrestest.DB, rollupID idgen.UUID, token string) {
	t.Helper()
	usageDispatchClaimFor(t, db, rollupID, token, "1 minute")
}

func usageDispatchClaimFor(t *testing.T, db *postgrestest.DB, rollupID idgen.UUID, token, lifetime string) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET send_provenance = 'possibly_attempted',
        claim_from_never = true, claim_token = decode($2, 'hex'),
	        claimed_until = clock_timestamp() + $3::interval WHERE id = $1`, rollupID, token, lifetime); err != nil {
		t.Fatal(err)
	}
}

func usageDispatchStartedRollup(t *testing.T, db *postgrestest.DB, brandID, tenantID idgen.UUID, trackingID string) idgen.UUID {
	t.Helper()
	rollupID := usageDispatchTrustedRollup(t, db, brandID, tenantID, trackingID)
	usageDispatchStartRollup(t, db, rollupID)
	return rollupID
}

func usageDispatchStartRollup(t *testing.T, db *postgrestest.DB, rollupID idgen.UUID) {
	t.Helper()
	usageDispatchClaim(t, db, rollupID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET subscription_id = $2,
        target_endpoint_fingerprint = $3, target_killbill_tenant_id = $4,
        target_killbill_account_id = $5 WHERE id = $1`,
		rollupID, idgen.MustNewV7(), strings.Repeat("b", 64), idgen.MustNewV7(), idgen.MustNewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET send_started_at = clock_timestamp(),
        send_payload = '{"usage":1}'::jsonb WHERE id = $1`, rollupID); err != nil {
		t.Fatal(err)
	}
}

func usageDispatchTrustedEvent(t *testing.T, db *postgrestest.DB, brandID, tenantID idgen.UUID, eventName string) (idgen.UUID, time.Time) {
	return usageDispatchTrustedEventAt(t, db, brandID, tenantID, eventName, time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC))
}

func usageDispatchTrustedEventAt(t *testing.T, db *postgrestest.DB, brandID, tenantID idgen.UUID, eventName string, occurredAt time.Time) (idgen.UUID, time.Time) {
	t.Helper()
	ctx := context.Background()
	meterID := idgen.MustNewV7()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO meters (id, brand_id, slug, unit, killbill_unit)
        VALUES ($1, $2, $3, 'count', 'api-call')`, meterID, brandID, "meter-"+eventName); err != nil {
		t.Fatal(err)
	}
	eventID := idgen.MustNewV7()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_events (id, brand_id, tenant_id, meter_id, event_id,
        killbill_unit, quantity_micro, dimensions, dimensions_key, occurred_at, received_at, acceptance_protocol)
        VALUES ($1, $2, $3, $4, $5, 'api-call', 1, '{}'::jsonb, '{}', $6, $6, 1)`,
		eventID, brandID, tenantID, meterID, eventName, occurredAt); err != nil {
		t.Fatal(err)
	}
	return eventID, occurredAt
}

func usageDispatchLinkEvent(t *testing.T, db *postgrestest.DB, eventID idgen.UUID, occurredAt time.Time, rollupID idgen.UUID) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_events SET rollup_id = $3 WHERE occurred_at = $1 AND id = $2`, occurredAt, eventID, rollupID); err != nil {
		t.Fatal(err)
	}
}

func usageDispatchClosingCutoff(t *testing.T, db *postgrestest.DB, brandID, tenantID idgen.UUID, cutoff time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `UPDATE tenants SET status = 'closing' WHERE brand_id = $1 AND id = $2`, brandID, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO tenant_closure_settlements (brand_id, tenant_id, cutoff_at) VALUES ($1, $2, $3)`, brandID, tenantID, cutoff); err != nil {
		t.Fatal(err)
	}
}

func usageDispatchForwardRollup(t *testing.T, db *postgrestest.DB, rollupID idgen.UUID) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_rollups SET status = 'forwarded', forwarded_at = clock_timestamp(),
        claim_token = NULL, claimed_until = NULL, claim_from_never = false, last_error = NULL WHERE id = $1`, rollupID); err != nil {
		t.Fatal(err)
	}
}

func assertCheckViolation(t *testing.T, err error, message string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || (message != "" && pgErr.Message != message) {
		t.Fatalf("check violation = %v", err)
	}
}
