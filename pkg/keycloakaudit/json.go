package keycloakaudit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// Decode parses one JSON object, rejecting unknown and duplicate fields.
func Decode(body []byte) (Envelope, error) {
	if len(body) == 0 || len(body) > MaxBodyBytes || !utf8.Valid(body) || !json.Valid(body) {
		return Envelope{}, errors.New("invalid Keycloak audit envelope")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return Envelope{}, errors.New("invalid Keycloak audit envelope")
	}
	if d, ok := token.(json.Delim); !ok || d != '{' {
		return Envelope{}, errors.New("invalid Keycloak audit envelope")
	}
	allowed := map[string]bool{"version": true, "source_id": true, "event_id": true, "realm_id": true, "realm_name": true, "kind": true, "occurred_at_ms": true, "failed": true, "event_type": true, "subject_id": true, "client_id": true, "operation": true, "resource_type": true, "admin_actor_realm_id": true, "admin_actor_user_id": true, "admin_actor_client_id": true}
	values := make(map[string]json.RawMessage, len(allowed))
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return Envelope{}, errors.New("invalid Keycloak audit envelope")
		}
		name, ok := key.(string)
		if !ok || !allowed[name] || values[name] != nil {
			return Envelope{}, errors.New("invalid Keycloak audit envelope")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return Envelope{}, errors.New("invalid Keycloak audit envelope")
		}
		values[name] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return Envelope{}, errors.New("invalid Keycloak audit envelope")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Envelope{}, errors.New("invalid Keycloak audit envelope")
	}
	for _, required := range []string{"version", "source_id", "event_id", "realm_id", "realm_name", "kind", "occurred_at_ms", "failed"} {
		if values[required] == nil {
			return Envelope{}, errors.New("invalid Keycloak audit envelope")
		}
	}
	for _, name := range []string{"source_id", "event_id", "realm_id", "realm_name", "kind", "event_type", "subject_id", "client_id", "operation", "resource_type", "admin_actor_realm_id", "admin_actor_user_id", "admin_actor_client_id"} {
		if raw := values[name]; raw != nil && (len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"') {
			return Envelope{}, errors.New("invalid Keycloak audit envelope")
		}
	}
	if raw := values["failed"]; string(raw) != "true" && string(raw) != "false" {
		return Envelope{}, errors.New("invalid Keycloak audit envelope")
	}
	for _, name := range []string{"version", "occurred_at_ms"} {
		raw := values[name]
		if len(raw) == 0 || raw[0] == '-' || bytes.IndexAny(raw, ".eE") >= 0 {
			return Envelope{}, errors.New("invalid Keycloak audit envelope")
		}
	}
	var kind string
	if err := json.Unmarshal(values["kind"], &kind); err != nil {
		return Envelope{}, errors.New("invalid Keycloak audit envelope")
	}
	if kind == "user" {
		if values["event_type"] == nil || values["operation"] != nil || values["resource_type"] != nil || values["admin_actor_realm_id"] != nil || values["admin_actor_user_id"] != nil || values["admin_actor_client_id"] != nil {
			return Envelope{}, errors.New("invalid Keycloak audit envelope")
		}
	} else if kind == "admin" {
		if values["operation"] == nil || values["resource_type"] == nil || values["event_type"] != nil || values["subject_id"] != nil || values["client_id"] != nil {
			return Envelope{}, errors.New("invalid Keycloak audit envelope")
		}
	}
	encoded, _ := json.Marshal(values)
	var e Envelope
	if err := json.Unmarshal(encoded, &e); err != nil || e.Validate() != nil {
		return Envelope{}, errors.New("invalid Keycloak audit envelope")
	}
	return e, nil
}
