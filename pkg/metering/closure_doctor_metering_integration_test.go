//go:build integration

package metering_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/doctor"
	"github.com/zsoftly/zs-core/internal/enginetest"
	"github.com/zsoftly/zs-core/pkg/metering"
)

// An unconfirmed usage send is reported with its tenant and its reason, so
// the operator knows staff must resolve it against Kill Bill.
func TestClosureFindingsReportAnUnconfirmedUsageSend(t *testing.T) {
	e, _, tenantID := setupOneRollup(t, "zs-doctor-unconfirmed")
	ctx := context.Background()
	e.fake.LoseAnswers("/1.0/kb/usages", 1)
	if report, err := e.svc.Forward(ctx); err != nil || report.Unconfirmed != 1 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	fs, err := doctor.ClosureFindings(ctx, e.db.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fs {
		if f.Check == "usage_unconfirmed" && f.Status == doctor.Warn && strings.Contains(f.Message, "usage_send_unconfirmed") && strings.Contains(f.Message, "tnt_") {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want an unconfirmed usage warning for %s", fs, tenantID)
	}
}

// A claim that expired long ago and was never released or resolved is a
// stranded claim: the forwarder that took it is gone.
func TestClosureFindingsReportAStrandedClaim(t *testing.T) {
	e, _, tenantID := setupOneRollup(t, "zs-doctor-stranded")
	ctx := context.Background()
	e.svc.ClaimTTL = time.Second
	sc := scopeOf(t, e, tenantID)
	if n, err := e.svc.RollUp(ctx, sc); err != nil || n != 1 {
		t.Fatalf("rollup = %d %v", n, err)
	}
	if due, err := e.svc.ClaimDue(ctx, &sc); err != nil || len(due) != 1 {
		t.Fatalf("claim = %d %v", len(due), err)
	}
	// A claim that has just been taken is not stranded, and the same claim
	// long after it expired is.
	fs, err := doctor.ClosureFindings(ctx, e.db.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Check == "usage_claims_stranded" {
			t.Fatalf("a live claim was reported as stranded: %+v", f)
		}
	}
	fs, err = doctor.ClosureFindings(ctx, e.db.Pool, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fs {
		if f.Check == "usage_claims_stranded" && f.Status == doctor.Warn && strings.Contains(f.Message, "tnt_") {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want a stranded claim warning", fs)
	}
}

// A rejected rollup holds the drain of a closing tenant, so it is reported: a
// tenant cannot wait at cancelling with nothing to say why.
func TestClosureFindingsReportARejectedRollup(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	b := e.brand("zs-doctor-rejected")
	tn := e.tenant(b, "Doctor Rejected")
	late := e.subscribe(b, tn, enginetest.PlanMetered)
	e.fake.SetSubscriptionStart(b.Slug, late, e.clock.Now().Add(time.Hour))
	if _, _, err := e.record(tn.Scope(), metering.Event{Meter: "api-calls", EventID: "early", QuantityMicro: 4_000_000}); err != nil {
		t.Fatal(err)
	}
	if report, err := e.svc.Forward(ctx); err != nil || report.Rejected != 1 {
		t.Fatalf("forward = %+v %v", report, err)
	}
	e.closing(tn)
	if done, err := e.svc.Drained(ctx, tn.Scope()); err != nil || done {
		t.Fatalf("drained with a rejected rollup = %v %v, want false", done, err)
	}
	fs, err := doctor.ClosureFindings(ctx, e.db.Pool, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fs {
		if f.Check == "usage_rejected" && f.Status == doctor.Warn && strings.Contains(f.Message, tn.PublicID()) {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want a rejected usage warning for %s", fs, tn.PublicID())
	}
}
