package metering

import (
	"context"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/scope"
)

// RollUp runs one rollup pass of a tenant without sending anything.
func (s *Service) RollUp(ctx context.Context, sc scope.Scope) (int, error) {
	return s.rollUp(ctx, sc)
}

// Claimed is a rollup a test claimed.
type Claimed struct{ ro rollup }

// ID returns the rollup ID.
func (c Claimed) ID() idgen.UUID { return c.ro.id }

// ClaimDue claims one batch without sending anything.
func (s *Service) ClaimDue(ctx context.Context, only *scope.Scope) ([]Claimed, error) {
	due, err := s.claimDue(ctx, only)
	out := make([]Claimed, len(due))
	for i, ro := range due {
		out[i] = Claimed{ro}
	}
	return out, err
}

// RenewClaim renews the claim of c.
func (s *Service) RenewClaim(ctx context.Context, c Claimed) (bool, error) {
	return s.renewClaim(ctx, c.ro)
}

// BindTarget binds the target of the claimed rollup the way send does.
func (s *Service) BindTarget(ctx context.Context, c Claimed, subscription string) (bool, error) {
	acct, err := s.Billing.TenantAccount(ctx, s.DB, c.ro.sc)
	if err != nil {
		return false, err
	}
	var t killbill.UsageTarget
	if t, err = acct.UsageTarget(ctx, subscription); err != nil {
		return false, err
	}
	return s.bindTarget(ctx, c.ro, subscription, t)
}
