package keycloakaudit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDeliverySignatureVector(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	body := []byte(`{"event_id":"e1"}`)
	got := DeliverySignature(key, "1760000000", body)
	const expected = "18e3a09b4c64c64c810cb1f0ba20bb13bf936c3415c3f6c9994d1b0ab555b76a"
	if got != expected {
		t.Fatalf("signature = %s, want %s", got, expected)
	}
	if DeliverySignature(append(append([]byte(nil), key...), 'x'), "1760000000", body) == expected {
		t.Fatal("tampered key retained signature")
	}
	if DeliverySignature(key, "1760000001", body) == expected {
		t.Fatal("tampered timestamp retained signature")
	}
	if DeliverySignature(key, "1760000000", append(body, ' ')) == expected {
		t.Fatal("tampered body retained signature")
	}
	if err := VerifyDelivery(key, "1760000000", got, body, time.Unix(1760000000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyDelivery(key, "1760000000", got, append(body, ' '), time.Unix(1760000000, 0)); err == nil {
		t.Fatal("tampered body accepted")
	}
	for _, signature := range []string{strings.ToUpper(got), got[:2] + strings.ToUpper(got[2:3]) + got[3:]} {
		if err := VerifyDelivery(key, "1760000000", signature, body, time.Unix(1760000000, 0)); err == nil {
			t.Fatalf("accepted alternate-case signature: %s", signature)
		}
	}
}

func TestDecodeRejectsDuplicateAndWrongKind(t *testing.T) {
	valid := `{"version":1,"source_id":"keycloak","event_id":"x","realm_id":"realm-1","realm_name":"realm","kind":"user","occurred_at_ms":1,"failed":false,"event_type":"LOGIN"}`
	if _, err := Decode([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode([]byte(strings.Replace(valid, `"event_id":"x"`, `"event_id":"x","event_id":"y"`, 1))); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err := Decode([]byte(strings.Replace(valid, `"event_type":"LOGIN"`, `"event_type":"LOGIN","operation":"CREATE"`, 1))); err == nil {
		t.Fatal("mixed kind accepted")
	}
}

func TestDecodeRejectsNullAndWrongPrimitiveTypes(t *testing.T) {
	valid := `{"version":1,"source_id":"keycloak","event_id":"x","realm_id":"realm-1","realm_name":"realm","kind":"user","occurred_at_ms":1,"failed":false,"event_type":"LOGIN"}`
	for _, pair := range [][2]string{
		{`"event_id":"x"`, `"event_id":null`}, {`"event_id":"x"`, `"event_id":1`},
		{`"failed":false`, `"failed":"false"`}, {`"failed":false`, `"failed":null`},
		{`"version":1`, `"version":"1"`}, {`"version":1`, `"version":1.0`},
		{`"occurred_at_ms":1`, `"occurred_at_ms":null`}, {`"occurred_at_ms":1`, `"occurred_at_ms":1e3`},
	} {
		if _, err := Decode([]byte(strings.Replace(valid, pair[0], pair[1], 1))); err == nil {
			t.Fatalf("accepted %s", pair[1])
		}
	}
}

func TestDecodeRejectsMalformedUTF8AndCrossKindFields(t *testing.T) {
	validUser := []byte(`{"version":1,"source_id":"keycloak","event_id":"x","realm_id":"realm-1","realm_name":"realm","kind":"user","occurred_at_ms":1,"failed":false,"event_type":"LOGIN"}`)
	malformed := append([]byte(nil), validUser...)
	index := bytes.Index(malformed, []byte(`"event_id":"x"`))
	if index < 0 {
		t.Fatal("fixture lacks event ID")
	}
	malformed[index+len(`"event_id":"`)] = 0xff
	if _, err := Decode(malformed); err == nil {
		t.Fatal("accepted malformed UTF-8")
	}
	validReplacement := bytes.Replace(validUser, []byte(`"event_id":"x"`), []byte("\"event_id\":\"�\""), 1)
	if _, err := Decode(validReplacement); err != nil {
		t.Fatalf("replacement-character control rejected: %v", err)
	}
	for _, body := range []string{
		`{"version":1,"source_id":"keycloak","event_id":"x","realm_id":"realm-1","realm_name":"realm","kind":"user","occurred_at_ms":1,"failed":false}`,
		`{"version":1,"source_id":"keycloak","event_id":"x","realm_id":"realm-1","realm_name":"realm","kind":"user","occurred_at_ms":1,"failed":false,"event_type":"LOGIN","operation":""}`,
		`{"version":1,"source_id":"keycloak","event_id":"x","realm_id":"realm-1","realm_name":"realm","kind":"admin","occurred_at_ms":1,"failed":false,"operation":"UPDATE","resource_type":"USER","client_id":""}`,
	} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Fatalf("accepted invalid kind shape: %s", body)
		}
	}
}

func TestDecodeEscapedKindsApplyCrossKindRules(t *testing.T) {
	validUser := `{"version":1,"source_id":"keycloak","event_id":"x","realm_id":"realm-1","realm_name":"realm","kind":"\u0075ser","occurred_at_ms":1,"failed":false,"event_type":"LOGIN"}`
	if _, err := Decode([]byte(validUser)); err != nil {
		t.Fatalf("escaped user rejected: %v", err)
	}
	validAdmin := `{"version":1,"source_id":"keycloak","event_id":"x","realm_id":"realm-1","realm_name":"realm","kind":"adm\u0069n","occurred_at_ms":1,"failed":false,"operation":"UPDATE","resource_type":"USER"}`
	if _, err := Decode([]byte(validAdmin)); err != nil {
		t.Fatalf("escaped admin rejected: %v", err)
	}
	for _, field := range []string{"operation", "resource_type", "admin_actor_realm_id", "admin_actor_user_id", "admin_actor_client_id"} {
		body := strings.Replace(validUser, `}`, `,"`+field+`":""}`, 1)
		if _, err := Decode([]byte(body)); err == nil {
			t.Fatalf("escaped user accepted %s", field)
		}
	}
	for _, field := range []string{"event_type", "subject_id", "client_id"} {
		body := strings.Replace(validAdmin, `}`, `,"`+field+`":""}`, 1)
		if _, err := Decode([]byte(body)); err == nil {
			t.Fatalf("escaped admin accepted %s", field)
		}
	}
}
