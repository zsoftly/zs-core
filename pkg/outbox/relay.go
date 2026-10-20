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
// for example while a required consumer is intentionally offline.
var ErrDeliveryDeferred = errors.New("outbox delivery deferred")

// ErrDeliveryTerminal quarantines an event on the first failure, for errors
// that a retry cannot fix.
var ErrDeliveryTerminal = errors.New("outbox delivery terminal")

// Reader reads unpublished events and acknowledges delivered ones.
type Reader interface {
	Unpublished(ctx context.Context, limit int) ([]events.Envelope, error)
	MarkPublished(ctx context.Context, ids []idgen.UUID) error
}

// FailureRecorder schedules retries and quarantines events. PostgresReader
// implements it. A Reader without it retries on the next pass.
type FailureRecorder interface {
	RecordFailure(ctx context.Context, id idgen.UUID, maxAttempts int) (quarantined bool, err error)
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

// RunOnce delivers one batch and returns the number of events published.
// Handler failures are joined into the returned error after the successful
// events are marked published.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	envelopes, err := r.reader.Unpublished(ctx, r.batch)
	if err != nil {
		return 0, err
	}
	if len(envelopes) == 0 {
		return 0, nil
	}

	published := make([]idgen.UUID, 0, len(envelopes))
	var failures []error
	for _, envelope := range envelopes {
		handleErr := r.handler.Handle(ctx, envelope)
		if handleErr == nil {
			published = append(published, envelope.StorageID())
			continue
		}
		if errors.Is(handleErr, ErrDeliveryDeferred) {
			continue
		}
		maxAttempts := r.maxAttempts
		if errors.Is(handleErr, ErrDeliveryTerminal) {
			maxAttempts = 1
		}
		quarantined := false
		if recorder, ok := r.reader.(FailureRecorder); ok {
			var recordErr error
			quarantined, recordErr = recorder.RecordFailure(ctx, envelope.StorageID(), maxAttempts)
			if recordErr != nil {
				failures = append(failures, fmt.Errorf("record failure for %s: %w", envelope.ID, recordErr))
			}
		}
		r.log.Error("outbox dispatch failed",
			"relay", r.name, "event_id", envelope.ID, "event_type", envelope.Type,
			"quarantined", quarantined, "error", handleErr.Error())
		failures = append(failures, fmt.Errorf("dispatch %s: %w", envelope.ID, handleErr))
	}

	if len(published) > 0 {
		if err := r.reader.MarkPublished(ctx, published); err != nil {
			return 0, err
		}
	}
	return len(published), errors.Join(failures...)
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
