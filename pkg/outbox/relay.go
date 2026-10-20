package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

const (
	// DefaultBatchSize is the number of events read per relay pass.
	DefaultBatchSize = 100
	// DefaultMaxAttempts is the number of failed deliveries before an event
	// is quarantined for an operator.
	DefaultMaxAttempts = 10
)

// ErrDeliveryDeferred leaves an event unpublished without spending a retry,
// for example while a required consumer is intentionally offline. The next
// attempt still waits the retry backoff, so the event does not hold up the
// events behind it.
var ErrDeliveryDeferred = errors.New("outbox delivery deferred")

// ErrDeliveryTerminal quarantines an event on the first failure, for errors
// that a retry cannot fix.
var ErrDeliveryTerminal = errors.New("outbox delivery terminal")

// Claim is a batch of due events held by one relay until Until. Token
// identifies the claim; acknowledgements carry it, so a relay whose claim
// expired cannot acknowledge an event another relay now holds.
type Claim struct {
	Token  []byte
	Until  time.Time
	Events []events.Envelope
	// Quarantined lists rows that could not be decoded or validated. They
	// were quarantined while claiming and are not delivered.
	Quarantined []idgen.UUID
}

// Reader claims due events and acknowledges them. PostgresReader implements
// it.
type Reader interface {
	// Claim takes up to limit due events that no other relay holds.
	Claim(ctx context.Context, limit int) (Claim, error)
	// MarkPublished records the delivery of an event held by token.
	MarkPublished(ctx context.Context, token []byte, id idgen.UUID) error
	// RecordFailure schedules a retry of an event held by token and reports
	// whether it is now quarantined because it reached maxAttempts.
	RecordFailure(ctx context.Context, token []byte, id idgen.UUID, maxAttempts int) (quarantined bool, err error)
	// Defer schedules the next attempt of an event held by token without
	// counting an attempt.
	Defer(ctx context.Context, token []byte, id idgen.UUID) error
}

// Handler delivers one event. It must be idempotent.
type Handler interface {
	Handle(ctx context.Context, envelope events.Envelope) error
}

// HandlerFunc adapts a function to a Handler.
type HandlerFunc func(context.Context, events.Envelope) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, envelope events.Envelope) error {
	return f(ctx, envelope)
}

// Relay drains the outbox into a handler.
type Relay struct {
	name        string
	reader      Reader
	handler     Handler
	batch       int
	maxAttempts int
	log         *slog.Logger
}

// NewRelay returns a relay. A batch or maxAttempts of zero uses the default.
func NewRelay(name string, reader Reader, handler Handler, batch, maxAttempts int, log *slog.Logger) *Relay {
	if batch <= 0 {
		batch = DefaultBatchSize
	}
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Relay{name: name, reader: reader, handler: handler, batch: batch, maxAttempts: maxAttempts, log: log}
}

// RunOnce claims one batch, delivers it and returns the number of events
// published. Each event is acknowledged as soon as its handler returns.
// Handler failures are joined into the returned error. Delivery stops when
// the claim expires; the undelivered events become due again for any relay.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	claim, err := r.reader.Claim(ctx, r.batch)
	if err != nil {
		return 0, err
	}
	for _, id := range claim.Quarantined {
		r.log.Error("outbox event quarantined: the stored envelope cannot be decoded",
			"relay", r.name, "event_uuid", id.String())
	}
	if len(claim.Events) == 0 {
		return 0, nil
	}

	deliverCtx, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()

	published := 0
	var failures []error
	for i, envelope := range claim.Events {
		if deliverCtx.Err() != nil {
			if ctx.Err() == nil {
				failures = append(failures, fmt.Errorf("outbox claim expired before %d event(s) were delivered", len(claim.Events)-i))
			}
			break
		}
		id := envelope.StorageID()
		handleErr := r.handler.Handle(deliverCtx, envelope)
		switch {
		case handleErr == nil:
			if err := r.reader.MarkPublished(ctx, claim.Token, id); err != nil {
				failures = append(failures, fmt.Errorf("mark %s published: %w", envelope.ID, err))
				continue
			}
			published++
		case errors.Is(handleErr, ErrDeliveryDeferred):
			if err := r.reader.Defer(ctx, claim.Token, id); err != nil {
				failures = append(failures, fmt.Errorf("defer %s: %w", envelope.ID, err))
			}
		default:
			maxAttempts := r.maxAttempts
			if errors.Is(handleErr, ErrDeliveryTerminal) {
				maxAttempts = 1
			}
			quarantined, recordErr := r.reader.RecordFailure(ctx, claim.Token, id, maxAttempts)
			if recordErr != nil {
				failures = append(failures, fmt.Errorf("record failure for %s: %w", envelope.ID, recordErr))
			}
			r.log.Error("outbox dispatch failed",
				"relay", r.name, "event_id", envelope.ID, "event_type", envelope.Type,
				"quarantined", quarantined, "error", handleErr.Error())
			failures = append(failures, fmt.Errorf("dispatch %s: %w", envelope.ID, handleErr))
		}
	}
	return published, errors.Join(failures...)
}

// Run calls RunOnce every interval until ctx is done. A full batch is
// followed immediately by the next pass.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		count, err := r.RunOnce(ctx)
		if err != nil {
			r.log.Error("outbox relay pass failed", "relay", r.name, "error", err.Error())
		} else if count > 0 {
			r.log.Info("outbox relay published events", "relay", r.name, "count", count)
			if count == r.batch {
				if ctx.Err() != nil {
					return
				}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
