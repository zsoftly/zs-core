package identity

import (
	"strings"
	"testing"
	"time"
)

func TestNeedsUpsert(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	p := Principal{Email: " person@example.test ", EmailVerified: true, Name: "Person"}
	stored := Identity{Email: "person@example.test", EmailVerified: true, DisplayName: "Person", LastSeenAt: now.Add(-4 * time.Minute)}
	if NeedsUpsert(stored, p, now) {
		t.Fatal("an unchanged identity seen four minutes ago needs a write")
	}
	cases := map[string]func(i *Identity, p *Principal){
		"email changed":    func(_ *Identity, p *Principal) { p.Email = "other@example.test" },
		"name changed":     func(_ *Identity, p *Principal) { p.Name = "Other" },
		"verified changed": func(i *Identity, _ *Principal) { i.EmailVerified = false },
		"seen 5 min ago":   func(i *Identity, _ *Principal) { i.LastSeenAt = now.Add(-LastSeenGranularity) },
	}
	for name, mutate := range cases {
		i, q := stored, p
		mutate(&i, &q)
		if !NeedsUpsert(i, q, now) {
			t.Errorf("%s: no write", name)
		}
	}
	// Values are compared as stored: clipped and trimmed.
	long := p
	long.Name = strings.Repeat("n", 250)
	clipped := stored
	clipped.DisplayName = strings.Repeat("n", 200)
	if NeedsUpsert(clipped, long, now) {
		t.Fatal("a name longer than the stored limit always needs a write")
	}
}
