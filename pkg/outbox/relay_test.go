package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

type fakeReader struct {
	pending     []events.Envelope
	until       time.Time
	published   []idgen.UUID
	deferred    []idgen.UUID
	failures    map[idgen.UUID]int
	maxAttempts map[idgen.UUID]int
}

func newFakeReader(pending ...events.Envelope) *fakeReader {
	return &fakeReader{pending: pending, failures: map[idgen.UUID]int{}, maxAttempts: map[idgen.UUID]int{}}
}

func (f *fakeReader) Claim(context.Context, int) (Claim, error) {
	until := f.until
	if until.IsZero() {
		until = time.Now().Add(time.Minute)
	}
	return Claim{Token: []byte("token"), Until: until, Events: f.pending}, nil
}

func (f *fakeReader) MarkPublished(_ context.Context, _ []byte, id idgen.UUID) error {
	f.published = append(f.published, id)
	return nil
}

func (f *fakeReader) RecordFailure(_ context.Context, _ []byte, id idgen.UUID, maxAttempts int) (bool, error) {
	f.failures[id]++
	f.maxAttempts[id] = maxAttempts
	return f.failures[id] >= maxAttempts, nil
}

func (f *fakeReader) Defer(_ context.Context, _ []byte, id idgen.UUID) error {
	f.deferred = append(f.deferred, id)
	return nil
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
	reader := newFakeReader(ok, failing, terminal, deferred)
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
	if len(reader.deferred) != 1 || reader.deferred[0] != deferred.StorageID() {
		t.Fatalf("deferred event was not rescheduled: %v", reader.deferred)
	}
}

func TestRelayStopsWhenTheClaimExpires(t *testing.T) {
	first := envelope(t, "test.first")
	second := envelope(t, "test.second")
	reader := newFakeReader(first, second)
	reader.until = time.Now().Add(50 * time.Millisecond)
	var handled []string
	handler := HandlerFunc(func(ctx context.Context, e events.Envelope) error {
		handled = append(handled, e.Type)
		<-ctx.Done()
		return nil
	})
	count, err := NewRelay("test", reader, handler, 10, 3, nil).RunOnce(context.Background())
	if count != 1 || err == nil || len(handled) != 1 {
		t.Fatalf("RunOnce = %d, %v, handled %v", count, err, handled)
	}
}

func TestRelayEmptyOutbox(t *testing.T) {
	reader := newFakeReader()
	relay := NewRelay("test", reader, HandlerFunc(func(context.Context, events.Envelope) error {
		t.Fatal("handler called")
		return nil
	}), 10, 0, nil)
	if count, err := relay.RunOnce(context.Background()); count != 0 || err != nil {
		t.Fatalf("RunOnce = %d, %v", count, err)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	reader := newFakeReader()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	NewRelay("test", reader, HandlerFunc(func(context.Context, events.Envelope) error { return nil }), 1, 1, nil).Run(ctx, 1)
}
