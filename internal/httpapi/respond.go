package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
)

func (s *server) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeInternal, "", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}

// writeError writes the error envelope. Only the safe message of a typed
// error reaches the client; the full error, including any cause, is logged
// for server errors.
func (s *server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, envelope := zserrors.ToEnvelope(err)
	if isBodyTooLarge(err) {
		status, envelope = zserrors.ToEnvelope(zserrors.New(zserrors.CodePayloadTooLarge, ""))
	}
	if status >= http.StatusInternalServerError {
		code := zserrors.CodeInternal
		var zsErr *zserrors.Error
		if errors.As(err, &zsErr) {
			code = zsErr.Code
		}
		s.log.ErrorContext(r.Context(), "request failed",
			"request_id", RequestID(r.Context()),
			"code", string(code),
			"error", errString(err))
	}
	if envelope.Error.Code == zserrors.Lookup(zserrors.CodeTenantBusy).Name {
		w.Header().Set("Retry-After", "1")
	}
	s.writeJSON(w, r, status, gen.ErrorEnvelope{Error: gen.Error{
		Code:      envelope.Error.Code,
		Message:   envelope.Error.Message,
		Retryable: envelope.Error.Retryable,
	}})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func levelFor(status int) slog.Level {
	if status >= http.StatusInternalServerError {
		return slog.LevelError
	}
	return slog.LevelInfo
}
