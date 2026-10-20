package apikey

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseSecret(t *testing.T) {
	random := base64.RawURLEncoding.EncodeToString(make([]byte, secretBytes))
	if len(random) != encodedSecretLength {
		t.Fatalf("encoded length %d", len(random))
	}
	// The random part may hold underscores; the slug never does.
	withUnderscore := "_" + random[1:]
	for secret, want := range map[string]string{
		"zs_zs-reference_" + random:         "zs-reference",
		"zs_zs_" + random:                   "zs",
		"zs_zs-reference_" + withUnderscore: "zs-reference",
	} {
		if slug, ok := ParseSecret(secret); !ok || slug != want {
			t.Errorf("ParseSecret(%q) = %q, %v", secret, slug, ok)
		}
	}
	for _, bad := range []string{
		"", "zs_", "zs_zs-reference", "zs_zs-reference_" + random[:42], "zs_zs-reference_" + random + "A",
		"zs_Bad-Slug_" + random, "zs_-bad_" + random, "ey" + random, "zs_zs-reference_" + strings.Repeat("!", 43),
	} {
		if _, ok := ParseSecret(bad); ok {
			t.Errorf("ParseSecret(%q) accepted", bad)
		}
	}
}

func TestDigestIsSHA256(t *testing.T) {
	if len(digestOf("zs_x_y")) != 32 {
		t.Fatal("digest length")
	}
	if string(digestOf("a")) == string(digestOf("b")) {
		t.Fatal("digest collision")
	}
}
