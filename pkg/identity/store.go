package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Identity is a row of the identities table: one person or service account
// of one brand, keyed on issuer and subject. Email is an attribute, never a
// key.
type Identity struct {
	ID             idgen.UUID
	BrandID        idgen.UUID
	Issuer         string
	Subject        string
	KeycloakUserID string
	Email          string
	EmailVerified  bool
	DisplayName    string
	LastSeenAt     time.Time
	CreatedAt      time.Time
}

// PublicID returns the idn_ ID.
func (i Identity) PublicID() string { return idgen.Format(idgen.Identity, i.ID) }

// LastSeenGranularity limits how often a returning identity is written: an
// unchanged identity is written again only when its last_seen_at is older.
const LastSeenGranularity = 5 * time.Minute

// Querier is the subset of pgxpool.Pool and pgx.Tx the reads need.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Upsert records p on first verified contact and refreshes its email and
// name afterwards. A principal without a verified email is not recorded:
// ok is false and nothing is written. The first insert is audited as
// identity.created in the same transaction.
func Upsert(ctx context.Context, tx pgx.Tx, p Principal, correlationID string) (ident Identity, ok bool, err error) {
	if !p.EmailVerified {
		return Identity{}, false, nil
	}
	id, err := idgen.NewV7()
	if err != nil {
		return Identity{}, false, err
	}
	var inserted bool
	err = tx.QueryRow(ctx, `
INSERT INTO identities (id, brand_id, issuer, subject, keycloak_user_id, email, email_verified, display_name, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, true, $7, now())
ON CONFLICT (brand_id, issuer, subject) DO UPDATE
   SET email = EXCLUDED.email,
       email_verified = true,
       display_name = EXCLUDED.display_name,
       last_seen_at = now(),
       updated_at = now()
 WHERE identities.email IS DISTINCT FROM EXCLUDED.email
    OR identities.email_verified IS DISTINCT FROM true
    OR identities.display_name IS DISTINCT FROM EXCLUDED.display_name
    OR identities.last_seen_at < now() - $8::interval
RETURNING id, (xmax = 0) AS inserted, email, email_verified, coalesce(display_name, ''), last_seen_at, created_at`,
		id, p.Brand.ID, p.Issuer, p.Subject, p.Subject, storedEmail(p), nullable(storedName(p)),
		fmt.Sprintf("%d seconds", int(LastSeenGranularity.Seconds())),
	).Scan(&ident.ID, &inserted, &ident.Email, &ident.EmailVerified, &ident.DisplayName, &ident.LastSeenAt, &ident.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing changed and the row was seen recently: read it.
		ident, err = getIdentity(ctx, tx, p.Brand.ID, p.Issuer, p.Subject)
		if err != nil {
			return Identity{}, false, err
		}
		return ident, true, nil
	}
	if err != nil {
		return Identity{}, false, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	ident.BrandID, ident.Issuer, ident.Subject, ident.KeycloakUserID = p.Brand.ID, p.Issuer, p.Subject, p.Subject
	if inserted {
		if _, err := audit.WriteBrand(ctx, tx, p.Brand.ID, audit.Entry{
			Actor:         events.Actor{Type: actorType(p.Kind), ID: ident.PublicID()},
			Action:        "identity.created",
			Outcome:       audit.OutcomeSuccess,
			CorrelationID: correlationID,
			Details:       map[string]any{"identity_id": ident.PublicID(), "kind": string(p.Kind)},
		}); err != nil {
			return Identity{}, false, err
		}
	}
	return ident, true, nil
}

func storedEmail(p Principal) string { return clip(strings.TrimSpace(p.Email), 320) }
func storedName(p Principal) string  { return clip(p.Name, 200) }

// NeedsUpsert reports whether Upsert would change the stored identity i of
// p: the email, the verified flag or the display name differ, or i was last
// seen LastSeenGranularity or more before now. Callers read the identity
// first and skip the write when it returns false.
func NeedsUpsert(i Identity, p Principal, now time.Time) bool {
	return i.Email != storedEmail(p) || i.EmailVerified != p.EmailVerified || i.DisplayName != storedName(p) ||
		now.Sub(i.LastSeenAt) >= LastSeenGranularity
}

func actorType(k Kind) events.ActorType {
	switch k {
	case KindStaff:
		return events.ActorStaff
	case KindService:
		return events.ActorService
	}
	return events.ActorUser
}

func getIdentity(ctx context.Context, db Querier, brand idgen.UUID, issuer, subject string) (Identity, error) {
	var (
		i       Identity
		kcUser  *string
		email   *string
		display *string
	)
	err := db.QueryRow(ctx, `SELECT id, brand_id, issuer, subject, keycloak_user_id, email, email_verified, display_name, last_seen_at, created_at
FROM identities WHERE brand_id = $1 AND issuer = $2 AND subject = $3`, brand, issuer, subject).
		Scan(&i.ID, &i.BrandID, &i.Issuer, &i.Subject, &kcUser, &email, &i.EmailVerified, &display, &i.LastSeenAt, &i.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Identity{}, zserrors.New(zserrors.CodeNotFound, "")
		}
		return Identity{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	if kcUser != nil {
		i.KeycloakUserID = *kcUser
	}
	if email != nil {
		i.Email = *email
	}
	if display != nil {
		i.DisplayName = *display
	}
	return i, nil
}

// Get returns the identity of a principal, or not_found.
func Get(ctx context.Context, db Querier, brand idgen.UUID, issuer, subject string) (Identity, error) {
	return getIdentity(ctx, db, brand, issuer, subject)
}

// StaffScope is the authority a staff grant gives.
type StaffScope string

// Staff scopes.
const (
	StaffOperator   StaffScope = "operator"
	StaffActivation StaffScope = "activation"
	StaffBilling    StaffScope = "billing"
	StaffSupport    StaffScope = "support"
	StaffClosure    StaffScope = "closure"
)

// Valid reports whether s is a known scope.
func (s StaffScope) Valid() bool {
	switch s {
	case StaffOperator, StaffActivation, StaffBilling, StaffSupport, StaffClosure:
		return true
	}
	return false
}

// StaffGrant is a row of staff_grants.
type StaffGrant struct {
	ID        idgen.UUID
	BrandID   idgen.UUID
	Issuer    string
	Subject   string
	Scope     StaffScope
	Status    string
	GrantedBy string
	Reason    string
	CreatedAt time.Time
}

// PublicID returns the grt_ ID.
func (g StaffGrant) PublicID() string { return idgen.Format(idgen.Grant, g.ID) }

// HasStaffGrant reports whether an active grant with scope exists for the
// principal in its own brand. Grants of other brands never count.
func HasStaffGrant(ctx context.Context, db Querier, p Principal, scope StaffScope) (bool, error) {
	if p.Kind != KindStaff || p.Brand.ID.IsNil() || !scope.Valid() {
		return false, nil
	}
	var ok bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM staff_grants
WHERE brand_id = $1 AND issuer = $2 AND subject = $3 AND scope = $4 AND status = 'active')`,
		p.Brand.ID, p.Issuer, p.Subject, string(scope)).Scan(&ok)
	if err != nil {
		return false, zserrors.Wrap(zserrors.CodeAuthenticationUnavailable, "", err)
	}
	return ok, nil
}

// AddStaffGrant creates an active grant and audits it as staff.grant.added.
// A second active grant with the same brand, issuer, subject and scope is a
// conflict.
func AddStaffGrant(ctx context.Context, tx pgx.Tx, g StaffGrant, actor events.Actor, correlationID string) (StaffGrant, error) {
	var problems []string
	if g.BrandID.IsNil() {
		problems = append(problems, "brand is required")
	}
	if !g.Scope.Valid() {
		problems = append(problems, "scope must be operator, activation, billing or support")
	}
	if g.Issuer == "" || len(g.Issuer) > 1000 {
		problems = append(problems, "issuer is required")
	}
	if g.Subject == "" || len(g.Subject) > 255 {
		problems = append(problems, "subject is required")
	}
	if strings.TrimSpace(g.GrantedBy) == "" || len(g.GrantedBy) > 255 {
		problems = append(problems, "granted_by is required")
	}
	if strings.TrimSpace(g.Reason) == "" || utf8.RuneCountInString(g.Reason) > 1000 {
		problems = append(problems, "a reason is required")
	}
	if len(problems) > 0 {
		return StaffGrant{}, zserrors.New(zserrors.CodeValidation, "Invalid staff grant: "+strings.Join(problems, "; ")+".")
	}
	id, err := idgen.NewV7()
	if err != nil {
		return StaffGrant{}, err
	}
	g.ID, g.Status = id, "active"
	err = tx.QueryRow(ctx, `INSERT INTO staff_grants (id, brand_id, issuer, subject, scope, granted_by, reason)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		g.ID, g.BrandID, g.Issuer, g.Subject, string(g.Scope), g.GrantedBy, g.Reason).Scan(&g.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return StaffGrant{}, zserrors.New(zserrors.CodeConflict, "An active grant with this scope already exists.")
		}
		return StaffGrant{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	if _, err := audit.WriteBrand(ctx, tx, g.BrandID, audit.Entry{
		Actor: actor, Action: "staff.grant.added", Reason: g.Reason, Outcome: audit.OutcomeSuccess,
		CorrelationID: correlationID,
		Details: map[string]any{
			"grant_id": g.PublicID(), "issuer": g.Issuer, "subject": g.Subject,
			"scope": string(g.Scope), "granted_by": g.GrantedBy,
		},
	}); err != nil {
		return StaffGrant{}, err
	}
	return g, nil
}

// RevokeStaffGrant sets the active grant of brand, issuer, subject and scope
// to revoked and audits it as staff.grant.revoked. Verification already
// requires an active grant, so the subject loses the scope at once. No
// active grant is not_found.
func RevokeStaffGrant(ctx context.Context, tx pgx.Tx, g StaffGrant, revokedBy string, actor events.Actor, correlationID string) (StaffGrant, error) {
	var problems []string
	if g.BrandID.IsNil() {
		problems = append(problems, "brand is required")
	}
	if !g.Scope.Valid() {
		problems = append(problems, "scope must be operator, activation, billing or support")
	}
	if g.Issuer == "" || g.Subject == "" {
		problems = append(problems, "issuer and subject are required")
	}
	if strings.TrimSpace(revokedBy) == "" {
		problems = append(problems, "the operator is required")
	}
	if strings.TrimSpace(g.Reason) == "" || utf8.RuneCountInString(g.Reason) > 1000 {
		problems = append(problems, "a reason is required")
	}
	if len(problems) > 0 {
		return StaffGrant{}, zserrors.New(zserrors.CodeValidation, "Invalid staff grant revocation: "+strings.Join(problems, "; ")+".")
	}
	reason := g.Reason
	var scope string
	err := tx.QueryRow(ctx, `UPDATE staff_grants SET status = 'revoked', updated_at = now()
WHERE brand_id = $1 AND issuer = $2 AND subject = $3 AND scope = $4 AND status = 'active'
RETURNING id, brand_id, issuer, subject, scope, status, granted_by, reason, created_at`,
		g.BrandID, g.Issuer, g.Subject, string(g.Scope)).
		Scan(&g.ID, &g.BrandID, &g.Issuer, &g.Subject, &scope, &g.Status, &g.GrantedBy, &g.Reason, &g.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffGrant{}, zserrors.New(zserrors.CodeNotFound, "No active grant with this scope exists.")
	}
	if err != nil {
		return StaffGrant{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	g.Scope = StaffScope(scope)
	if _, err := audit.WriteBrand(ctx, tx, g.BrandID, audit.Entry{
		Actor: actor, Action: "staff.grant.revoked", Reason: reason, Outcome: audit.OutcomeSuccess,
		CorrelationID: correlationID,
		Details: map[string]any{
			"grant_id": g.PublicID(), "issuer": g.Issuer, "subject": g.Subject,
			"scope": string(g.Scope), "revoked_by": revokedBy,
		},
	}); err != nil {
		return StaffGrant{}, err
	}
	return g, nil
}

// ListStaffGrants returns the grants of a brand, newest first.
func ListStaffGrants(ctx context.Context, db Querier, brand idgen.UUID) ([]StaffGrant, error) {
	rows, err := db.Query(ctx, `SELECT id, brand_id, issuer, subject, scope, status, granted_by, reason, created_at
FROM staff_grants WHERE brand_id = $1 ORDER BY created_at DESC, id DESC`, brand)
	if err != nil {
		return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	defer rows.Close()
	var out []StaffGrant
	for rows.Next() {
		var g StaffGrant
		var scope string
		if err := rows.Scan(&g.ID, &g.BrandID, &g.Issuer, &g.Subject, &scope, &g.Status, &g.GrantedBy, &g.Reason, &g.CreatedAt); err != nil {
			return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
		}
		g.Scope = StaffScope(scope)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return out, nil
}
