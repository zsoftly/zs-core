// Package scope defines the brand and tenant scope that every tenant-owned
// query in ZSoftly Core carries.
//
// A brand is the top-level boundary and a tenant always belongs to exactly one
// brand. Every row that belongs to a tenant stores both IDs and every query
// filters on both. A Scope is valid only when both are set. Bind is the one
// way to turn a Scope into query arguments, and it refuses an incomplete
// scope, so a query cannot run with a missing filter by accident.
package scope

import (
	"context"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Scope is the brand and tenant a call acts within.
type Scope struct {
	Brand  idgen.UUID
	Tenant idgen.UUID
}

// ErrMissing is returned when the brand or the tenant is missing.
var ErrMissing = zserrors.New(zserrors.CodeScopeRequired, "")

// ErrInvalid is returned when a brand or tenant public ID is malformed.
var ErrInvalid = zserrors.New(zserrors.CodeScopeInvalid, "")

// New returns a validated scope for the given UUIDs.
func New(brand, tenant idgen.UUID) (Scope, error) {
	s := Scope{Brand: brand, Tenant: tenant}
	if err := s.Validate(); err != nil {
		return Scope{}, err
	}
	return s, nil
}

// Parse builds a scope from public IDs such as "brd_..." and "tnt_...".
// Either value being empty is ErrMissing; a malformed or wrongly prefixed
// value is ErrInvalid.
func Parse(brandID, tenantID string) (Scope, error) {
	if brandID == "" || tenantID == "" {
		return Scope{}, ErrMissing
	}
	brand, err := idgen.ParseAs(idgen.Brand, brandID)
	if err != nil {
		return Scope{}, ErrInvalid
	}
	tenant, err := idgen.ParseAs(idgen.Tenant, tenantID)
	if err != nil {
		return Scope{}, ErrInvalid
	}
	return New(brand, tenant)
}

// Validate returns ErrMissing unless both the brand and the tenant are set.
func (s Scope) Validate() error {
	if s.Brand.IsNil() || s.Tenant.IsNil() {
		return ErrMissing
	}
	return nil
}

// BrandID returns the public brand ID.
func (s Scope) BrandID() string { return idgen.Format(idgen.Brand, s.Brand) }

// TenantID returns the public tenant ID.
func (s Scope) TenantID() string { return idgen.Format(idgen.Tenant, s.Tenant) }

// Bind validates the scope and returns query arguments with the brand as $1
// and the tenant as $2, followed by args. SQL that uses Bind must filter on
// brand_id = $1 AND tenant_id = $2.
func (s Scope) Bind(args ...any) ([]any, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	bound := make([]any, 0, len(args)+2)
	bound = append(bound, s.Brand, s.Tenant)
	return append(bound, args...), nil
}

type contextKey struct{}

// WithContext returns a context carrying s.
func WithContext(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

// WithBrand returns a context carrying a brand-level scope: the brand is set
// and the tenant is not. Tenant-owned queries refuse such a scope, so a
// brand-level route cannot reach tenant rows by accident.
func WithBrand(ctx context.Context, brand idgen.UUID) context.Context {
	return WithContext(ctx, Scope{Brand: brand})
}

// BrandFromContext returns the brand of the scope in ctx, whether or not a
// tenant is set. It returns ErrMissing when no brand is present.
func BrandFromContext(ctx context.Context) (idgen.UUID, error) {
	s, ok := ctx.Value(contextKey{}).(Scope)
	if !ok || s.Brand.IsNil() {
		return idgen.Nil, ErrMissing
	}
	return s.Brand, nil
}

// FromContext returns the scope stored in ctx. It returns ErrMissing when no
// complete scope is present.
func FromContext(ctx context.Context) (Scope, error) {
	s, ok := ctx.Value(contextKey{}).(Scope)
	if !ok {
		return Scope{}, ErrMissing
	}
	if err := s.Validate(); err != nil {
		return Scope{}, err
	}
	return s, nil
}
