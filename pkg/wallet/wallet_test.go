package wallet

import (
	"errors"
	"strings"
	"testing"
	"time"

	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/idgen"
	"github.com/zsoftly/zs-core/pkg/metering"
	"github.com/zsoftly/zs-core/pkg/outbox"
	"github.com/zsoftly/zs-core/pkg/scope"
	"github.com/zsoftly/zs-core/pkg/tenant"
)

// The event types with one internal consumer are the ones generic outbox
// readers skip.
func TestInternalEventTypes(t *testing.T) {
	for _, typ := range []string{EventSettlementRequested, metering.EventUsageRecorded, tenant.MirrorEventType} {
		if !outbox.IsInternal(typ) {
			t.Errorf("%s is not internal", typ)
		}
	}
	for _, typ := range []string{EventReservationSettled, EventLowBalance, EventReconciliationMismatch} {
		if outbox.IsInternal(typ) {
			t.Errorf("%s is internal", typ)
		}
	}
}

func TestSettlementCheck(t *testing.T) {
	r := Reservation{ExternalRef: "job", Attempt: 2, AmountMinor: 100}
	for name, st := range map[string]Settlement{
		"empty":         {},
		"both":          {CreditMinor: 1, Usage: &UsageSettlement{Unit: "u", QuantityMicro: 1}},
		"negative":      {CreditMinor: -1},
		"above maximum": {CreditMinor: MaxAmountMinor + 1},
		"bad unit":      {Usage: &UsageSettlement{Unit: "a b", QuantityMicro: 1}},
		"no quantity":   {Usage: &UsageSettlement{Unit: "u"}},
		"bad sub":       {Usage: &UsageSettlement{Unit: "u", QuantityMicro: 1, Subscription: "nope"}},
		"huge quantity": {Usage: &UsageSettlement{Unit: "u", QuantityMicro: 1 << 62}},
	} {
		if _, err := st.check(r); !zserrors.IsCode(err, zserrors.CodeValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	st, err := Settlement{Usage: &UsageSettlement{Unit: "api-call", QuantityMicro: 5}}.check(r)
	if err != nil || st.Usage.EventID != "rsv:job:2" {
		t.Fatalf("default event ID = %+v %v", st.Usage, err)
	}
	if fingerprintOf(st) == fingerprintOf(Settlement{CreditMinor: 5}) {
		t.Fatal("two settlements share a fingerprint")
	}
}

func TestKeysLeasesAndShortfall(t *testing.T) {
	for _, ok := range []string{"job-1", "a", "ci:run.42_x"} {
		if !ValidRef(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-x", "a b", strings.Repeat("a", 201), "a/b"} {
		if ValidRef(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := checkKey("job", -1); err == nil {
		t.Fatal("negative attempt accepted")
	}
	if d, err := checkLease(0); err != nil || d != DefaultLease {
		t.Fatalf("default lease = %v %v", d, err)
	}
	for _, bad := range []time.Duration{time.Second, 25 * time.Hour} {
		if _, err := checkLease(bad); err == nil {
			t.Errorf("lease %v accepted", bad)
		}
	}
	err := insufficient(250)
	if short, ok := Shortfall(err); !ok || short != 250 || !zserrors.IsCode(err, zserrors.CodeInsufficientCredit) || !strings.Contains(err.Error(), "250") {
		t.Fatalf("shortfall = %d %v %v", short, ok, err)
	}
	if _, ok := Shortfall(errors.New("other")); ok {
		t.Fatal("shortfall of another error")
	}
}

func TestDerivedReferences(t *testing.T) {
	sc := scope.Scope{Brand: idgen.MustNewV7(), Tenant: idgen.MustNewV7()}
	r := Reservation{id: idgen.MustNewV7()}
	r.ID = idgen.Format(idgen.Reservation, r.id)
	// Kill Bill's keys come from the caller's keys only: a reservation made
	// again under the same ref and attempt, with another ID, reaches the
	// same usage record and charge.
	if a, b := UsageTrackingID(sc, "job", 1), UsageTrackingID(sc, "job", 1); a != b || !strings.HasPrefix(a, "zs-rsv-") || len(a) != 47 {
		t.Fatalf("tracking IDs = %s %s", a, b)
	}
	for _, other := range []string{UsageTrackingID(sc, "job", 2), UsageTrackingID(sc, "job-2", 1),
		UsageTrackingID(scope.Scope{Brand: sc.Brand, Tenant: idgen.MustNewV7()}, "job", 1)} {
		if other == UsageTrackingID(sc, "job", 1) {
			t.Fatal("two keys share a tracking ID")
		}
	}
	if d := ChargeDescription(sc, "job", 1); !strings.HasPrefix(d, ChargeDescriptionPrefix) || d != ChargeDescription(sc, "job", 1) ||
		d == ChargeDescription(sc, "job", 2) || strings.Contains(d, r.ID) {
		t.Fatalf("charge description = %s", d)
	}
	if Available(1000, 300, 200, 50) != 550 || Available(100, 0, 300, 100) != -100 {
		t.Fatal("available")
	}
	at := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	gotAt, gotID, err := decodeCursor(encodeCursor(at, r.id))
	if err != nil || !gotAt.Equal(at) || gotID != r.id {
		t.Fatalf("cursor = %v %v %v", gotAt, gotID, err)
	}
	if _, _, err := decodeCursor("zz"); err == nil {
		t.Fatal("bad cursor accepted")
	}
}
