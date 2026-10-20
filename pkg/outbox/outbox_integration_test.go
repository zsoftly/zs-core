//go:build integration

package outbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/postgrestest"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/outbox"
)

func newEvent(t *testing.T, eventType string) events.Envelope {
	t.Helper()
	s := postgrestest.NewScope(t)
	e, err := events.New(context.Background(), events.Params{
		Type: eventType, SchemaVersion: 1,
		Producer:      events.Producer{Service: "zs-core", Instance: "test"},
		Actor:         events.Actor{Type: events.ActorUser, ID: "user-1"},
		Brand:         s.Brand,
		Tenant:        s.Tenant,
		Aggregate:     events.Aggregate{Type: "tenant", ID: s.TenantID()},
		CorrelationID: "corr-1",
		Traceparent:   "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Payload:       map[string]any{"n": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestWriteJoinsCallerTransaction(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	reader := outbox.NewPostgresReader(db.Pool)

	rolledBack := newEvent(t, "test.rolled_back")
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.Write(ctx, tx, rolledBack); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	committed := newEvent(t, "test.committed")
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, committed) }); err != nil {
		t.Fatal(err)
	}

	got, err := reader.Unpublished(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != committed.ID {
		t.Fatalf("unpublished = %+v", got)
	}
	e := got[0]
	if e.BrandID != committed.BrandID || e.TenantID != committed.TenantID || e.Traceparent != committed.Traceparent ||
		e.Actor != committed.Actor || e.Producer != committed.Producer || !e.OccurredAt.Equal(committed.OccurredAt) {
		t.Fatalf("envelope did not round trip: %+v vs %+v", e, committed)
	}
}

func TestWriteRejectsInvalidEnvelope(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	bad := newEvent(t, "test.bad")
	bad.Type = "Bad Type"
	err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error { return outbox.Write(ctx, tx, bad) })
	if err == nil {
		t.Fatal("invalid envelope written")
	}
	if err := outbox.Write(ctx, nil, newEvent(t, "test.no_tx")); err == nil {
		t.Fatal("write without a transaction accepted")
	}
}

func TestRelayPublishesRetriesAndQuarantines(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	ok := newEvent(t, "test.ok")
	failing := newEvent(t, "test.fail")
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if err := outbox.Write(ctx, tx, ok); err != nil {
			return err
		}
		return outbox.Write(ctx, tx, failing)
	}); err != nil {
		t.Fatal(err)
	}

	var delivered []string
	handler := outbox.HandlerFunc(func(_ context.Context, e events.Envelope) error {
		if e.Type == "test.fail" {
			return errors.New("endpoint returned 502 with body: secret upstream text")
		}
		delivered = append(delivered, e.ID)
		return nil
	})
	reader := outbox.NewPostgresReader(db.Pool)
	relay := outbox.NewRelay("test", reader, handler, 10, 2, nil)

	count, err := relay.RunOnce(ctx)
	if count != 1 || err == nil || len(delivered) != 1 || delivered[0] != ok.ID {
		t.Fatalf("first pass = %d, %v, %v", count, err, delivered)
	}

	var attempts int
	var nextAttempt time.Time
	var lastError string
	if err := db.Pool.QueryRow(ctx, `SELECT dispatch_attempts, next_attempt_at, last_dispatch_error FROM outbox WHERE id = $1`,
		failing.StorageID()).Scan(&attempts, &nextAttempt, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !nextAttempt.After(time.Now()) || lastError != "handler_failure" {
		t.Fatalf("after failure: attempts=%d next=%v error=%q", attempts, nextAttempt, lastError)
	}

	// Not due yet: the next pass sees nothing.
	if count, err := relay.RunOnce(ctx); count != 0 || err != nil {
		t.Fatalf("second pass = %d, %v", count, err)
	}

	if _, err := db.Pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE id = $1`, failing.StorageID()); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.RunOnce(ctx); err == nil {
		t.Fatal("expected the second failure to be reported")
	}
	var quarantined bool
	if err := db.Pool.QueryRow(ctx, `SELECT quarantined_at IS NOT NULL FROM outbox WHERE id = $1`, failing.StorageID()).Scan(&quarantined); err != nil || !quarantined {
		t.Fatalf("event not quarantined after max attempts: %v %v", quarantined, err)
	}
	if pending, err := reader.Unpublished(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("pending after quarantine = %v, %v", pending, err)
	}
}
