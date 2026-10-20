package billing

import (
	"encoding/json"
	"testing"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/brand"
)

func TestMicroConversions(t *testing.T) {
	for micro, text := range map[int64]string{0: "0.000000", 1: "0.000001", 2_500_000: "2.500000", -3: "-0.000003"} {
		if got := MicroToDecimal(micro); string(got) != text {
			t.Errorf("%d = %s", micro, got)
		}
		if back, err := DecimalToMicro(json.Number(text)); err != nil || back != micro {
			t.Errorf("%s = %d %v", text, back, err)
		}
	}
	// Kill Bill keeps nine digits; Core rounds to six.
	if v, err := DecimalToMicro("8.500000500"); err != nil || v != 8_500_001 {
		t.Fatalf("rounding = %d %v", v, err)
	}
	if _, err := DecimalToMicro("99999999999999999999"); err == nil {
		t.Fatal("out of range accepted")
	}
	if _, err := DecimalToMicro("abc"); err == nil {
		t.Fatal("not a decimal accepted")
	}
}

func TestEstimateUsage(t *testing.T) {
	m := &brand.Manifest{}
	m.Billing.Currencies = []string{"CAD"}
	k := &TenantAccount{a: account{brand: brand.Record{Manifest: m}}}
	plan := killbill.CatalogPlan{Phases: []killbill.CatalogPhase{{Type: "EVERGREEN", Usages: []killbill.CatalogUsage{{Tiers: []killbill.CatalogTier{{
		Blocks: []killbill.TieredBlock{{Unit: "api-call", Size: "1000", Prices: []killbill.Price{{Currency: "CAD", Value: "1.00"}}}}}}}}}}}
	// 2500 calls are three started blocks of 1000 at 1.00.
	if v, ok := k.EstimateUsage(plan, "EVERGREEN", "api-call", 2_500_000_000); !ok || v != 300 {
		t.Fatalf("estimate = %d %v", v, ok)
	}
	if _, ok := k.EstimateUsage(plan, "TRIAL", "api-call", 1); ok {
		t.Fatal("a phase without the unit was rated")
	}
	if _, ok := k.EstimateUsage(plan, "EVERGREEN", "token", 1); ok {
		t.Fatal("another unit was rated")
	}
	if !planHasUnit(plan, "api-call") || planHasUnit(plan, "token") {
		t.Fatal("planHasUnit")
	}
}
