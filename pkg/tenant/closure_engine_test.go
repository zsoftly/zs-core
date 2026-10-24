package tenant

import (
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
)

// Only a subscription that is ACTIVE, BLOCKED or PENDING can be cancelled by
// the closure step. One that is CANCELLED but does not satisfy the receipt, with
// a day after today or none that can be read, cannot be fixed by cancelling it
// again and is a case for a person.
func TestClosureCancelNeedsAPerson(t *testing.T) {
	for name, tc := range map[string]struct {
		failing []killbill.CancelFailure
		want    bool
	}{
		"nothing failing":            {nil, false},
		"active and blocked":         {[]killbill.CancelFailure{{State: "ACTIVE"}, {State: "BLOCKED"}, {State: "PENDING"}}, false},
		"cancelled after today":      {[]killbill.CancelFailure{{State: "CANCELLED", Day: "2026-12-31"}}, true},
		"cancelled with no day":      {[]killbill.CancelFailure{{State: "CANCELLED"}}, true},
		"one of each":                {[]killbill.CancelFailure{{State: "ACTIVE"}, {State: "CANCELLED"}}, true},
		"a state it does not expect": {[]killbill.CancelFailure{{State: "EXPIRED"}}, true},
	} {
		if got := cancelNeedsAPerson(killbill.CancelReceipt{Failing: tc.failing}); got != tc.want {
			t.Errorf("%s = %v, want %v", name, got, tc.want)
		}
	}
}
