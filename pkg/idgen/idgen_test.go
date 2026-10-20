package idgen

import (
	"strings"
	"testing"
	"time"
)

func TestNewV7LayoutAndOrder(t *testing.T) {
	previous := Nil
	for i := 0; i < 10000; i++ {
		u, err := NewV7()
		if err != nil {
			t.Fatal(err)
		}
		if u.Version() != 7 {
			t.Fatalf("version = %d, want 7", u.Version())
		}
		if u[8]&0xc0 != 0x80 {
			t.Fatalf("variant bits = %08b, want 10xxxxxx", u[8])
		}
		if i > 0 && u.String() <= previous.String() {
			t.Fatalf("ids not strictly increasing: %s then %s", previous, u)
		}
		previous = u
	}
}

func TestNewV7CounterOverflowAdvancesTime(t *testing.T) {
	fixed := time.UnixMilli(1_800_000_000_000)
	clock = func() time.Time { return fixed }
	t.Cleanup(func() { clock = time.Now })

	var last UUID
	for i := 0; i < 5000; i++ {
		u := MustNewV7()
		if i > 0 && u.String() <= last.String() {
			t.Fatalf("ids not strictly increasing at %d", i)
		}
		last = u
	}
	if !last.Time().After(fixed) {
		t.Fatalf("expected timestamp to advance past the frozen clock after counter overflow")
	}
}

func TestUUIDTimeAndStringRoundTrip(t *testing.T) {
	before := time.Now().Add(-time.Millisecond)
	u := MustNewV7()
	if u.Time().Before(before.Truncate(time.Millisecond)) {
		t.Fatalf("uuid time %v is before %v", u.Time(), before)
	}
	parsed, err := ParseUUID(u.String())
	if err != nil || parsed != u {
		t.Fatalf("round trip failed: %v %s %s", err, parsed, u)
	}
	for _, bad := range []string{"", "not-a-uuid", strings.Repeat("g", 36), "0190a6c4-1b2c-7d3e-8f40-12345678901"} {
		if _, err := ParseUUID(bad); err == nil {
			t.Errorf("ParseUUID(%q) succeeded", bad)
		}
	}
}

func TestUUIDScan(t *testing.T) {
	u := MustNewV7()
	var got UUID
	if err := got.Scan(u.String()); err != nil || got != u {
		t.Fatalf("scan string: %v", err)
	}
	if err := got.Scan(u[:]); err != nil || got != u {
		t.Fatalf("scan bytes: %v", err)
	}
	if err := got.Scan(42); err == nil {
		t.Fatal("scan int succeeded")
	}
}

func TestPublicIDRoundTripForEveryPrefix(t *testing.T) {
	for _, prefix := range Prefixes() {
		id, err := New(prefix)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, string(prefix)+"_") || len(id) != len(prefix)+1+encodedLength {
			t.Fatalf("unexpected id shape %q", id)
		}
		gotPrefix, u, err := Parse(id)
		if err != nil || gotPrefix != prefix || u.Version() != 7 {
			t.Fatalf("Parse(%q) = %q, %v, %v", id, gotPrefix, u, err)
		}
		if Format(prefix, u) != id {
			t.Fatalf("format mismatch for %q", id)
		}
		if err := Validate(prefix, id); err != nil {
			t.Fatalf("Validate(%q): %v", id, err)
		}
	}
}

func TestPublicIDEncodingIsSortable(t *testing.T) {
	a, _ := New(Audit)
	b, _ := New(Audit)
	if a >= b {
		t.Fatalf("public ids are not sortable: %s >= %s", a, b)
	}
}

func TestParseRejectsMalformedAndWrongPrefix(t *testing.T) {
	tenant, _ := New(Tenant)
	suffix := strings.TrimPrefix(tenant, "tnt_")
	cases := map[string]string{
		"empty":              "",
		"no separator":       "tnt" + suffix,
		"unknown prefix":     "abc_" + suffix,
		"uppercase prefix":   "TNT_" + suffix,
		"short suffix":       "tnt_" + suffix[:25],
		"long suffix":        "tnt_" + suffix + "0",
		"excluded character": "tnt_" + suffix[:25] + "u",
		"overflowing value":  "tnt_8" + suffix[1:],
		"nil uuid":           "tnt_" + strings.Repeat("0", encodedLength),
	}
	for name, value := range cases {
		if _, _, err := Parse(value); err == nil {
			t.Errorf("%s: Parse(%q) succeeded", name, value)
		}
	}
	if err := Validate(Brand, tenant); err == nil {
		t.Fatal("Validate accepted a tenant id as a brand id")
	}
	if _, err := New(Prefix("nope")); err == nil {
		t.Fatal("New accepted an unregistered prefix")
	}
}
