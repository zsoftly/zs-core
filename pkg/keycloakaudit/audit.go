// Package keycloakaudit validates the narrow Keycloak-to-Core audit protocol.
package keycloakaudit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	Version      = 1
	Path         = "/v1/identity/events/keycloak"
	MaxBodyBytes = 16 << 10
	MaxSkew      = 5 * time.Minute
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Envelope is the privacy-minimized event sent by the Keycloak listener.
// It deliberately has no representation, details, resource path, IP address,
// username, email, session, token, or provider error field.
type Envelope struct {
	Version            int    `json:"version"`
	SourceID           string `json:"source_id"`
	EventID            string `json:"event_id"`
	RealmID            string `json:"realm_id"`
	RealmName          string `json:"realm_name"`
	Kind               string `json:"kind"`
	OccurredAtMS       int64  `json:"occurred_at_ms"`
	Failed             bool   `json:"failed"`
	EventType          string `json:"event_type,omitempty"`
	SubjectID          string `json:"subject_id,omitempty"`
	ClientID           string `json:"client_id,omitempty"`
	Operation          string `json:"operation,omitempty"`
	ResourceType       string `json:"resource_type,omitempty"`
	AdminActorRealmID  string `json:"admin_actor_realm_id,omitempty"`
	AdminActorUserID   string `json:"admin_actor_user_id,omitempty"`
	AdminActorClientID string `json:"admin_actor_client_id,omitempty"`
}

func bounded(name, value string, min, max int) error {
	if len(value) < min || len(value) > max || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

// Validate rejects any combination that would make a user and admin event
// ambiguous. The event names remain pinned at the sender; Core accepts only
// protocol-safe, bounded values here.
func (e Envelope) Validate() error {
	if e.Version != Version || !identifier.MatchString(e.SourceID) || bounded("event_id", e.EventID, 1, 128) != nil {
		return errors.New("invalid Keycloak audit envelope")
	}
	if err := bounded("realm_id", e.RealmID, 1, 255); err != nil {
		return errors.New("invalid Keycloak audit envelope")
	}
	if err := bounded("realm_name", e.RealmName, 1, 63); err != nil {
		return errors.New("invalid Keycloak audit envelope")
	}
	if e.OccurredAtMS <= 0 {
		return errors.New("invalid Keycloak audit envelope")
	}
	for _, v := range []string{e.SubjectID, e.ClientID, e.AdminActorRealmID, e.AdminActorUserID, e.AdminActorClientID} {
		if v != "" && bounded("identifier", v, 1, 255) != nil {
			return errors.New("invalid Keycloak audit envelope")
		}
	}
	switch e.Kind {
	case "user":
		if bounded("event_type", e.EventType, 1, 100) != nil || e.Operation != "" || e.ResourceType != "" || e.AdminActorRealmID != "" || e.AdminActorUserID != "" || e.AdminActorClientID != "" {
			return errors.New("invalid Keycloak audit envelope")
		}
	case "admin":
		if bounded("operation", e.Operation, 1, 32) != nil || bounded("resource_type", e.ResourceType, 1, 128) != nil || e.EventType != "" || e.SubjectID != "" || e.ClientID != "" {
			return errors.New("invalid Keycloak audit envelope")
		}
	default:
		return errors.New("invalid Keycloak audit envelope")
	}
	return nil
}

// DeliverySignature returns the lowercase hexadecimal HMAC-SHA256 for the
// canonical byte sequence timestamp + "\\n" + exact body bytes. Timestamp is
// decimal Unix seconds, encoded without whitespace or a sign.
func DeliverySignature(secret []byte, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp))
	m.Write([]byte{'\n'})
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyDelivery checks timestamp skew and signature. It accepts no alternate
// encodings so the provider and receiver sign exactly the same bytes.
func VerifyDelivery(secret []byte, timestamp, signature string, body []byte, now time.Time) error {
	if !regexp.MustCompile(`^[0-9]{1,16}$`).MatchString(timestamp) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(signature) {
		return errors.New("invalid Keycloak audit signature")
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || abs(now.Unix()-seconds) > int64(MaxSkew/time.Second) {
		return errors.New("invalid Keycloak audit signature")
	}
	provided, err := hex.DecodeString(signature)
	expected, _ := hex.DecodeString(DeliverySignature(secret, timestamp, body))
	if err != nil || !hmac.Equal(provided, expected) {
		return errors.New("invalid Keycloak audit signature")
	}
	return nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// BodyDigest returns the stable digest stored with an idempotent receipt.
func BodyDigest(body []byte) []byte { sum := sha256.Sum256(body); return sum[:] }
