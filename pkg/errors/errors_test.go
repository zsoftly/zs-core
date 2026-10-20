package errors

import (
	stderrors "errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestDefinitionsAreUniqueAndWellFormed(t *testing.T) {
	codePattern := regexp.MustCompile(`^ZS-[0-9]{4}$`)
	namePattern := regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	codes := map[Code]bool{}
	names := map[string]bool{}
	for _, d := range Definitions() {
		if !codePattern.MatchString(string(d.Code)) {
			t.Errorf("code %q does not match ZS-nnnn", d.Code)
		}
		if !namePattern.MatchString(d.Name) {
			t.Errorf("name %q is not snake_case", d.Name)
		}
		if codes[d.Code] || names[d.Name] {
			t.Errorf("duplicate definition %s %s", d.Code, d.Name)
		}
		codes[d.Code], names[d.Name] = true, true
		if d.Status < 400 || d.Status > 599 {
			t.Errorf("%s has non-error status %d", d.Code, d.Status)
		}
		if d.Message == "" {
			t.Errorf("%s has no default message", d.Code)
		}
		if d.Status == http.StatusServiceUnavailable && !strings.HasSuffix(d.Name, "_unavailable") && d.Name != "not_ready" {
			t.Errorf("%s returns 503 but is not named *_unavailable", d.Code)
		}
	}
}

func TestToEnvelopeHidesCause(t *testing.T) {
	cause := stderrors.New("pq: password authentication failed for user secret_admin at 10.0.0.5")
	err := Wrap(CodeDatabaseUnavailable, "", cause)
	status, env := ToEnvelope(fmt.Errorf("list audit: %w", err))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", status)
	}
	if env.Error.Code != "database_unavailable" || !env.Error.Retryable {
		t.Fatalf("envelope = %+v", env)
	}
	if strings.Contains(env.Error.Message, "secret_admin") || strings.Contains(env.Error.Message, "10.0.0.5") {
		t.Fatalf("cause leaked into message: %q", env.Error.Message)
	}
	if !stderrors.Is(err, cause) {
		t.Fatal("cause is not reachable with errors.Is")
	}
	if !strings.Contains(err.Error(), "secret_admin") {
		t.Fatal("cause should remain in the log form")
	}
}

func TestToEnvelopeForUntypedAndUnknown(t *testing.T) {
	status, env := ToEnvelope(stderrors.New("stripe: card_declined for cus_123"))
	if status != http.StatusInternalServerError || env.Error.Code != "internal_error" {
		t.Fatalf("untyped error mapped to %d %+v", status, env)
	}
	if strings.Contains(env.Error.Message, "cus_123") {
		t.Fatal("untyped error text leaked")
	}
	status, env = ToEnvelope(New(Code("ZS-9999"), "do not show this"))
	if status != http.StatusInternalServerError || env.Error.Message == "do not show this" {
		t.Fatalf("unknown code mapped to %d %+v", status, env)
	}
	status, env = ToEnvelope(nil)
	if status != http.StatusInternalServerError || env.Error.Code != "internal_error" {
		t.Fatalf("nil error mapped to %d %+v", status, env)
	}
}

func TestStatusMapping(t *testing.T) {
	cases := map[Code]int{
		CodeValidation:               http.StatusBadRequest,
		CodeNotFound:                 http.StatusNotFound,
		CodeUnauthenticated:          http.StatusUnauthorized,
		CodeForbidden:                http.StatusForbidden,
		CodeIdempotencyKeyConflict:   http.StatusConflict,
		CodePayloadTooLarge:          http.StatusRequestEntityTooLarge,
		CodeMethodNotAllowed:         http.StatusMethodNotAllowed,
		CodeRateLimited:              http.StatusTooManyRequests,
		CodeDatabaseUnavailable:      http.StatusServiceUnavailable,
		CodeIdempotencyKeyInProgress: http.StatusConflict,
	}
	for code, want := range cases {
		status, env := ToEnvelope(New(code, ""))
		if status != want {
			t.Errorf("%s status = %d, want %d", code, status, want)
		}
		if env.Error.Message != Lookup(code).Message {
			t.Errorf("%s default message not used", code)
		}
	}
}

func TestIsCode(t *testing.T) {
	err := fmt.Errorf("outer: %w", New(CodeNotFound, "Audit event not found."))
	if !IsCode(err, CodeNotFound) || IsCode(err, CodeConflict) || IsCode(stderrors.New("x"), CodeNotFound) {
		t.Fatal("IsCode mismatch")
	}
	var nilErr *Error
	if nilErr.Error() != "<nil>" || nilErr.Unwrap() != nil {
		t.Fatal("nil receiver handling")
	}
}
