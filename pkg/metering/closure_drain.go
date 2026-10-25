package metering

import (
	"context"

	"github.com/zsoftly/zs-core/pkg/scope"
)

// maxDrainRounds bounds how many batches of events one Drained call rolls up.
const maxDrainRounds = 50

// Drained is what a closure asks before it invoices: it rolls up the tenant's
// events and sends every rollup that is due to Kill Bill, and it answers true
// only when nothing of the tenant's usage is left on this side. It is the
// ClosureDrain of the tenant package.
//
// Drained is all of these:
//   - no event accepted under dispatch protocol 1 is in no rollup;
//   - no rollup of protocol 1 is pending, claimed, started or in flight, and
//     none is rejected: an unconfirmed send is a pending one that staff must
//     resolve against Kill Bill, so the tenant is not drained until they do;
//   - no wallet reservation is held, each being settled or released.
//
// Usage accepted before protocol 1 is read-only and is not rolled up, so it
// does not hold the drain: the closure review takes it to a person, and
// completion waits for that.
func (s *Service) Drained(ctx context.Context, sc scope.Scope) (bool, error) {
	for round := 0; round < maxDrainRounds; round++ {
		report, err := s.ForwardTenant(ctx, sc)
		if err != nil {
			return false, err
		}
		if report.RolledUp == 0 {
			break
		}
	}
	args, err := sc.Bind()
	if err != nil {
		return false, err
	}
	var left bool
	if err := s.DB.QueryRow(ctx, `SELECT
    EXISTS (SELECT 1 FROM usage_events WHERE brand_id = $1 AND tenant_id = $2 AND acceptance_protocol = 1 AND rollup_id IS NULL AND reservation_id IS NULL)
 OR EXISTS (SELECT 1 FROM usage_rollups u WHERE u.brand_id = $1 AND u.tenant_id = $2 AND u.send_protocol = 1 AND u.status IN ('pending', 'rejected')
    AND NOT EXISTS (SELECT 1 FROM usage_rollup_resolutions x WHERE x.rollup_id = u.id))
 OR EXISTS (SELECT 1 FROM reservations WHERE brand_id = $1 AND tenant_id = $2 AND status = 'held')`, args...).Scan(&left); err != nil {
		return false, unavailable(err)
	}
	return !left, nil
}
