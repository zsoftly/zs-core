//go:build integration

package brand

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/outbox"
)

func TestRecordManifestWebhookCallbackSeesNewManifestAndCommitsWithIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := postgrestest.New(t)
	before := brandManifest(t, "zs-hook", "env://BRAND_ZS_HOOK__KEYCLOAK_STAFF_CLIENT_SECRET")
	brandID := insertBrand(t, db, before)
	after := *before
	after.Notifications = &Notifications{Webhooks: []Webhook{{
		URL: "https://receiver.example/hook", SecretRef: "env://BRAND_ZS_HOOK__WEBHOOK",
	}}}
	var callbackRecord Record
	deps := Deps{DB: db.Pool, AfterManifestRecorded: func(ctx context.Context, tx pgx.Tx, record Record, _ Options) error {
		callbackRecord = record
		event, err := events.New(ctx, events.Params{
			Type:          "test.webhook.manifest.recorded",
			SchemaVersion: 1,
			Producer:      events.Producer{Service: "zs-core", Instance: "test"},
			Actor:         events.Actor{Type: events.ActorSystem},
			Brand:         record.ID,
			Aggregate:     events.Aggregate{Type: "brand", ID: record.PublicID()},
			CorrelationID: "webhook-manifest-commit",
			Payload:       nil,
		})
		if err != nil {
			return err
		}
		return outbox.Write(ctx, tx, event)
	}}
	opts := integrationActor
	opts.CorrelationID = "webhook-manifest-commit"
	if err := recordManifest(ctx, deps, brandID, &after, Diff{ManifestDigestBefore: before.Digest(), ManifestDigest: after.Digest()}, opts); err != nil {
		t.Fatal(err)
	}
	if callbackRecord.ManifestDigest != after.Digest() || callbackRecord.Manifest == nil || callbackRecord.Manifest.Notifications == nil || len(callbackRecord.Manifest.Notifications.Webhooks) != 1 {
		t.Fatalf("callback record = %#v", callbackRecord)
	}
	stored, err := Get(ctx, db.Pool, brandID)
	if err != nil || stored.ManifestDigest != after.Digest() || stored.Manifest == nil || stored.Manifest.Notifications == nil || len(stored.Manifest.Notifications.Webhooks) != 1 {
		t.Fatalf("stored record = %#v, %v", stored, err)
	}
	assertManifestTransactionCounts(t, ctx, db, brandID, 1, 1)
}

func TestRecordManifestWebhookCallbackFailureRollsBackManifestAuditAndOutbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := postgrestest.New(t)
	before := brandManifest(t, "zs-hook-failure", "env://BRAND_ZS_HOOK_FAILURE__KEYCLOAK_STAFF_CLIENT_SECRET")
	brandID := insertBrand(t, db, before)
	after := *before
	after.Notifications = &Notifications{Webhooks: []Webhook{{
		URL: "https://receiver.example/hook", SecretRef: "env://BRAND_ZS_HOOK_FAILURE__WEBHOOK",
	}}}
	callbackFailure := errors.New("webhook reconciliation failed")
	deps := Deps{DB: db.Pool, AfterManifestRecorded: func(ctx context.Context, tx pgx.Tx, record Record, _ Options) error {
		event, err := events.New(ctx, events.Params{
			Type:          "test.webhook.manifest.failed",
			SchemaVersion: 1,
			Producer:      events.Producer{Service: "zs-core", Instance: "test"},
			Actor:         events.Actor{Type: events.ActorSystem},
			Brand:         record.ID,
			Aggregate:     events.Aggregate{Type: "brand", ID: idgen.Format(idgen.Brand, record.ID)},
			CorrelationID: "webhook-manifest-rollback",
			Payload:       nil,
		})
		if err != nil {
			return err
		}
		if err := outbox.Write(ctx, tx, event); err != nil {
			return err
		}
		return callbackFailure
	}}
	opts := integrationActor
	opts.CorrelationID = "webhook-manifest-rollback"
	if err := recordManifest(ctx, deps, brandID, &after, Diff{ManifestDigestBefore: before.Digest(), ManifestDigest: after.Digest()}, opts); !errors.Is(err, callbackFailure) {
		t.Fatalf("record manifest error = %v, want callback failure", err)
	}
	stored, err := Get(ctx, db.Pool, brandID)
	if err != nil || stored.ManifestDigest != before.Digest() || stored.Manifest == nil || stored.Manifest.Notifications != nil {
		t.Fatalf("rolled back record = %#v, %v", stored, err)
	}
	assertManifestTransactionCounts(t, ctx, db, brandID, 0, 0)
}

func assertManifestTransactionCounts(t *testing.T, ctx context.Context, db *postgrestest.DB, brandID idgen.UUID, wantAudit, wantOutbox int) {
	t.Helper()
	var auditCount, outboxCount int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE brand_id = $1 AND action = 'brand.manifest.recorded'`, brandID).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE brand_id = $1 AND event_type LIKE 'test.webhook.manifest.%'`, brandID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != wantAudit || outboxCount != wantOutbox {
		t.Fatalf("transaction rows: audit=%d outbox=%d, want audit=%d outbox=%d", auditCount, outboxCount, wantAudit, wantOutbox)
	}
}
