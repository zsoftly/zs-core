package brand

import (
	"strings"
	"testing"
)

func TestCanonicalWebhookURLRejectsUnsupportedParts(t *testing.T) {
	got, err := CanonicalWebhookURL("https://[2001:4860:4860::8888]:443/a%2Fb")
	if err != nil || got != "https://[2001:4860:4860::8888]/a%2Fb" {
		t.Fatalf("canonical = %q, %v", got, err)
	}
	for _, value := range []string{
		"http://example.test",
		"https://user@example.test/a",
		"https://example.test/a?q=1",
		"https://example.test/a?",
		"https://example.test/a#fragment",
		"https://example.test/a#",
		"https:example.test/a",
		"https://example.test:",
		"https://example.test:0",
		"https://example.test:65536",
		"https://example.test:abc",
	} {
		if _, err := CanonicalWebhookURL(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestCanonicalWebhookURLEquivalentHosts(t *testing.T) {
	a, err := CanonicalWebhookURL("https://EXAMPLE.test./hook")
	if err != nil || a != "https://example.test/hook" {
		t.Fatalf("DNS identity = %q, %v", a, err)
	}
	b, err := CanonicalWebhookURL("https://[::ffff:192.0.2.1]/hook")
	if err != nil || b != "https://192.0.2.1/hook" {
		t.Fatalf("IP identity = %q, %v", b, err)
	}
	if _, err := CanonicalWebhookURL("https://[fe80::1%25en0]/hook"); err == nil {
		t.Fatal("zone was accepted")
	}
	defaultPort, err := CanonicalWebhookURL("https://example.test:00443/hook")
	if err != nil || defaultPort != "https://example.test/hook" {
		t.Fatalf("default port identity = %q, %v", defaultPort, err)
	}
	port, err := CanonicalWebhookURL("https://example.test:008443/hook")
	if err != nil || port != "https://example.test:8443/hook" {
		t.Fatalf("port identity = %q, %v", port, err)
	}
	escapedSlash, err := CanonicalWebhookURL("https://example.test/a%2fb")
	if err != nil || escapedSlash != "https://example.test/a%2Fb" {
		t.Fatalf("escaped slash identity = %q, %v", escapedSlash, err)
	}
	unreserved, err := CanonicalWebhookURL("https://example.test/%68ook")
	if err != nil || unreserved != "https://example.test/hook" {
		t.Fatalf("unreserved identity = %q, %v", unreserved, err)
	}
}

func TestManifestRejectsEquivalentWebhookEndpoints(t *testing.T) {
	for _, urls := range [][2]string{
		{"https://EXAMPLE.test./hook", "https://example.test:00443/hook"},
		{"https://example.test/a%2fb", "https://example.test/a%2Fb"},
		{"https://example.test/%68ook", "https://example.test/hook"},
	} {
		m := reference(t)
		m.Notifications = &Notifications{Webhooks: []Webhook{
			{URL: urls[0], SecretRef: "env://BRAND_ZS_REFERENCE__WEBHOOK_A"},
			{URL: urls[1], SecretRef: "env://BRAND_ZS_REFERENCE__WEBHOOK_B"},
		}}
		err := m.Validate()
		if err == nil || !strings.Contains(err.Error(), "/notifications/webhooks/1/url") || !strings.Contains(err.Error(), "/notifications/webhooks/0/url") {
			t.Fatalf("duplicate endpoint error for %q and %q = %v", urls[0], urls[1], err)
		}
	}
}
