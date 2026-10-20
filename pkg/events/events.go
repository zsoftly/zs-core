// Package events defines the versioned envelope for ZSoftly Core domain
// events.
//
// A package writes an Envelope to the transactional outbox (package outbox)
// in the same transaction as the state change that produced it, so the event
// and the change commit together. A relay later delivers the envelope. The
// payload is owned by the producing module and carried here as opaque JSON.
//
// Every envelope names its brand. Tenant-owned events also name the tenant;
// brand-level events, such as a change to brand configuration, leave it empty.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/zsoftly/zs-core/pkg/idgen"
)

const (
	// EnvelopeVersion is the version of the envelope shape itself. A change
	// that breaks consumers of the envelope increments it.
	EnvelopeVersion = 1
	// MaxPayloadBytes caps the encoded payload.
	MaxPayloadBytes = 64 * 1024
)

// ActorType classifies who caused an event.
type ActorType string

const (
	ActorUser     ActorType = "user"
	ActorAPIKey   ActorType = "api_key"
	ActorStaff    ActorType = "staff"
	ActorService  ActorType = "service"
	ActorProvider ActorType = "provider"
	ActorSystem   ActorType = "system"
)

// Valid reports whether t is a known actor type.
func (t ActorType) Valid() bool {
	switch t {
	case ActorUser, ActorAPIKey, ActorStaff, ActorService, ActorProvider, ActorSystem:
		return true
	}
	return false
}

// RequiresID reports whether an actor of this type must carry an ID. Only the
// system actor may be anonymous.
func (t ActorType) RequiresID() bool { return t != ActorSystem }

// Actor is who caused an event.
type Actor struct {
	Type ActorType `json:"type"`
	ID   string    `json:"id,omitempty"`
}

// Producer is the service instance that wrote an event.
type Producer struct {
	Service  string `json:"service"`
	Instance string `json:"instance"`
}

// Aggregate is the object an event is about.
type Aggregate struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Envelope is the wire and storage form of a domain event.
type Envelope struct {
	EnvelopeVersion int             `json:"envelope_version"`
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	SchemaVersion   int             `json:"schema_version"`
	OccurredAt      time.Time       `json:"occurred_at"`
	Producer        Producer        `json:"producer"`
	Actor           Actor           `json:"actor"`
	BrandID         string          `json:"brand_id"`
	TenantID        string          `json:"tenant_id,omitempty"`
	Aggregate       Aggregate       `json:"aggregate"`
	CorrelationID   string          `json:"correlation_id"`
	CausationID     string          `json:"causation_id,omitempty"`
	Traceparent     string          `json:"traceparent,omitempty"`
	Payload         json.RawMessage `json:"payload"`
}

// Params are the inputs of New. Tenant is idgen.Nil for brand-level events.
// When Traceparent is empty, New takes it from the span in ctx.
type Params struct {
	Type          string
	SchemaVersion int
	OccurredAt    time.Time
	Producer      Producer
	Actor         Actor
	Brand         idgen.UUID
	Tenant        idgen.UUID
	Aggregate     Aggregate
	CorrelationID string
	CausationID   string
	Traceparent   string
	Payload       any
}

