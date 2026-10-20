package outbox

import (
	"context"
	"errors"
	"testing"

	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

type fakeReader struct {
	pending     []events.Envelope
	published   []idgen.UUID
	failures    map[idgen.UUID]int
	maxAttempts map[idgen.UUID]int
}

func (f *fakeReader) Unpublished(context.Context, int) ([]events.Envelope, error) {
	return f.pending, nil
}

func (f *fakeReader) MarkPublished(_ context.Context, ids []idgen.UUID) error {
	f.published = append(f.published, ids...)
	return nil
}

func (f *fakeReader) RecordFailure(_ context.Context, id idgen.UUID, maxAttempts int) (bool, error) {
	f.failures[id]++
	f.maxAttempts[id] = maxAttempts
	return f.failures[id] >= maxAttempts, nil
}

func envelope(t *testing.T, eventType string) events.Envelope {
	t.Helper()
	e, err := events.New(context.Background(), events.Params{
		Type: eventType, SchemaVersion: 1,
		Producer: events.Producer{Service: "zs-core", Instance: "test"},
		Actor:    events.Actor{Type: events.ActorSystem},
		Brand:    idgen.MustNewV7(), Tenant: idgen.MustNewV7(),
		Aggregate:     events.Aggregate{Type: "test", ID: "1"},
		CorrelationID: "corr-1", Payload: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRelayPublishesSuccessesAndRecordsFailures(t *testing.T) {
	ok := envelope(t, "test.ok")
	failing := envelope(t, "test.fail")
	terminal := envelope(t, "test.terminal")
	deferred := envelope(t, "test.deferred")
	reader := &fakeReader{
		pending:     []events.Envelope{ok, failing, terminal, deferred},
		failures:    map[idgen.UUID]int{},
		maxAttempts: map[idgen.UUID]int{},
	}
	handler := HandlerFunc(func(_ context.Context, e events.Envelope) error {
		switch e.Type {
		case "test.fail":
			return errors.New("webhook endpoint returned 500")
		case "test.terminal":
			return ErrDeliveryTerminal
		case "test.deferred":
			return ErrDeliveryDeferred
		}
		return nil
	})
	relay := NewRelay("test", reader, handler, 0, 3, nil)
	count, err := relay.RunOnce(context.Background())
	if count != 1 || err == nil {
		t.Fatalf("RunOnce = %d, %v", count, err)
	}
	if len(reader.published) != 1 || reader.published[0] != ok.StorageID() {
		t.Fatalf("published = %v", reader.published)
	}
	if reader.maxAttempts[failing.StorageID()] != 3 {
		t.Fatalf("failing event max attempts = %d", reader.maxAttempts[failing.StorageID()])
	}
	if reader.maxAttempts[terminal.StorageID()] != 1 {
		t.Fatalf("terminal event max attempts = %d", reader.maxAttempts[terminal.StorageID()])
	}
	if _, recorded := reader.failures[deferred.StorageID()]; recorded {
		t.Fatal("deferred event spent a retry")
	}
}

func TestRelayEmptyOutbox(t *testing.T) {
	reader := &fakeReader{failures: map[idgen.UUID]int{}, maxAttempts: map[idgen.UUID]int{}}
	relay := NewRelay("test", reader, HandlerFunc(func(context.Context, events.Envelope) error {
		t.Fatal("handler called")
		return nil
	}), 10, 0, nil)
	if count, err := relay.RunOnce(context.Background()); count != 0 || err != nil {
		t.Fatalf("RunOnce = %d, %v", count, err)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	reader := &fakeReader{failures: map[idgen.UUID]int{}, maxAttempts: map[idgen.UUID]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	NewRelay("test", reader, HandlerFunc(func(context.Context, events.Envelope) error { return nil }), 1, 1, nil).Run(ctx, 1)
}
