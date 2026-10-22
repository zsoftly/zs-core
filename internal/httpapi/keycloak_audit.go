package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/keycloakaudit"
)

const keycloakAuditMaxBody = keycloakaudit.MaxBodyBytes

func oneHeader(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, len(values) == 1 && returnValue != ""
}

// receiveKeycloakAudit accepts only the deployment-authenticated listener.
// It is deliberately outside bearer-token authentication.
func (s *server) receiveKeycloakAudit(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/json" {
		s.writeError(w, r, zserrors.New(zserrors.CodeUnsupportedMediaType, ""))
		return
	}
	realm, realmOK := oneHeader(r, "X-ZS-Keycloak-Realm")
	timestamp, timestampOK := oneHeader(r, "X-ZS-Keycloak-Timestamp")
	signature, signatureOK := oneHeader(r, "X-ZS-Keycloak-Signature")
	secret := s.keycloakAuditSecrets[realm]
	if !realmOK || !timestampOK || !signatureOK || len(secret) == 0 {
		s.writeError(w, r, zserrors.New(zserrors.CodeUnauthenticated, ""))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, keycloakAuditMaxBody))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := keycloakaudit.VerifyDelivery(secret, timestamp, signature, body, s.now()); err != nil {
		s.writeError(w, r, zserrors.New(zserrors.CodeUnauthenticated, ""))
		return
	}
	e, err := keycloakaudit.Decode(body)
	if err != nil || e.RealmName != realm || e.SourceID != s.keycloakAuditSourceID {
		s.writeError(w, r, zserrors.New(zserrors.CodeBadRequest, ""))
		return
	}
	if err := s.recordKeycloakAudit(r.Context(), realm, e, keycloakaudit.BodyDigest(body)); err != nil {
		if errors.Is(err, errKeycloakAuditConflict) {
			s.writeError(w, r, zserrors.New(zserrors.CodeConflict, ""))
			return
		}
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var errKeycloakAuditConflict = errors.New("keycloak audit duplicate body conflict")

func (s *server) recordKeycloakAudit(ctx context.Context, realm string, e keycloakaudit.Envelope, digest []byte) error {
	if s.db == nil {
		return zserrors.New(zserrors.CodeNotReady, "")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	defer tx.Rollback(ctx)
	var brandID idgen.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM brands WHERE keycloak_realm = $1 AND status = 'active' AND applied_at IS NOT NULL`, realm).Scan(&brandID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zserrors.New(zserrors.CodeNotFound, "")
		}
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	var inserted bool
	err = tx.QueryRow(ctx, `INSERT INTO keycloak_audit_receipts (brand_id, realm_name, kind, event_id, body_digest) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING RETURNING true`, brandID, realm, e.Kind, e.EventID, digest).Scan(&inserted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	if !inserted {
		var prior []byte
		if err := tx.QueryRow(ctx, `SELECT body_digest FROM keycloak_audit_receipts WHERE brand_id = $1 AND realm_name = $2 AND kind = $3 AND event_id = $4`, brandID, realm, e.Kind, e.EventID).Scan(&prior); err != nil {
			return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
		}
		if !bytes.Equal(prior, digest) {
			return errKeycloakAuditConflict
		}
		return tx.Commit(ctx)
	}
	outcome := audit.OutcomeSuccess
	if e.Failed {
		outcome = audit.OutcomeFailure
	}
	details := map[string]any{"realm_id": e.RealmID}
	for key, value := range map[string]string{
		"event_type": e.EventType, "subject_id": e.SubjectID, "client_id": e.ClientID,
		"operation": e.Operation, "resource_type": e.ResourceType,
		"admin_actor_realm_id": e.AdminActorRealmID, "admin_actor_user_id": e.AdminActorUserID,
		"admin_actor_client_id": e.AdminActorClientID,
	} {
		if value != "" {
			details[key] = value
		}
	}
	_, err = audit.WriteBrand(ctx, tx, brandID, audit.Entry{Actor: events.Actor{Type: events.ActorProvider, ID: "keycloak:" + realm}, Action: "identity.keycloak." + e.Kind, Outcome: outcome, CorrelationID: e.EventID, OccurredAt: time.UnixMilli(e.OccurredAtMS), Details: details})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
