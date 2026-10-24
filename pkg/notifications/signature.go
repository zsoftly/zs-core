// Package notifications fans public outbox events to a brand's configured
// webhook endpoints. Deliveries are at least once; receivers deduplicate on
// the stable event ID carried by the envelope.
package notifications

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	SignatureHeader    = "X-ZS-Signature"
	SignatureTolerance = 5 * time.Minute
)

// Sign formats the v1 signature header for the exact bytes sent on the wire.
func Sign(secret, body []byte, now time.Time) string {
	timestamp := strconv.FormatInt(now.UTC().Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "t=" + timestamp + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks the signature in constant time and rejects timestamps outside
// tolerance. It intentionally does not provide replay storage: receivers must
// retain and deduplicate the envelope event ID for their own retention period.
func Verify(secret, body []byte, header string, now time.Time) error {
	if len(secret) == 0 {
		return errors.New("webhook signing secret is required")
	}
	var timestamp string
	var signature string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return errors.New("webhook signature is malformed")
		}
		switch key {
		case "t":
			if timestamp != "" || value == "" {
				return errors.New("webhook signature is malformed")
			}
			timestamp = value
		case "v1":
			if signature != "" || value == "" {
				return errors.New("webhook signature is malformed")
			}
			signature = value
		default:
			return errors.New("webhook signature is malformed")
		}
	}
	if timestamp == "" || signature == "" {
		return errors.New("webhook signature is malformed")
	}
	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("webhook signature timestamp is invalid")
	}
	if age := now.UTC().Sub(time.Unix(unix, 0)); age > SignatureTolerance || age < -SignatureTolerance {
		return errors.New("webhook signature timestamp is outside tolerance")
	}
	provided, err := hex.DecodeString(signature)
	if err != nil || len(provided) != sha256.Size {
		return errors.New("webhook signature is malformed")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	expectedBytes := mac.Sum(nil)
	if subtle.ConstantTimeCompare(provided, expectedBytes) != 1 {
		return fmt.Errorf("webhook signature does not match")
	}
	return nil
}
