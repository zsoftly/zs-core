package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/zsoftly/zs-core/internal/database"
	"github.com/zsoftly/zs-core/internal/httpapi/gen"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
)

const readinessTimeout = 2 * time.Second

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, gen.HealthStatus{Status: gen.HealthStatusStatusOk})
}

// readyz is ready when the database answers and every embedded migration is
// applied.
func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	if s.db == nil {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotReady, ""))
		return
	}
	if err := s.db.Ping(ctx); err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err))
		return
	}
	pending, err := database.Pending(ctx, s.db, s.migrations)
	if err != nil {
		s.writeError(w, r, zserrors.Wrap(zserrors.CodeNotReady, "", err))
		return
	}
	if len(pending) > 0 {
		s.writeError(w, r, zserrors.New(zserrors.CodeNotReady, "Database migrations are pending."))
		return
	}
	s.writeJSON(w, r, http.StatusOK, gen.HealthStatus{Status: gen.HealthStatusStatusReady})
}
