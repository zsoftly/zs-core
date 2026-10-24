package tenant

import (
	"errors"
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

// What Prepare does when the account is still not cancelled after its cancels.
func TestCancelVerdict(t *testing.T) {
	active := killbill.CancelReceipt{Failing: []killbill.CancelFailure{{State: "ACTIVE"}}}
	cancelled := killbill.CancelReceipt{Failing: []killbill.CancelFailure{{State: "CANCELLED", Day: "2026-12-31"}}}
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		receipt  killbill.CancelReceipt
		refused  bool
		notFound bool
		err      error
		want     string
	}{
		"still active, nothing said":   {active, false, false, nil, ""},
		"still active after a refusal": {active, true, false, nil, "conflict"},
		"not found":                    {active, false, true, nil, "conflict"},
		"cancelled for a later day":    {cancelled, false, false, nil, "conflict"},
		"a failed cancel":              {active, false, false, boom, "internal_error"},
		"a refusal wins over an error": {active, true, false, boom, "conflict"},
	} {
		err := cancelVerdict(tc.receipt, tc.refused, tc.notFound, tc.err)
		got := ""
		if err != nil {
			got = codeName(err)
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", name, got, tc.want)
		}
	}
}
