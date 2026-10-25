//go:build integration

package tenant_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

func closureFindings(t *testing.T, e *env) []doctor.Finding {
	t.Helper()
	fs, err := doctor.ClosureFindings(context.Background(), e.db.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func hasFinding(fs []doctor.Finding, check string, status doctor.Status, contains ...string) bool {
	for _, f := range fs {
		if f.Check != check || f.Status != status {
			continue
		}
		ok := true
		for _, c := range contains {
			ok = ok && strings.Contains(f.Message, c)
		}
		if ok {
			return true
		}
	}
	return false
}

// With nothing closing the closure checks are green.
func TestClosureFindingsAreGreenWhenNothingIsClosing(t *testing.T) {
	e := newEnv(t)
	fs := closureFindings(t, e)
	if len(fs) != 1 || fs[0].Status != doctor.OK || fs[0].Check != "closures" {
		t.Fatalf("findings = %+v, want one OK closures finding", fs)
	}
}

// A closure that has not finished after an hour is reported with its tenant,
// where it is and how long it has been closing, and a fresh one is not.
func TestClosureFindingsReportAClosureThatIsStuck(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	fresh := settlementAt(t, e, "cancelling", false, false)
	stale := settlementAt(t, e, "blocking", true, false)
	if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET created_at = now() - interval '3 hours', attempts = 4, last_error = 'step_timeout' WHERE tenant_id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	fs := closureFindings(t, e)
	if !hasFinding(fs, "closure_stuck", doctor.Warn, stale.PublicID(), "blocking", "3h", "step_timeout") {
		t.Fatalf("findings = %+v, want a warning for the stale closure", fs)
	}
	for _, f := range fs {
		if strings.Contains(f.Message, fresh.PublicID()) {
			t.Fatalf("the closure that just started was reported: %+v", f)
		}
		if f.Status == doctor.OK {
			t.Fatalf("an OK finding next to a warning: %+v", f)
		}
	}
}

// Open closure review items are reported per tenant, and are gone once
// they are resolved. Nothing in a finding discloses the usage itself.
func TestClosureFindingsReportOpenReviews(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tn := settlementAt(t, e, "blocking", true, true)
	addLegacyEvent(t, e, tn)
	if _, err := tenant.RecordClosureReviews(ctx, e.db.Pool, tn.Scope(), reviewActor, "corr-doctor"); err != nil {
		t.Fatal(err)
	}
	fs := closureFindings(t, e)
	if !hasFinding(fs, "closure_reviews", doctor.Warn, tn.PublicID(), "1 unresolved") {
		t.Fatalf("findings = %+v, want an open review warning", fs)
	}
	items, err := tenant.ListClosureReviews(ctx, e.db.Pool, tn.Scope())
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %+v %v", items, err)
	}
	if err := tenant.ResolveClosureReview(ctx, e.db.Pool, tn.Scope(), items[0].ID, reviewActor, "reviewed with finance", "corr-doctor"); err != nil {
		t.Fatal(err)
	}
	if fs := closureFindings(t, e); hasFinding(fs, "closure_reviews", doctor.Warn) {
		t.Fatalf("a resolved review is still reported: %+v", fs)
	}
}

// A kind with more findings than the limit lists the limit and says how many
// are left out, never silently.
//
// It sets doctor.ClosureFindingMax, which is global, so it must not run in
// parallel with another test of the closure findings: none of them calls
// t.Parallel, and this one must not either.
func TestClosureFindingsSayHowManyAreLeftOut(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for range 3 {
		tn := settlementAt(t, e, "cancelling", false, false)
		if _, err := e.db.Pool.Exec(ctx, `UPDATE tenant_closure_settlements SET created_at = now() - interval '3 hours' WHERE tenant_id = $1`, tn.ID); err != nil {
			t.Fatal(err)
		}
	}
	was := doctor.ClosureFindingMax
	doctor.ClosureFindingMax = 2
	t.Cleanup(func() { doctor.ClosureFindingMax = was })
	fs := closureFindings(t, e)
	listed, more := 0, false
	for _, f := range fs {
		if f.Check != "closure_stuck" {
			continue
		}
		if strings.HasPrefix(f.Message, "and 1 more") {
			more = true
		} else {
			listed++
		}
	}
	if listed != 2 || !more {
		t.Fatalf("findings = %+v, want 2 listed and an 'and 1 more' warning", fs)
	}
}
