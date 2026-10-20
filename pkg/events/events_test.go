package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/zsoftly/zs-core/pkg/idgen"
)

func validParams() Params {
	return Params{
		Type:          "tenant.member.added",
		SchemaVersion: 1,
		Producer:      Producer{Service: "zs-core", Instance: "host-1"},
		Actor:         Actor{Type: ActorUser, ID: "usr-1"},
		Brand:         idgen.MustNewV7(),
		Tenant:        idgen.MustNewV7(),
		Aggregate:     Aggregate{Type: "membership", ID: "mem_1"},
		CorrelationID: "req-12345678",
		Payload:       map[string]any{"role": "member"},
	}
}

func TestNewAndRoundTrip(t *testing.T) {
	p := validParams()
	e, err := New(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.ID, "evt_") || e.EnvelopeVersion != EnvelopeVersion {
		t.Fatalf("envelope = %+v", e)
	}
	if e.Brand() != p.Brand || e.Tenant() != p.Tenant || e.StorageID().IsNil() {
		t.Fatal("scope accessors do not round trip")
	}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"envelope_version":1`, `"brand_id":"brd_`, `"tenant_id":"tnt_`, `"producer":{`, `"actor":{"type":"user"`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("encoded envelope lacks %s: %s", field, raw)
		}
	}
	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ID != e.ID || !decoded.OccurredAt.Equal(e.OccurredAt) || string(decoded.Payload) != string(e.Payload) {
		t.Fatalf("decoded = %+v", decoded)
	}
}

func TestBrandLevelEventOmitsTenant(t *testing.T) {
	p := validParams()
	p.Tenant = idgen.Nil
	e, err := New(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "tenant_id") || !e.Tenant().IsNil() {
		t.Fatalf("brand-level event carries a tenant: %s", raw)
	}
}

func TestValidationFailures(t *testing.T) {
	cases := map[string]func(*Params){
		"bad type":          func(p *Params) { p.Type = "Tenant Created" },
		"single segment":    func(p *Params) { p.Type = "tenant" },
		"schema version":    func(p *Params) { p.SchemaVersion = 0 },
		"missing producer":  func(p *Params) { p.Producer = Producer{} },
		"bad actor":         func(p *Params) { p.Actor = Actor{Type: "robot", ID: "x"} },
		"actor without id":  func(p *Params) { p.Actor = Actor{Type: ActorAPIKey} },
		"missing brand":     func(p *Params) { p.Brand = idgen.Nil },
		"missing aggregate": func(p *Params) { p.Aggregate = Aggregate{} },
		"no correlation":    func(p *Params) { p.CorrelationID = "" },
		"bad traceparent":   func(p *Params) { p.Traceparent = "00-zz-yy-01" },
		"payload too large": func(p *Params) { p.Payload = strings.Repeat("x", MaxPayloadBytes) },
		"payload not json":  func(p *Params) { p.Payload = func() {} },
	}
	for name, mutate := range cases {
		p := validParams()
		mutate(&p)
		if _, err := New(context.Background(), p); err == nil {
			t.Errorf("%s: New succeeded", name)
		}
	}
	p := validParams()
	p.Actor = Actor{Type: ActorSystem}
	if _, err := New(context.Background(), p); err != nil {
		t.Fatalf("system actor without id rejected: %v", err)
	}
}

func TestUnmarshalRejectsInvalid(t *testing.T) {
	var e Envelope
	if err := json.Unmarshal([]byte(`{"envelope_version":2}`), &e); err == nil {
		t.Fatal("unsupported envelope version accepted")
	}
}

func TestTraceparentFromContext(t *testing.T) {
	if got := TraceparentFromContext(context.Background()); got != "" {
		t.Fatalf("empty context traceparent = %q", got)
	}
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if got := TraceparentFromContext(ctx); got != want {
		t.Fatalf("traceparent = %q, want %q", got, want)
	}
	p := validParams()
	p.OccurredAt = time.Date(2026, 10, 20, 12, 0, 0, 123456789, time.UTC)
	e, err := New(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Traceparent != want {
		t.Fatalf("envelope traceparent = %q", e.Traceparent)
	}
	if e.OccurredAt.Nanosecond()%1000 != 0 {
		t.Fatal("occurred_at not truncated to microseconds")
	}
	if !ValidTraceparent(want) || ValidTraceparent("00-00000000000000000000000000000000-00f067aa0ba902b7-01") {
		t.Fatal("ValidTraceparent mismatch")
	}
}
