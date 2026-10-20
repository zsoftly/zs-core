package audit

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

func validEntry() Entry {
	return Entry{
		Actor:         events.Actor{Type: events.ActorUser, ID: "user-1"},
		Action:        "tenant.member.added",
		Reason:        "Invited by owner",
		Outcome:       OutcomeSuccess,
		CorrelationID: "req-1",
	}
}

func TestEntryValidate(t *testing.T) {
	if err := validEntry().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Entry){
		"actor type":     func(e *Entry) { e.Actor.Type = "robot" },
		"actor id":       func(e *Entry) { e.Actor.ID = "" },
		"long actor id":  func(e *Entry) { e.Actor.ID = strings.Repeat("a", MaxActorIDLength+1) },
		"action":         func(e *Entry) { e.Action = "Delete Everything" },
		"reason":         func(e *Entry) { e.Reason = strings.Repeat("r", MaxReasonLength+1) },
		"outcome":        func(e *Entry) { e.Outcome = "maybe" },
		"correlation":    func(e *Entry) { e.CorrelationID = "" },
		"long correlate": func(e *Entry) { e.CorrelationID = strings.Repeat("c", MaxCorrelationIDLength+1) },
	}
	for name, mutate := range cases {
		e := validEntry()
		mutate(&e)
		if err := e.Validate(); !zserrors.IsCode(err, zserrors.CodeValidation) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	system := validEntry()
	system.Actor = events.Actor{Type: events.ActorSystem}
	if err := system.Validate(); err != nil {
		t.Fatalf("system actor rejected: %v", err)
	}
}

func TestWriteRequiresTransaction(t *testing.T) {
	s, _ := scope.New(idgen.MustNewV7(), idgen.MustNewV7())
	if _, err := Write(context.Background(), nil, s, validEntry()); err == nil {
		t.Fatal("Write accepted a nil transaction")
	}
}

func TestQueryRefusesIncompleteScopeBeforeDatabase(t *testing.T) {
	_, err := Query(context.Background(), nil, scope.Scope{Brand: idgen.MustNewV7()}, Filter{})
	if !zserrors.IsCode(err, zserrors.CodeScopeRequired) {
		t.Fatalf("err = %v, want scope_required", err)
	}
}

func TestQueryValidatesFilter(t *testing.T) {
	s, _ := scope.New(idgen.MustNewV7(), idgen.MustNewV7())
	now := time.Now()
	cases := map[Filter]zserrors.Code{
		{Limit: -1}:                                        zserrors.CodeInvalidLimit,
		{Limit: MaxLimit + 1}:                              zserrors.CodeInvalidLimit,
		{Action: "Bad Action"}:                             zserrors.CodeValidation,
		{OccurredAfter: now, OccurredBefore: now}:          zserrors.CodeValidation,
		{Cursor: "not-a-cursor"}:                           zserrors.CodeInvalidCursor,
		{ActorID: strings.Repeat("a", MaxActorIDLength+1)}: zserrors.CodeValidation,
	}
	for f, want := range cases {
		if _, err := Query(context.Background(), nil, s, f); !zserrors.IsCode(err, want) {
			t.Errorf("filter %+v: err = %v, want %s", f, err, want)
		}
	}
}

func TestCursorRoundTripAndTamper(t *testing.T) {
	c := cursor{occurredAt: time.Now().UTC().Truncate(time.Microsecond), id: idgen.MustNewV7()}
	decoded, err := decodeCursor(encodeCursor(c))
	if err != nil || !decoded.occurredAt.Equal(c.occurredAt) || decoded.id != c.id {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
	bad := []string{
		"!!!",
		base64.RawURLEncoding.EncodeToString([]byte("v2.1.0190a6c4-1b2c-7d3e-8f40-123456789012")),
		base64.RawURLEncoding.EncodeToString([]byte("v1.x.0190a6c4-1b2c-7d3e-8f40-123456789012")),
		base64.RawURLEncoding.EncodeToString([]byte("v1.-5.0190a6c4-1b2c-7d3e-8f40-123456789012")),
		base64.RawURLEncoding.EncodeToString([]byte("v1.1.not-a-uuid")),
		strings.Repeat("a", 200),
	}
	for _, value := range bad {
		if _, err := decodeCursor(value); !zserrors.IsCode(err, zserrors.CodeInvalidCursor) {
			t.Errorf("decodeCursor(%q) = %v", value, err)
		}
	}
}
