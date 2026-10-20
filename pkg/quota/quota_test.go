package quota

import (
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/billing"
	"github.com/zsoftly/zs-core/pkg/brand"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
)

func state() billing.EntitlementState {
	return billing.EntitlementState{
		Active:  []killbill.Subscription{{ProductName: "core", PlanName: "core-monthly", State: "ACTIVE"}},
		Catalog: killbill.Catalog{Products: []killbill.CatalogProduct{{Name: "core", Plans: []killbill.CatalogPlan{{Name: "core-monthly"}}}}},
	}
}

func TestAnswer(t *testing.T) {
	st := state()
	if got := answer(st, "core", effective{}, 0, false); !got.Allowed || got.Limit != nil || got.Plan != "core-monthly" {
		t.Fatalf("no quota = %+v", got)
	}
	if got := answer(st, "other", effective{}, 0, false); got.Allowed || got.Reason != billing.ReasonNotInPlan {
		t.Fatalf("not granted = %+v", got)
	}
	e := effective{found: true, plan: "core-monthly", limit: 3, window: brand.WindowMonth, meter: "m"}
	if got := answer(st, "other", e, 2, true); !got.Allowed || *got.Used != 2 || *got.Remaining != 1 || got.Window != brand.WindowMonth {
		t.Fatalf("within = %+v", got)
	}
	if got := answer(st, "other", e, 4, true); got.Allowed || got.Reason != billing.ReasonQuotaExceeded || *got.Remaining != 0 {
		t.Fatalf("over = %+v", got)
	}
	none := effective{found: true, plan: "core-monthly", limit: 0, window: brand.WindowNone}
	if got := answer(st, "seats", none, 0, false); got.Allowed || got.Used != nil {
		t.Fatalf("zero limit without a counter = %+v", got)
	}
}

func TestDenied(t *testing.T) {
	for reason, code := range map[string]zserrors.Code{
		billing.ReasonBillingUnavailable:   zserrors.CodeBillingUnavailable,
		billing.ReasonWalletOnHold:         zserrors.CodeWalletOnHold,
		billing.ReasonTenantNotFound:       zserrors.CodeTenantNotFound,
		billing.ReasonNoActiveSubscription: zserrors.CodeQuotaExceeded,
	} {
		if err := denied(reason); !zserrors.IsCode(err, code) {
			t.Errorf("%s: %v", reason, err)
		}
	}
}

func TestWindowsLeasesAndDrift(t *testing.T) {
	now := time.Date(2026, 10, 20, 15, 4, 5, 0, time.UTC)
	if start, end := windowStart(brand.WindowDay, now); !start.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) || !end.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("day = %v %v", start, end)
	}
	if start, end := windowStart(brand.WindowMonth, now); !start.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month = %v %v", start, end)
	}
	if start, _ := windowStart(brand.WindowConcurrent, now); !start.Equal(concurrentWindow) {
		t.Fatalf("concurrent = %v", start)
	}
	if d, err := checkLease(0); err != nil || d != DefaultLease {
		t.Fatalf("default lease = %v %v", d, err)
	}
	for _, bad := range []time.Duration{time.Second, 25 * time.Hour} {
		if _, err := checkLease(bad); err == nil {
			t.Errorf("lease %v accepted", bad)
		}
	}
	if u := (use{counter: 2, summary: 5}); u.used() != 5 || !u.drifted() {
		t.Fatalf("drift = %+v", u)
	}
	if u := (use{counter: 5, summary: 2}); u.used() != 5 || u.drifted() {
		t.Fatalf("consumed ahead of usage = %+v", u)
	}
	if (Call{Actor: events.Actor{Type: events.ActorStaff, ID: "s"}, CorrelationID: "c", Staff: true}).validate() == nil {
		t.Fatal("a staff call without a reason is valid")
	}
	if got := (Call{Actor: events.Actor{Type: events.ActorService, ID: "x"}}).holder(); got != "service:x" {
		t.Fatalf("holder = %s", got)
	}
}
