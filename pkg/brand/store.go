package brand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Statuses of a brand row.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Record is a row of the brands table. It never holds a secret value.
type Record struct {
	ID                   idgen.UUID
	Slug                 string
	Name                 string
	Status               string
	KeycloakRealm        string
	KillBillTenantID     string
	KillBillAPIKey       string
	KillBillAPISecretRef string
	Manifest             *Manifest
	ManifestDigest       string
	AppliedAt            time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// PublicID returns the brd_ ID.
func (r Record) PublicID() string { return idgen.Format(idgen.Brand, r.ID) }

// Querier is the subset of pgxpool.Pool and pgx.Tx the reads need.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrNotFound is returned when no brand matches.
var ErrNotFound = zserrors.New(zserrors.CodeNotFound, "The brand was not found.")

const selectColumns = `id, slug, name, status, keycloak_realm, killbill_tenant_id::text, killbill_api_key,
       killbill_api_secret_ref, manifest, manifest_digest, applied_at, created_at, updated_at`

func scanRecord(row pgx.Row) (Record, error) {
	var (
		r         Record
		tenantID  *string
		manifest  []byte
		appliedAt *time.Time
	)
	if err := row.Scan(&r.ID, &r.Slug, &r.Name, &r.Status, &r.KeycloakRealm, &tenantID, &r.KillBillAPIKey,
		&r.KillBillAPISecretRef, &manifest, &r.ManifestDigest, &appliedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return Record{}, err
	}
	if tenantID != nil {
		r.KillBillTenantID = *tenantID
	}
	if appliedAt != nil {
		r.AppliedAt = appliedAt.UTC()
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return r, &manifestDecodeError{slug: r.Slug, err: err}
	}
	r.Manifest = &m
	return r, nil
}

// manifestDecodeError is a brand row whose stored manifest cannot be decoded.
type manifestDecodeError struct {
	slug string
	err  error
}

func (e *manifestDecodeError) Error() string {
	return fmt.Sprintf("decode stored manifest of brand %s: %v", e.slug, e.err)
}

func (e *manifestDecodeError) Unwrap() error { return e.err }

func getOne(ctx context.Context, db Querier, where string, arg any) (Record, error) {
	r, err := scanRecord(db.QueryRow(ctx, `SELECT `+selectColumns+` FROM brands WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return r, nil
}

// Get returns the brand with id.
func Get(ctx context.Context, db Querier, id idgen.UUID) (Record, error) {
	return getOne(ctx, db, "id = $1", id)
}

// GetBySlug returns the brand with slug.
func GetBySlug(ctx context.Context, db Querier, slug string) (Record, error) {
	return getOne(ctx, db, "slug = $1", slug)
}

// List returns every brand ordered by slug. A row whose stored manifest
// cannot be decoded is logged with its slug and left out, so one bad row
// never hides the other brands; token verification then treats that brand
// as inactive. log may be nil.
func List(ctx context.Context, db Querier, log *slog.Logger) ([]Record, error) {
	rows, err := db.Query(ctx, `SELECT `+selectColumns+` FROM brands ORDER BY slug`)
	if err != nil {
		return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		var decodeErr *manifestDecodeError
		if errors.As(err, &decodeErr) {
			if log != nil {
				log.Error("stored brand manifest cannot be decoded; the brand is inactive until it is applied again",
					"brand", decodeErr.slug, "error", decodeErr.err.Error())
			}
			continue
		}
		if err != nil {
			return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return out, nil
}

// insertRecord creates the brand row. The manifest stored is the one being
// applied; applied_at stays NULL until an apply completes.
func insertRecord(ctx context.Context, tx pgx.Tx, r Record) error {
	manifest, err := json.Marshal(r.Manifest)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO brands (id, slug, name, status, keycloak_realm, killbill_api_key,
    killbill_api_secret_ref, manifest, manifest_digest)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		r.ID, r.Slug, r.Name, r.Status, r.KeycloakRealm, r.KillBillAPIKey, r.KillBillAPISecretRef, manifest, r.ManifestDigest)
	return err
}

// updateIdentityFields updates the fields a manifest controls directly.
func updateIdentityFields(ctx context.Context, tx pgx.Tx, id idgen.UUID, name, secretRef string) error {
	_, err := tx.Exec(ctx, `UPDATE brands SET name = $2, killbill_api_secret_ref = $3, updated_at = now() WHERE id = $1`,
		id, name, secretRef)
	return err
}

func setKillBillTenant(ctx context.Context, tx pgx.Tx, id idgen.UUID, tenantID string) error {
	_, err := tx.Exec(ctx, `UPDATE brands SET killbill_tenant_id = $2::uuid, updated_at = now() WHERE id = $1`, id, tenantID)
	return err
}

func recordApplied(ctx context.Context, tx pgx.Tx, id idgen.UUID, m *Manifest, digest string) error {
	manifest, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE brands SET manifest = $2, manifest_digest = $3, applied_at = now(), updated_at = now() WHERE id = $1`,
		id, manifest, digest)
	return err
}
