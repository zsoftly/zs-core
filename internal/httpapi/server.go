// Package httpapi serves the ZSoftly Core HTTP API.
//
// The handler chain, outermost first, is: request context (request ID, W3C
// trace context and the access log), security headers including
// Cache-Control: no-store, panic recovery, the request body limit, and the
// router. Every error leaves through writeError as the JSON error envelope.
// Request and response types come from internal/httpapi/gen, which is
// generated from api/openapi.yaml.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// MaxBodyBytes is the request body limit, 1 MiB.
const MaxBodyBytes int64 = 1 << 20

// DB is the subset of pgxpool.Pool the API needs.
type DB interface {
	Ping(ctx context.Context) error
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options configure the API.
type Options struct {
	DB         DB
	Migrations fs.FS
	Logger     *slog.Logger
	// TrustedScopeHeaders reads the brand and tenant of /v1 requests from
	// the X-ZS-Brand and X-ZS-Tenant headers. Development only; see
	// internal/config.
	TrustedScopeHeaders bool
}

type server struct {
	db                  DB
	migrations          fs.FS
	log                 *slog.Logger
	trustedScopeHeaders bool
}

// New returns the API handler.
func New(opts Options) http.Handler {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &server{
		db:                  opts.DB,
		migrations:          opts.Migrations,
		log:                 log,
		trustedScopeHeaders: opts.TrustedScopeHeaders,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /v1/audit/events", s.scoped(http.HandlerFunc(s.listAuditEvents)))

	var h http.Handler = s.route(mux)
	h = s.limitBody(h)
	h = s.recoverPanics(h)
	h = securityHeaders(h)
	h = s.requestContext(h)
	return h
}
