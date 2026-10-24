package notifications

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestVerifyRejectsChangedBodyAndStaleTimestamp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	secret := []byte("a test signing secret")
	body := []byte(`{"envelope_version":1,"id":"evt_example"}`)
	header := Sign(secret, body, now)
	if err := Verify(nil, body, header, now); err == nil {
		t.Fatal("empty secret was accepted")
	}
	if err := Verify(secret, body, header, now); err != nil {
		t.Fatal(err)
	}
	if err := Verify(secret, append([]byte(nil), append(body, ' ')...), header, now); err == nil {
		t.Fatal("changed body was accepted")
	}
	if err := Verify(secret, body, header, now.Add(SignatureTolerance+time.Second)); err == nil {
		t.Fatal("stale timestamp was accepted")
	}
	if err := Verify(secret, body, strings.Replace(header, "v1=", "v2=", 1), now); err == nil {
		t.Fatal("unknown signature version was accepted")
	}
}

func TestVerifyRejectsMalformedDuplicateAndFutureFields(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	secret := []byte("a test signing secret")
	body := []byte(`{"envelope_version":1,"id":"evt_example"}`)
	header := Sign(secret, body, now)

	for _, invalid := range []string{
		"t=1700000000",
		"v1=00",
		header + ",v1=00",
		header + ",t=1700000000",
		header + ",v2=00",
		"t=9223372036854775808,v1=00",
		"t=1700000000,v1",
	} {
		if err := Verify(secret, body, invalid, now); err == nil {
			t.Errorf("Verify accepted %q", invalid)
		}
	}
	if err := Verify(secret, body, Sign(secret, body, now.Add(SignatureTolerance+time.Second)), now); err == nil {
		t.Fatal("future timestamp was accepted")
	}
}

func TestSignMatchesKnownHMACSHA256Vector(t *testing.T) {
	secret := []byte("a test signing secret")
	body := []byte(`{"envelope_version":1,"id":"evt_example"}`)
	now := time.Unix(1_700_000_000, 0)
	const want = "t=1700000000,v1=c4772330f044a597a1cdd60a23b0828a4eb0c2d7f867566d03fe837efa01af13"
	if got := Sign(secret, body, now); got != want {
		t.Fatalf("Sign() = %q, want %q", got, want)
	}
}

func TestVerifyUsesLiteralTimestampText(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	secret := []byte("a test signing secret")
	body := []byte(`{"envelope_version":1,"id":"evt_example"}`)
	for _, tc := range []struct {
		name, timestamp string
	}{
		{"canonical", "1700000000"},
		{"leading_zero", "01700000000"},
		{"plus", "+1700000000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Verify(secret, body, literalSignatureHeader(secret, body, tc.timestamp, tc.timestamp, false), now); err != nil {
				t.Fatal("literal timestamp signature was rejected")
			}
			if tc.timestamp == "1700000000" {
				return
			}
			if err := Verify(secret, body, literalSignatureHeader(secret, body, "1700000000", tc.timestamp, false), now); err == nil {
				t.Fatal("canonical timestamp signature was accepted for a noncanonical timestamp spelling")
			}
		})
	}
	if err := Verify(secret, body, literalSignatureHeader(secret, body, "1700000000", "1700000000", true), now); err != nil {
		t.Fatal("upper-case hexadecimal signature was rejected")
	}
}

func literalSignatureHeader(secret, body []byte, signedTimestamp, headerTimestamp string, uppercase bool) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signedTimestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))
	if uppercase {
		signature = strings.ToUpper(signature)
	}
	return "t=" + headerTimestamp + ",v1=" + signature
}
