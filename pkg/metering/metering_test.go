package metering

import (
	"testing"
	"time"

	"github.com/zsoftly/zs-core/pkg/brand"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

func TestTrackingIDIsADigestOfTheRollup(t *testing.T) {
	sc := scope.Scope{Brand: idgen.MustNewV7(), Tenant: idgen.MustNewV7()}
	a, b := idgen.MustNewV7(), idgen.MustNewV7()
	day := time.Date(2026, 10, 20, 15, 4, 5, 0, time.UTC)
	id := TrackingID(sc, "api-call", "", day, []idgen.UUID{a, b})
	if len(id) != len("zs-usg-")+40 || id[:7] != "zs-usg-" {
		t.Fatalf("tracking ID = %s", id)
	}
	// The order of the events and the time of day do not matter.
	if again := TrackingID(sc, "api-call", "", day.Truncate(24*time.Hour), []idgen.UUID{b, a}); again != id {
		t.Fatalf("same rollup, other tracking ID: %s %s", again, id)
	}
	for name, other := range map[string]string{
		"another event":        TrackingID(sc, "api-call", "", day, []idgen.UUID{a}),
		"another unit":         TrackingID(sc, "token", "", day, []idgen.UUID{a, b}),
		"another subscription": TrackingID(sc, "api-call", "00000000-0000-0000-0000-000000000001", day, []idgen.UUID{a, b}),
		"another day":          TrackingID(sc, "api-call", "", day.AddDate(0, 0, 1), []idgen.UUID{a, b}),
		"another tenant":       TrackingID(scope.Scope{Brand: sc.Brand, Tenant: idgen.MustNewV7()}, "api-call", "", day, []idgen.UUID{a, b}),
	} {
		if other == id {
			t.Errorf("%s gives the same tracking ID", name)
		}
	}
}

func TestFingerprintAndDimensions(t *testing.T) {
	at := time.Date(2026, 10, 20, 1, 2, 3, 4000, time.UTC)
	ev := Event{Meter: "api-calls", EventID: "e", QuantityMicro: 5, OccurredAt: at, Dimensions: map[string]string{"b": "2", "a": "1"}}
	same := ev
	same.Dimensions = map[string]string{"a": "1", "b": "2"}
	if fingerprint(ev) != fingerprint(same) {
		t.Fatal("dimension order changes the fingerprint")
	}
	for name, change := range map[string]func(*Event){
		"quantity":   func(e *Event) { e.QuantityMicro = 6 },
		"time":       func(e *Event) { e.OccurredAt = at.Add(time.Microsecond) },
		"dimensions": func(e *Event) { e.Dimensions = map[string]string{"a": "1"} },
		"resource":   func(e *Event) { e.ResourceRef = "vm-1" },
	} {
		other := ev
		change(&other)
		if fingerprint(other) == fingerprint(ev) {
			t.Errorf("%s does not change the fingerprint", name)
		}
	}
	dims, key := canonicalDimensions(map[string]string{"z": "\"q\"", "a": "x"})
	if string(dims) != `{"a":"x","z":"\"q\""}` || len(key) != 64 {
		t.Fatalf("canonical = %s %s", dims, key)
	}
}

func TestNormalize(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	r := &recording{now: now, backfill: 7 * 24 * time.Hour, ahead: 5 * time.Minute}
	m := brand.MeterRecord{Meter: brand.Meter{Slug: "api-calls", Dimensions: []string{"region"}}}
	ev, err := r.normalize(Event{Meter: "api-calls", EventID: "e", QuantityMicro: 1, OccurredAt: now.Add(-time.Hour).Add(1500 * time.Nanosecond)}, m, true)
	if err != nil || ev.OccurredAt.Nanosecond()%1000 != 0 || ev.Dimensions == nil {
		t.Fatalf("normalize = %+v %v", ev, err)
	}
	if ev, err := r.normalize(Event{Meter: "api-calls", EventID: "e", QuantityMicro: 1}, m, true); err != nil || !ev.OccurredAt.Equal(now) {
		t.Fatalf("default time = %+v %v", ev, err)
	}
	// A settlement keeps its date outside the window.
	if _, err := r.normalize(Event{Meter: "api-calls", EventID: "e", QuantityMicro: 1, OccurredAt: now.AddDate(0, 0, -30)}, m, false); err != nil {
		t.Fatalf("settlement outside the window: %v", err)
	}
	if _, err := r.normalize(Event{Meter: "api-calls", EventID: "e", QuantityMicro: MaxQuantityMicro + 1}, m, true); err == nil {
		t.Fatal("quantity past the cap accepted")
	}
	if _, err := r.normalize(Event{Meter: "api-calls", EventID: "e", QuantityMicro: 1, ResourceRef: "a\nb"}, m, true); err == nil {
		t.Fatal("resource ref with a newline accepted")
	}
	if safeKey("Bad Key") != "key" || safeKey("region") != "region" {
		t.Fatal("safeKey")
	}
}

func TestCursor(t *testing.T) {
	at := time.Date(2026, 10, 20, 1, 2, 3, 456789000, time.UTC)
	id := idgen.MustNewV7()
	gotAt, gotID, err := decodeCursor(encodeCursor(at, id))
	if err != nil || !gotAt.Equal(at) || gotID != id {
		t.Fatalf("cursor = %v %v %v", gotAt, gotID, err)
	}
	for _, bad := range []string{"%%", "dTF8eHx5", "eDF8MjAyNnwx"} {
		if _, _, err := decodeCursor(bad); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}
