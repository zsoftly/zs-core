package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idempotency"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// Activation is one append-only activation decision.
type Activation struct {
	ID        idgen.UUID
	State     string
	Reason    string
	RequestID string
	Actor     events.Actor
	DecidedAt time.Time
}

// Decision is what staff with the activation grant decide.
type Decision struct {
	State     string
	Reason    string
	RequestID string
}

func validActivation(state string) bool {
	return state == ActivationPending || state == ActivationApproved || state == ActivationRejected
}

func decisionFingerprint(d Decision) string {
	sum := sha256.Sum256([]byte(d.State + "\x00" + d.Reason))
	return hex.EncodeToString(sum[:])
}

const activationColumns = `id, state, reason, request_id, actor_type, actor_id, decided_at`

func scanActivation(row pgx.Row) (Activation, string, error) {
	var a Activation
	var actorType, fingerprint string
	err := row.Scan(&a.ID, &a.State, &a.Reason, &a.RequestID, &actorType, &a.Actor.ID, &a.DecidedAt, &fingerprint)
	a.Actor.Type = events.ActorType(actorType)
	return a, fingerprint, err
}

// Decide records an activation decision and sets the tenant's activation
// state, in tx. It is idempotent on the request ID: the same request ID with
// the same state and reason returns the stored decision with replayed true,
// and with a different state or reason is idempotency_key_conflict. A closed
// tenant is tenant_not_found. The caller has checked the staff activation
// grant.
func Decide(ctx context.Context, tx pgx.Tx, s scope.Scope, d Decision, actor events.Actor, correlationID string) (Activation, bool, error) {
	d.Reason = strings.TrimSpace(d.Reason)
	switch {
	case !validActivation(d.State):
		return Activation{}, false, validation("The state must be pending, approved or rejected.")
	case d.Reason == "" || utf8.RuneCountInString(d.Reason) > 1000:
		return Activation{}, false, zserrors.New(zserrors.CodeReasonRequired, "An activation decision needs a reason of at most 1000 characters.")
	}
	if err := idempotency.ValidateKey(d.RequestID); err != nil {
		return Activation{}, false, validation("The request_id must be 8 to 255 printable characters.")
	}
	t, err := lockTenant(ctx, tx, s)
	if err != nil {
		return Activation{}, false, err
	}
	if t.Status == StatusClosed {
		return Activation{}, false, ErrNotFound
	}
	fingerprint := decisionFingerprint(d)
	args, err := s.Bind(d.RequestID)
	if err != nil {
		return Activation{}, false, err
	}
	existing, stored, err := scanActivation(tx.QueryRow(ctx, `SELECT `+activationColumns+`, fingerprint FROM tenant_activations
WHERE brand_id = $1 AND tenant_id = $2 AND request_id = $3`, args...))
	switch {
	case err == nil && stored == fingerprint:
		return existing, true, nil
	case err == nil:
		return Activation{}, false, zserrors.New(zserrors.CodeIdempotencyKeyConflict, "The request_id was already used with a different decision.")
	case !errors.Is(err, pgx.ErrNoRows):
		return Activation{}, false, unavailable(err)
	}
	id, err := idgen.NewV7()
	if err != nil {
		return Activation{}, false, err
	}
	args, err = s.Bind(id, d.State, d.Reason, d.RequestID, fingerprint, string(actor.Type), actor.ID)
	if err != nil {
		return Activation{}, false, err
	}
	a, _, err := scanActivation(tx.QueryRow(ctx, `INSERT INTO tenant_activations
    (brand_id, tenant_id, id, state, reason, request_id, fingerprint, actor_type, actor_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+activationColumns+`, fingerprint`, args...))
	if err != nil {
		return Activation{}, false, unavailable(err)
	}
	args, err = s.Bind(d.State)
	if err != nil {
		return Activation{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tenants SET activation = $3, updated_at = now() WHERE brand_id = $1 AND id = $2`, args...); err != nil {
		return Activation{}, false, unavailable(err)
	}
	if _, err := audit.Write(ctx, tx, s, audit.Entry{
		Actor: actor, Action: "tenant.activation.decided", Reason: d.Reason, Outcome: audit.OutcomeSuccess,
		CorrelationID: correlationID,
		Details:       map[string]any{"state": map[string]any{"before": t.Activation, "after": d.State}, "request_id": d.RequestID},
	}); err != nil {
		return Activation{}, false, err
	}
	return a, false, nil
}

// MaxActivationHistory is how many decisions ActivationOf returns.
const MaxActivationHistory = 50

// ActivationOf returns the tenant's activation state and its latest
// decisions, newest first.
func ActivationOf(ctx context.Context, q Querier, s scope.Scope) (string, []Activation, error) {
	t, err := Get(ctx, q, s)
	if err != nil {
		return "", nil, err
	}
	args, err := s.Bind(MaxActivationHistory)
	if err != nil {
		return "", nil, err
	}
	rows, err := q.Query(ctx, `SELECT `+activationColumns+`, fingerprint FROM tenant_activations
WHERE brand_id = $1 AND tenant_id = $2 ORDER BY decided_at DESC, id DESC LIMIT $3`, args...)
	if err != nil {
		return "", nil, unavailable(err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Activation, error) {
		a, _, err := scanActivation(r)
		return a, err
	})
	if err != nil {
		return "", nil, unavailable(err)
	}
	return t.Activation, list, nil
}