// New builds and validates an envelope with a fresh evt_ ID.
func New(ctx context.Context, p Params) (Envelope, error) {
	id, err := idgen.New(idgen.Event)
	if err != nil {
		return Envelope{}, err
	}
	payload, err := json.Marshal(p.Payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal event payload: %w", err)
	}
	occurredAt := p.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	traceparent := p.Traceparent
	if traceparent == "" {
		traceparent = TraceparentFromContext(ctx)
	}
	e := Envelope{
		EnvelopeVersion: EnvelopeVersion,
		ID:              id,
		Type:            p.Type,
		SchemaVersion:   p.SchemaVersion,
		OccurredAt:      occurredAt.UTC().Truncate(time.Microsecond),
		Producer:        p.Producer,
		Actor:           p.Actor,
		Aggregate:       p.Aggregate,
		CorrelationID:   p.CorrelationID,
		CausationID:     p.CausationID,
		Traceparent:     traceparent,
		Payload:         payload,
	}
	if !p.Brand.IsNil() {
		e.BrandID = idgen.Format(idgen.Brand, p.Brand)
	}
	if !p.Tenant.IsNil() {
		e.TenantID = idgen.Format(idgen.Tenant, p.Tenant)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// Brand returns the brand UUID of a validated envelope.
func (e Envelope) Brand() idgen.UUID {
	u, _ := idgen.ParseAs(idgen.Brand, e.BrandID)
	return u
}

// Tenant returns the tenant UUID, or idgen.Nil for a brand-level event.
func (e Envelope) Tenant() idgen.UUID {
	if e.TenantID == "" {
		return idgen.Nil
	}
	u, _ := idgen.ParseAs(idgen.Tenant, e.TenantID)
	return u
}

// StorageID returns the UUID behind the evt_ ID.
func (e Envelope) StorageID() idgen.UUID {
	u, _ := idgen.ParseAs(idgen.Event, e.ID)
	return u
}

// Validate checks every field of the envelope.
func (e Envelope) Validate() error {
	switch {
	case e.EnvelopeVersion != EnvelopeVersion:
		return fmt.Errorf("event envelope_version %d is not supported", e.EnvelopeVersion)
	case idgen.Validate(idgen.Event, e.ID) != nil:
		return fmt.Errorf("event id must be an evt_ public id")
	case !ValidType(e.Type):
		return fmt.Errorf("event type %q must be a dotted lowercase name such as tenant.created", e.Type)
	case e.SchemaVersion <= 0:
		return fmt.Errorf("event schema_version must be positive")
	case e.OccurredAt.IsZero():
		return fmt.Errorf("event occurred_at must be set")
	case !bounded(e.Producer.Service, 1, 100) || !bounded(e.Producer.Instance, 1, 160):
		return fmt.Errorf("event producer service and instance must be set")
	case !e.Actor.Type.Valid():
		return fmt.Errorf("event actor type is invalid")
	case e.Actor.Type.RequiresID() && !bounded(e.Actor.ID, 1, 255):
		return fmt.Errorf("event actor id is required for actor type %q", e.Actor.Type)
	case idgen.Validate(idgen.Brand, e.BrandID) != nil:
		return fmt.Errorf("event brand_id must be a brd_ public id")
	case e.TenantID != "" && idgen.Validate(idgen.Tenant, e.TenantID) != nil:
		return fmt.Errorf("event tenant_id must be a tnt_ public id")
	case !bounded(e.Aggregate.Type, 1, 80) || !bounded(e.Aggregate.ID, 1, 255):
		return fmt.Errorf("event aggregate type and id must be set")
	case !bounded(e.CorrelationID, 1, 128):
		return fmt.Errorf("event correlation_id must be 1 to 128 characters")
	case len(e.CausationID) > 128:
		return fmt.Errorf("event causation_id must be at most 128 characters")
	case e.Traceparent != "" && !ValidTraceparent(e.Traceparent):
		return fmt.Errorf("event traceparent is invalid")
	case len(e.Payload) == 0:
		return fmt.Errorf("event payload must be set, use null for no payload")
	case len(e.Payload) > MaxPayloadBytes:
		return fmt.Errorf("event payload exceeds %d bytes", MaxPayloadBytes)
	}
	return nil
}

// UnmarshalJSON decodes and validates an envelope.
func (e *Envelope) UnmarshalJSON(data []byte) error {
	type plain Envelope
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	envelope := Envelope(decoded)
	if err := envelope.Validate(); err != nil {
		return err
	}
	*e = envelope
	return nil
}

// TraceparentFromContext formats the span context in ctx as a W3C
// traceparent header value. It returns "" when ctx carries no valid span.
func TraceparentFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%02x", sc.TraceID(), sc.SpanID(), byte(sc.TraceFlags()))
}

// ValidType reports whether value is a dotted lowercase name with at least two
// segments, such as "tenant.member.added".
func ValidType(value string) bool {
	if len(value) > 120 || !strings.Contains(value, ".") {
		return false
	}
	for _, segment := range strings.Split(value, ".") {
		if segment == "" {
			return false
		}
		for _, r := range segment {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
				return false
			}
		}
	}
	return true
}

// ValidTraceparent reports whether value is a W3C traceparent of version 00.
func ValidTraceparent(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return false
	}
	if strings.Trim(parts[1], "0") == "" || strings.Trim(parts[2], "0") == "" {
		return false
	}
	for _, part := range parts {
		for _, r := range part {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return false
			}
		}
	}
	return true
}

func bounded(value string, minimum, maximum int) bool {
	length := len(strings.TrimSpace(value))
	return length >= minimum && length <= maximum && length == len(value)
}
