//go:build integration

package outbox_test

import (
	"context"
	"errors"
	"sync"
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

	claim, err := reader.Claim(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := claim.Events
	if len(got) != 1 || got[0].ID != committed.ID {
		t.Fatalf("claimed = %+v", got)
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
	if claim, err := reader.Claim(ctx, 10); err != nil || len(claim.Events) != 0 {
		t.Fatalf("pending after quarantine = %v, %v", claim.Events, err)
	}
}

func writeEvents(t *testing.T, db *postgrestest.DB, envelopes ...events.Envelope) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		for _, e := range envelopes {
			if err := outbox.Write(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTwoRelaysDeliverEachEventOnce(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	const total = 120
	all := make([]events.Envelope, total)
	for i := range all {
		all[i] = newEvent(t, "test.shared")
	}
	writeEvents(t, db, all...)

	var mu sync.Mutex
	deliveries := map[string]int{}
	handler := outbox.HandlerFunc(func(_ context.Context, e events.Envelope) error {
		mu.Lock()
		deliveries[e.ID]++
		mu.Unlock()
		time.Sleep(time.Millisecond)
		return nil
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"relay-a", "relay-b"} {
		relay := outbox.NewRelay(name, outbox.NewPostgresReader(db.Pool), handler, 7, 3, nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				count, err := relay.RunOnce(ctx)
				if err != nil {
					errs <- err
					return
				}
				if count == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(deliveries) != total {
		t.Fatalf("delivered %d distinct events, want %d", len(deliveries), total)
	}
	for id, n := range deliveries {
		if n != 1 {
			t.Fatalf("event %s delivered %d times", id, n)
		}
	}
	var unpublished int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&unpublished); err != nil || unpublished != 0 {
		t.Fatalf("unpublished = %d, %v", unpublished, err)
	}
}

func TestExpiredClaimIsTakenOverAndTheOldClaimCannotAcknowledge(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	e := newEvent(t, "test.claimed")
	writeEvents(t, db, e)
	readerA := outbox.NewPostgresReader(db.Pool)
	readerB := outbox.NewPostgresReader(db.Pool)

	claimA, err := readerA.Claim(ctx, 10)
	if err != nil || len(claimA.Events) != 1 {
		t.Fatalf("claim A = %+v, %v", claimA, err)
	}
	if claimB, err := readerB.Claim(ctx, 10); err != nil || len(claimB.Events) != 0 {
		t.Fatalf("B claimed an event A holds: %+v, %v", claimB.Events, err)
	}

	// A crashes; its claim runs out.
	if _, err := db.Pool.Exec(ctx, `UPDATE outbox SET claimed_until = now() - interval '1 second' WHERE id = $1`, e.StorageID()); err != nil {
		t.Fatal(err)
	}
	claimB, err := readerB.Claim(ctx, 10)
	if err != nil || len(claimB.Events) != 1 {
		t.Fatalf("B did not take over the expired claim: %+v, %v", claimB.Events, err)
	}
	if _, err := readerA.RecordFailure(ctx, claimA.Token, e.StorageID(), 3); !errors.Is(err, outbox.ErrClaimLost) {
		t.Fatalf("stale failure = %v, want ErrClaimLost", err)
	}
	if err := readerA.MarkPublished(ctx, claimA.Token, e.StorageID()); !errors.Is(err, outbox.ErrClaimLost) {
		t.Fatalf("stale acknowledgement = %v, want ErrClaimLost", err)
	}
	var attempts int
	if err := db.Pool.QueryRow(ctx, `SELECT dispatch_attempts FROM outbox WHERE id = $1`, e.StorageID()).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts = %d, %v: a stale failure was counted", attempts, err)
	}
	if err := readerB.MarkPublished(ctx, claimB.Token, e.StorageID()); err != nil {
		t.Fatal(err)
	}
}

func TestUndecodableRowIsQuarantinedAndTheRestProceed(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	before := newEvent(t, "test.before")
	after := newEvent(t, "test.after")
	writeEvents(t, db, before)
	broken := newEvent(t, "test.broken")
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO outbox (id, event_type, brand_id, tenant_id, occurred_at, envelope) VALUES ($1, 'test.broken', $2, $3, now(), '{"envelope_version": 99}')`,
		broken.StorageID(), broken.Brand(), broken.Tenant()); err != nil {
		t.Fatal(err)
	}
	writeEvents(t, db, after)

	var delivered []string
	relay := outbox.NewRelay("test", outbox.NewPostgresReader(db.Pool), outbox.HandlerFunc(func(_ context.Context, e events.Envelope) error {
		delivered = append(delivered, e.Type)
		return nil
	}), 10, 3, nil)
	count, err := relay.RunOnce(ctx)
	if err != nil || count != 2 || len(delivered) != 2 {
		t.Fatalf("RunOnce = %d, %v, delivered %v", count, err, delivered)
	}
	var lastError string
	var quarantined bool
	if err := db.Pool.QueryRow(ctx, `SELECT last_dispatch_error, quarantined_at IS NOT NULL FROM outbox WHERE id = $1`,
		broken.StorageID()).Scan(&lastError, &quarantined); err != nil {
		t.Fatal(err)
	}
	if lastError != "decode_failure" || !quarantined {
		t.Fatalf("broken row: error %q quarantined %v", lastError, quarantined)
	}
	if count, err := relay.RunOnce(ctx); count != 0 || err != nil {
		t.Fatalf("second pass = %d, %v", count, err)
	}
}

func TestDeferredEventWaitsWithoutBlockingTheNext(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	deferred := newEvent(t, "test.deferred")
	next := newEvent(t, "test.next")
	writeEvents(t, db, deferred)
	writeEvents(t, db, next)

	var delivered []string
	relay := outbox.NewRelay("test", outbox.NewPostgresReader(db.Pool), outbox.HandlerFunc(func(_ context.Context, e events.Envelope) error {
		if e.Type == "test.deferred" {
			return outbox.ErrDeliveryDeferred
		}
		delivered = append(delivered, e.Type)
		return nil
	}), 1, 3, nil)

	if count, err := relay.RunOnce(ctx); count != 0 || err != nil {
		t.Fatalf("first pass = %d, %v", count, err)
	}
	var attempts int
	var nextAttempt time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT dispatch_attempts, next_attempt_at FROM outbox WHERE id = $1`,
		deferred.StorageID()).Scan(&attempts, &nextAttempt); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || !nextAttempt.After(time.Now()) {
		t.Fatalf("deferred: attempts=%d next=%v", attempts, nextAttempt)
	}
	if count, err := relay.RunOnce(ctx); count != 1 || err != nil || len(delivered) != 1 || delivered[0] != "test.next" {
		t.Fatalf("second pass = %d, %v, %v", count, err, delivered)
	}
}

// A reader with event types claims only those types and leaves the others
// due for another reader.
func TestReaderWithEventTypesClaimsOnlyThoseTypes(t *testing.T) {
	db := postgrestest.New(t)
	ctx := context.Background()
	mine, other := newEvent(t, "tenant.mirror"), newEvent(t, "test.other")
	writeEvents(t, db, other, mine)
	claim, err := outbox.NewPostgresReader(db.Pool).WithEventTypes("tenant.mirror").Claim(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.Events) != 1 || claim.Events[0].ID != mine.ID {
		t.Fatalf("filtered claim = %+v", claim.Events)
	}
	claim, err = outbox.NewPostgresReader(db.Pool).Claim(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.Events) != 1 || claim.Events[0].ID != other.ID {
		t.Fatalf("unfiltered claim after the filtered one = %+v", claim.Events)
	}
}
