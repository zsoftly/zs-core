package metering

import (
	"context"

	"github.com/zsoftly/zs-core/pkg/scope"
)

// RollUp runs one rollup pass of a tenant without sending anything.
func (s *Service) RollUp(ctx context.Context, sc scope.Scope) (int, error) {
	return s.rollUp(ctx, sc)
}
