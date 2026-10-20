package brand

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/killbill"
)

func TestReferenceManifestExtensions(t *testing.T) {
	m, err := Load(referencePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if m.Billing.Meters == nil || len(*m.Billing.Meters) != 1 || (*m.Billing.Meters)[0].KillBillUnit != "api-call" {
		t.Fatalf("meters = %+v", m.Billing.Meters)
	}
	if m.Entitlements == nil || len(*m.Entitlements) != 2 {
		t.Fatalf("entitlements = %+v", m.Entitlements)
	}
	if m.Billing.LowBalance() != 500 || m.Billing.Overdraft() != 0 || m.Billing.Backfill() != 168*time.Hour || m.Billing.Future() != 5*time.Minute {
		t.Fatalf("wallet and window = %+v %+v", m.Billing.Wallet, m.Billing.UsageWindow)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "brands", "zs-reference", "catalogue.xml"))
	if err != nil {
		t.Fatal(err)
	}
	facts, err := FactsFromXML(data)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(facts.Units, "api-call") || !slices.Contains(facts.Plans, "core-metered-monthly") || !slices.Contains(facts.Plans, "core-test-monthly") {
		t.Fatalf("facts = %+v", facts)
	}
	if p := CheckMetersAgainst("/billing/meters", *m.Billing.Meters, facts); len(p) != 0 {
		t.Fatalf("meters against the catalogue: %v", p)
	}
	if p := CheckQuotasAgainst("/entitlements", *m.Entitlements, facts); len(p) != 0 {
		t.Fatalf("quotas against the catalogue: %v", p)
	}
}

func TestDefaultsWithoutWalletOrWindow(t *testing.T) {
	var b Billing
	if b.Overdraft() != 0 || b.LowBalance() != 0 || b.Backfill() != 7*24*time.Hour || b.Future() != 5*time.Minute {
		t.Fatalf("defaults = %v %v %v %v", b.Overdraft(), b.LowBalance(), b.Backfill(), b.Future())
	}
}

func TestExtensionManifestRejects(t *testing.T) {
	ref := referenceYAML(t)
	for name, c := range map[string]struct{ old, new, want string }{
		"duplicate meter":       {"      dimensions: [region, method]\n", "      dimensions: [region, method]\n    - slug: api-calls\n      unit: request\n      killbill_unit: api-call\n", "is used twice"},
		"unknown unit":          {"      unit: request\n", "      unit: furlong\n", "/billing/meters/0/unit"},
		"bad dimension":         {"dimensions: [region, method]", "dimensions: [Region]", "/billing/meters/0/dimensions/0"},
		"day without meter":     {"    window: month\n    meter: api-calls\n", "    window: day\n", "a day or month limit counts the usage of a meter"},
		"concurrent with meter": {"    limit: 2\n    window: concurrent\n", "    limit: 2\n    window: concurrent\n    meter: api-calls\n", "a concurrent limit counts"},
		"unknown meter":         {"    meter: api-calls\n", "    meter: tokens\n", `"tokens" is not a meter of the brand`},
		"duplicate quota":       {"  - feature: concurrent-jobs\n", "  - feature: api-call\n    plan: core-metered-monthly\n    limit: 1\n    window: none\n  - feature: concurrent-jobs\n", "are declared twice"},
		"negative limit":        {"    limit: 2\n", "    limit: -1\n", "/entitlements/1/limit"},
		"unknown window":        {"    window: concurrent\n", "    window: hourly\n", "/entitlements/1/window"},
		"negative overdraft":    {"    overdraft_minor: 0\n", "    overdraft_minor: -5\n", "/billing/wallet/overdraft_minor"},
		"unknown key":           {"    low_balance_minor: 500\n", "    low_balance_minor: 500\n    balance_minor: 1\n", "/billing/wallet"},
	} {
		if !strings.Contains(ref, c.old) {
			t.Fatalf("%s: reference manifest lacks %q", name, c.old)
		}
		_, err := Parse([]byte(strings.Replace(ref, c.old, c.new, 1)))
		if !isInvalid(err) || !strings.Contains(messageOf(err), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCatalogueChecks(t *testing.T) {
	facts := CatalogueFacts{Units: []string{"api-call"}, Plans: []string{"basic"}}
	if p := CheckMetersAgainst("/m", []Meter{{Slug: "x", Unit: "count", KillBillUnit: "gpu-hour"}}, facts); len(p) != 1 || !strings.Contains(p[0], `"gpu-hour" is not a usage unit`) {
		t.Fatalf("unknown unit = %v", p)
	}
	if p := CheckQuotasAgainst("/q", []Entitlement{{Feature: "f", Plan: "pro", Limit: 1, Window: WindowNone}, {Feature: "f", Plan: AnyPlan, Limit: 1, Window: WindowNone}}, facts); len(p) != 1 || !strings.Contains(p[0], `"pro" is not a plan`) {
		t.Fatalf("unknown plan = %v", p)
	}
	versions := []killbill.Catalog{{}, {Units: []killbill.CatalogUnit{{Name: "api-call"}}, Products: []killbill.CatalogProduct{{Plans: []killbill.CatalogPlan{{Name: "basic"}}}}}}
	if f := FactsFromCatalog(versions); !slices.Equal(f.Units, []string{"api-call"}) || !slices.Equal(f.Plans, []string{"basic"}) {
		t.Fatalf("facts from Kill Bill = %+v", f)
	}
	if _, err := FactsFromXML([]byte("<catalog")); !isInvalid(err) {
		t.Fatalf("broken XML: %v", err)
	}
}

func TestMeterAndQuotaChanges(t *testing.T) {
	stored := []MeterRecord{
		{Meter: Meter{Slug: "a", Unit: "count", KillBillUnit: "u", Dimensions: []string{"y", "x"}}, Status: MeterActive},
		{Meter: Meter{Slug: "b", Unit: "count", KillBillUnit: "u"}, Status: MeterActive},
		{Meter: Meter{Slug: "c", Unit: "count", KillBillUnit: "u"}, Status: MeterRetired},
	}
	// Dimension order does not matter, b is retired, c comes back and d is
	// new.
	got := meterChanges(stored, []Meter{{Slug: "a", Unit: "count", KillBillUnit: "u", Dimensions: []string{"x", "y"}},
		{Slug: "c", Unit: "count", KillBillUnit: "u"}, {Slug: "d", Unit: "second", KillBillUnit: "u"}})
	var fields []string
	for _, f := range got {
		fields = append(fields, f.Field)
	}
	if strings.Join(fields, ",") != "billing.meters.b,billing.meters.c,billing.meters.d" {
		t.Fatalf("meter changes = %v", fields)
	}
	if len(meterChanges(stored[:1], []Meter{stored[0].Meter})) != 0 {
		t.Fatal("an unchanged meter changes")
	}
	q := []QuotaRecord{{Entitlement: Entitlement{Feature: "f", Plan: "p", Limit: 2, Window: WindowConcurrent}}}
	if c := quotaChanges(q, []Entitlement{{Feature: "f", Plan: "p", Limit: 2, Window: WindowConcurrent}}); len(c) != 0 {
		t.Fatalf("unchanged quota = %v", c)
	}
	if c := quotaChanges(q, []Entitlement{{Feature: "f", Plan: "p", Limit: 3, Window: WindowConcurrent}}); len(c) != 1 || c[0].Before == nil {
		t.Fatalf("changed quota = %v", c)
	}
	if c := quotaChanges(q, nil); len(c) != 1 || c[0].After != nil {
		t.Fatalf("removed quota = %v", c)
	}
}
