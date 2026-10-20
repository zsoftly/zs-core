package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

const (
	headerRequestID = "X-Request-ID"
	headerBrand     = "X-ZS-Brand"
	headerTenant    = "X-ZS-Tenant"
	tracerName      = "github.com/zsoftly/zs-core/internal/httpapi"
)

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// requestInfo is per-request state shared between middleware.
type requestInfo struct {
	requestID string
	route     string
}

type requestInfoKey struct{}

func info(ctx context.Context) *requestInfo {
	if ri, ok := ctx.Value(requestInfoKey{}).(*requestInfo); ok {
		return ri
	}
	return &requestInfo{}
}

// RequestID returns the request ID of the request in ctx.
func RequestID(ctx context.Context) string { return info(ctx).requestID }

// statusWriter records the status and size of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// requestContext assigns the request ID, joins or starts a W3C trace and
// writes one access log line per request.
func (s *server) requestContext(next http.Handler) http.Handler {
	propagator := propagation.TraceContext{}
	tracer := otel.Tracer(tracerName)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := r.Header.Get(headerRequestID)
		if !validRequestID.MatchString(requestID) {
			requestID = idgen.MustNewV7().String()
		}
		ri := &requestInfo{requestID: requestID}

		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		if !trace.SpanContextFromContext(ctx).IsValid() {
			// No tracing SDK is installed and the caller sent no
			// traceparent: start a local trace so events and logs still
			// carry one.
			ctx = trace.ContextWithSpanContext(ctx, newSpanContext())
		}
		ctx = context.WithValue(ctx, requestInfoKey{}, ri)

		w.Header().Set(headerRequestID, requestID)
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(ctx))

		if ri.route != "" {
			span.SetName(ri.route)
		}
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		s.log.LogAttrs(ctx, levelFor(status), "http request",
			slog.String("request_id", requestID),
			slog.String("trace_id", trace.SpanContextFromContext(ctx).TraceID().String()),
			slog.String("method", r.Method),
			slog.String("route", ri.route),
			slog.Int("status", status),
			slog.Int("bytes", sw.bytes),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		)
	})
}

func newSpanContext() trace.SpanContext {
	var traceID trace.TraceID
	var spanID trace.SpanID
	_, _ = rand.Read(traceID[:])
	_, _ = rand.Read(spanID[:])
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})
}

// securityHeaders sets headers that apply to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// recoverPanics turns a handler panic into a 500 envelope and logs the stack.
func (s *server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw, ok := w.(*statusWriter)
		if !ok {
			sw = &statusWriter{ResponseWriter: w}
		}
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			s.log.ErrorContext(r.Context(), "handler panic",
				"request_id", RequestID(r.Context()),
				"panic", fmt.Sprint(recovered),
				"stack", string(debug.Stack()))
			if sw.status == 0 {
				s.writeError(sw, r, zserrors.New(zserrors.CodeInternal, ""))
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// limitBody rejects declared bodies over MaxBodyBytes and caps the rest.
func (s *server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes {
			s.writeError(w, r, zserrors.New(zserrors.CodePayloadTooLarge, ""))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// route dispatches to mux and turns its plain-text 404 and 405 responses into
// error envelopes.
func (s *server) route(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		if pattern != "" {
			info(r.Context()).route = pattern
			mux.ServeHTTP(w, r)
			return
		}
		probe := &headerProbe{header: http.Header{}}
		mux.ServeHTTP(probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			if allow := probe.header.Get("Allow"); allow != "" {
				w.Header().Set("Allow", allow)
			}
			s.writeError(w, r, zserrors.New(zserrors.CodeMethodNotAllowed, ""))
			return
		}
		s.writeError(w, r, zserrors.New(zserrors.CodeNotFound, ""))
	})
}

// headerProbe captures the status and headers the mux would send for an
// unmatched request, discarding the body.
type headerProbe struct {
	header http.Header
	status int
}

func (p *headerProbe) Header() http.Header         { return p.header }
func (p *headerProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *headerProbe) WriteHeader(status int)      { p.status = status }

type scopeKey struct{}

// scoped resolves the brand and tenant of a /v1 request. Phase 1 reads them
// from trusted headers when enabled; otherwise every request is
// unauthenticated.
func (s *server) scoped(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.trustedScopeHeaders {
			s.writeError(w, r, zserrors.New(zserrors.CodeUnauthenticated, ""))
			return
		}
		sc, err := scope.Parse(r.Header.Get(headerBrand), r.Header.Get(headerTenant))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(scope.WithContext(r.Context(), sc)))
	})
}

// isBodyTooLarge reports whether err came from the body limit.
func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
