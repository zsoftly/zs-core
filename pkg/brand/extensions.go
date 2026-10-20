package brand

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// Targets of the extension changes of an apply.
const (
	TargetMeters = "core.meters"
	TargetQuotas = "core.quotas"
)

// Meter statuses. A meter the brand stops declaring is retired, never
// deleted, because usage events reference it.
const (
	MeterActive  = "active"
	MeterRetired = "retired"
)

// MeterRecord is a row of the meters table.
type MeterRecord struct {
	ID      idgen.UUID
	BrandID idgen.UUID
	Meter
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PublicID returns the mtr_ ID.
func (r MeterRecord) PublicID() string { return idgen.Format(idgen.Meter, r.ID) }

// QuotaRecord is a row of the quotas table.
type QuotaRecord struct {
	ID      idgen.UUID
	BrandID idgen.UUID
	Entitlement
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PublicID returns the qta_ ID.
func (r QuotaRecord) PublicID() string { return idgen.Format(idgen.Quota, r.ID) }

// CatalogueFacts are the names of a catalogue that meters and quotas refer
// to: its usage units and its plans.
type CatalogueFacts struct {
	Units []string
	Plans []string
}

// catalogueXML is the part of a Kill Bill catalogue file the facts need.
type catalogueXML struct {
	Units []struct {
		Name string `xml:"name,attr"`
	} `xml:"units>unit"`
	Plans []struct {
		Name string `xml:"name,attr"`
	} `xml:"plans>plan"`
}

// FactsFromXML reads the usage units and plans of a catalogue file.
func FactsFromXML(data []byte) (CatalogueFacts, error) {
	var c catalogueXML
	if err := xml.Unmarshal(data, &c); err != nil {
		return CatalogueFacts{}, invalid([]string{"/billing/catalogue: the catalogue is not valid XML"})
	}
	var f CatalogueFacts
	for _, u := range c.Units {
		f.Units = append(f.Units, u.Name)
	}
	for _, p := range c.Plans {
		f.Plans = append(f.Plans, p.Name)
	}
	return f, nil
}

// FactsFromCatalog reads the usage units and plans of the catalogue
// version Kill Bill holds in force, the last one.
func FactsFromCatalog(versions []killbill.Catalog) CatalogueFacts {
	var f CatalogueFacts
	if len(versions) == 0 {
		return f
	}
	c := versions[len(versions)-1]
	for _, u := range c.Units {
		f.Units = append(f.Units, u.Name)
	}
	for _, p := range c.Products {
		for _, pl := range p.Plans {
			f.Plans = append(f.Plans, pl.Name)
		}
	}
	return f
}

// extensionProblems checks the wallet, usage window, meters and
// entitlements of a manifest, without the catalogue.
func (m *Manifest) extensionProblems() []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if m.Billing.Meters != nil {
		problems = append(problems, MeterProblems("/billing/meters", *m.Billing.Meters)...)
	}
	if m.Entitlements != nil {
		var slugs []string
		if m.Billing.Meters != nil {
			for _, mt := range *m.Billing.Meters {
				slugs = append(slugs, mt.Slug)
			}
		}
		problems = append(problems, QuotaProblems("/entitlements", *m.Entitlements, slugs, m.Billing.Meters != nil)...)
	}
	if w := m.Billing.Wallet; w != nil && (w.OverdraftMinor < 0 || w.LowBalanceMinor < 0) {
		fail("/billing/wallet: amounts must not be negative")
	}
	return problems
}

// MeterProblems checks a list of meters without the catalogue: unique
// slugs, known units and unique dimension keys.
func MeterProblems(path string, meters []Meter) []string {
	var problems []string
	seen := map[string]bool{}
	for i, mt := range meters {
		at := fmt.Sprintf("%s/%d", path, i)
		if !meterSlug.MatchString(mt.Slug) {
			problems = append(problems, at+"/slug: must be 1 to 63 lowercase letters, digits and dashes")
		}
		if seen[mt.Slug] {
			problems = append(problems, fmt.Sprintf("%s/slug: %q is used twice", at, mt.Slug))
		}
		seen[mt.Slug] = true
		if !slices.Contains(MeterUnits, mt.Unit) {
			problems = append(problems, at+"/unit: must be one of "+strings.Join(MeterUnits, ", "))
		}
		if !killBillName.MatchString(mt.KillBillUnit) {
			problems = append(problems, at+"/killbill_unit: must be a usage unit name of the catalogue")
		}
		if len(mt.Dimensions) > MaxDimensions {
			problems = append(problems, fmt.Sprintf("%s/dimensions: at most %d keys", at, MaxDimensions))
		}
		keys := map[string]bool{}
		for j, d := range mt.Dimensions {
			if !DimensionKey.MatchString(d) || keys[d] {
				problems = append(problems, fmt.Sprintf("%s/dimensions/%d: must be a unique key of 1 to 40 lowercase letters, digits and underscores", at, j))
			}
			keys[d] = true
		}
	}
	return problems
}

// QuotaProblems checks a list of quotas without the catalogue. meterSlugs
// are the meters a quota may name. When checkMeters is false the meters
// are checked later, against the stored ones.
func QuotaProblems(path string, quotas []Entitlement, meterSlugs []string, checkMeters bool) []string {
	var problems []string
	seen := map[string]bool{}
	for i, q := range quotas {
		at := fmt.Sprintf("%s/%d", path, i)
		if !featureName.MatchString(q.Feature) {
			problems = append(problems, at+"/feature: must be 1 to 100 letters, digits, dots, colons, dashes or underscores")
		}
		if q.Plan != AnyPlan && !planName.MatchString(q.Plan) {
			problems = append(problems, at+"/plan: must be a plan name of the catalogue or *")
		}
		key := q.Feature + "\x00" + q.Plan
		if seen[key] {
			problems = append(problems, fmt.Sprintf("%s: feature %q and plan %q are declared twice", at, q.Feature, q.Plan))
		}
		seen[key] = true
		if q.Limit < 0 || q.Limit > MaxQuotaLimit {
			problems = append(problems, fmt.Sprintf("%s/limit: must be 0 to %d", at, int64(MaxQuotaLimit)))
		}
		switch q.Window {
		case WindowDay, WindowMonth:
			if q.Meter == "" {
				problems = append(problems, at+"/meter: a day or month limit counts the usage of a meter, so it needs one")
			}
		case WindowConcurrent:
			if q.Meter != "" {
				problems = append(problems, at+"/meter: a concurrent limit counts what is consumed and released, not a meter")
			}
		case WindowNone:
		default:
			problems = append(problems, at+"/window: must be none, day, month or concurrent")
		}
		if q.Meter != "" && checkMeters && !slices.Contains(meterSlugs, q.Meter) {
			problems = append(problems, fmt.Sprintf("%s/meter: %q is not a meter of the brand", at, q.Meter))
		}
	}
	return problems
}

// Limits of meters and quotas.
const (
	MaxDimensions = 16
	MaxQuotaLimit = 1_000_000_000_000_000
)

var (
	meterSlug    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	killBillName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	featureName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,99}$`)
	planName     = regexp.MustCompile(`^[^\x00-\x1f*][^\x00-\x1f]{0,254}$`)
	// DimensionKey is the form of a dimension key.
	DimensionKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
)

// CheckMetersAgainst refuses a meter whose Kill Bill unit is not a usage
// unit of the catalogue.
func CheckMetersAgainst(path string, meters []Meter, facts CatalogueFacts) []string {
	var problems []string
	for i, mt := range meters {
		if !slices.Contains(facts.Units, mt.KillBillUnit) {
			problems = append(problems, fmt.Sprintf("%s/%d/killbill_unit: %q is not a usage unit of the brand's catalogue", path, i, mt.KillBillUnit))
		}
	}
	return problems
}

// CheckQuotasAgainst refuses a quota whose plan is not a plan of the
// catalogue.
func CheckQuotasAgainst(path string, quotas []Entitlement, facts CatalogueFacts) []string {
	var problems []string
	for i, q := range quotas {
		if q.Plan != AnyPlan && !slices.Contains(facts.Plans, q.Plan) {
			problems = append(problems, fmt.Sprintf("%s/%d/plan: %q is not a plan of the brand's catalogue", path, i, q.Plan))
		}
	}
	return problems
}

const meterColumns = `id, brand_id, slug, unit, killbill_unit, dimensions, status, created_at, updated_at`

func scanMeter(row pgx.Row) (MeterRecord, error) {
	var r MeterRecord
	err := row.Scan(&r.ID, &r.BrandID, &r.Slug, &r.Unit, &r.KillBillUnit, &r.Dimensions, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if r.Dimensions == nil {
		r.Dimensions = []string{}
	}
	return r, err
}

// ListMeters returns every meter of a brand, retired ones included, by
// slug.
func ListMeters(ctx context.Context, q Querier, brandID idgen.UUID) ([]MeterRecord, error) {
	rows, err := q.Query(ctx, `SELECT `+meterColumns+` FROM meters WHERE brand_id = $1 ORDER BY slug`, brandID)
	if err != nil {
		return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (MeterRecord, error) { return scanMeter(r) })
	if err != nil {
		return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	if out == nil {
		out = []MeterRecord{}
	}
	return out, nil
}

// MeterBySlug returns an active meter of a brand. An unknown or retired
// meter is meter_not_found.
func MeterBySlug(ctx context.Context, q Querier, brandID idgen.UUID, slug string) (MeterRecord, error) {
	r, err := scanMeter(q.QueryRow(ctx, `SELECT `+meterColumns+` FROM meters WHERE brand_id = $1 AND slug = $2 AND status = 'active'`, brandID, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return MeterRecord{}, zserrors.New(zserrors.CodeMeterNotFound, "")
	}
	if err != nil {
		return MeterRecord{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return r, nil
}

const quotaColumns = `id, brand_id, feature, plan, limit_value, window_kind, coalesce(meter_slug, ''), created_at, updated_at`

func scanQuota(row pgx.Row) (QuotaRecord, error) {
	var r QuotaRecord
	err := row.Scan(&r.ID, &r.BrandID, &r.Feature, &r.Plan, &r.Limit, &r.Window, &r.Meter, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ListQuotas returns the quotas of a brand, by feature and plan. With
// feature set, only that feature's.
func ListQuotas(ctx context.Context, q Querier, brandID idgen.UUID, feature string) ([]QuotaRecord, error) {
	rows, err := q.Query(ctx, `SELECT `+quotaColumns+` FROM quotas WHERE brand_id = $1 AND ($2 = '' OR feature = $2) ORDER BY feature, plan`, brandID, feature)
	if err != nil {
		return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (QuotaRecord, error) { return scanQuota(r) })
	if err != nil {
		return nil, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	if out == nil {
		out = []QuotaRecord{}
	}
	return out, nil
}

func meterValue(m Meter, status string) map[string]any {
	return map[string]any{"unit": m.Unit, "killbill_unit": m.KillBillUnit, "dimensions": sortedCopy(m.Dimensions), "status": status}
}

func quotaValue(q Entitlement) map[string]any {
	v := map[string]any{"limit": q.Limit, "window": q.Window}
	if q.Meter != "" {
		v["meter"] = q.Meter
	}
	return v
}

// meterChanges returns the field changes that make the stored meters of a
// brand exactly want: new and changed meters, and stored ones to retire.
func meterChanges(stored []MeterRecord, want []Meter) []FieldChange {
	have := map[string]MeterRecord{}
	for _, r := range stored {
		have[r.Slug] = r
	}
	var out []FieldChange
	keep := map[string]bool{}
	for _, m := range want {
		keep[m.Slug] = true
		after := meterValue(m, MeterActive)
		r, ok := have[m.Slug]
		switch {
		case !ok:
			out = append(out, FieldChange{Field: "billing.meters." + m.Slug, After: after})
		case !jsonEqual(meterValue(r.Meter, r.Status), after):
			out = append(out, FieldChange{Field: "billing.meters." + m.Slug, Before: meterValue(r.Meter, r.Status), After: after})
		}
	}
	for _, r := range stored {
		if !keep[r.Slug] && r.Status == MeterActive {
			out = append(out, FieldChange{Field: "billing.meters." + r.Slug, Before: meterValue(r.Meter, r.Status), After: meterValue(r.Meter, MeterRetired)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// quotaChanges returns the field changes that make the stored quotas of a
// brand exactly want.
func quotaChanges(stored []QuotaRecord, want []Entitlement) []FieldChange {
	key := func(q Entitlement) string { return "entitlements." + q.Feature + "." + q.Plan }
	have := map[string]QuotaRecord{}
	for _, r := range stored {
		have[key(r.Entitlement)] = r
	}
	var out []FieldChange
	keep := map[string]bool{}
	for _, q := range want {
		k := key(q)
		keep[k] = true
		r, ok := have[k]
		switch {
		case !ok:
			out = append(out, FieldChange{Field: k, After: quotaValue(q)})
		case !jsonEqual(quotaValue(r.Entitlement), quotaValue(q)):
			out = append(out, FieldChange{Field: k, Before: quotaValue(r.Entitlement), After: quotaValue(q)})
		}
	}
	for _, r := range stored {
		if k := key(r.Entitlement); !keep[k] {
			out = append(out, FieldChange{Field: k, Before: quotaValue(r.Entitlement)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// storeMeters makes the meters of a brand exactly want in tx: it adds and
// updates the declared meters and retires the others.
func storeMeters(ctx context.Context, tx pgx.Tx, brandID idgen.UUID, want []Meter) error {
	slugs := make([]string, 0, len(want))
	for _, m := range want {
		slugs = append(slugs, m.Slug)
		if _, err := tx.Exec(ctx, `INSERT INTO meters (id, brand_id, slug, unit, killbill_unit, dimensions)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (brand_id, slug) DO UPDATE SET unit = EXCLUDED.unit, killbill_unit = EXCLUDED.killbill_unit,
    dimensions = EXCLUDED.dimensions, status = 'active', updated_at = now()
WHERE (meters.unit, meters.killbill_unit, meters.dimensions, meters.status)
    IS DISTINCT FROM (EXCLUDED.unit, EXCLUDED.killbill_unit, EXCLUDED.dimensions, 'active')`,
			idgen.MustNewV7(), brandID, m.Slug, m.Unit, m.KillBillUnit, sortedCopy(m.Dimensions)); err != nil {
			return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE meters SET status = 'retired', updated_at = now()
WHERE brand_id = $1 AND status = 'active' AND NOT (slug = ANY($2::text[]))`, brandID, slugs); err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return nil
}

// storeQuotas makes the quotas of a brand exactly want in tx.
func storeQuotas(ctx context.Context, tx pgx.Tx, brandID idgen.UUID, want []Entitlement) error {
	features := make([]string, 0, len(want))
	plans := make([]string, 0, len(want))
	for _, q := range want {
		features, plans = append(features, q.Feature), append(plans, q.Plan)
		if _, err := tx.Exec(ctx, `INSERT INTO quotas (id, brand_id, feature, plan, limit_value, window_kind, meter_slug)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (brand_id, feature, plan) DO UPDATE SET limit_value = EXCLUDED.limit_value, window_kind = EXCLUDED.window_kind,
    meter_slug = EXCLUDED.meter_slug, updated_at = now()
WHERE (quotas.limit_value, quotas.window_kind, quotas.meter_slug)
    IS DISTINCT FROM (EXCLUDED.limit_value, EXCLUDED.window_kind, EXCLUDED.meter_slug)`,
			idgen.MustNewV7(), brandID, q.Feature, q.Plan, q.Limit, q.Window, nilIfEmpty(q.Meter)); err != nil {
			return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM quotas q WHERE q.brand_id = $1 AND NOT EXISTS (
    SELECT 1 FROM unnest($2::text[], $3::text[]) AS k(feature, plan) WHERE k.feature = q.feature AND k.plan = q.plan)`, brandID, features, plans); err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return nil
}

// catalogueFacts reads the facts of the catalogue the apply converges to:
// the manifest's catalogue file when it names one, otherwise the catalogue
// Kill Bill holds for the brand.
func catalogueFacts(ctx context.Context, kb *killbill.Client, creds killbill.TenantCredentials, exists bool, m *Manifest, fs files) (CatalogueFacts, error) {
	if m.Billing.Catalogue != "" {
		return FactsFromXML(fs[m.Billing.Catalogue])
	}
	if !exists {
		return CatalogueFacts{}, nil
	}
	versions, err := kb.Catalogue(ctx, creds)
	if err != nil {
		return CatalogueFacts{}, err
	}
	return FactsFromCatalog(versions), nil
}

// planExtensions adds the changes that converge the brand's meters and
// quotas to the manifest, when the manifest declares them. It checks every
// meter against the catalogue's usage units and every quota against its
// plans and meters before anything changes.
func planExtensions(ctx context.Context, deps Deps, p *plan, m *Manifest, sec resolved, fs files) ([]Change, error) {
	if m.Billing.Meters == nil && m.Entitlements == nil {
		return nil, nil
	}
	creds := killbill.TenantCredentials{APIKey: m.Billing.KillBillTenantKey, APISecret: sec[m.Billing.KillBillAPISecretRef]}
	exists := true
	if _, err := deps.KillBill.TenantByAPIKey(ctx, creds.APIKey); errors.Is(err, killbill.ErrNotFound) {
		exists = false
	} else if err != nil {
		return nil, err
	}
	facts, err := catalogueFacts(ctx, deps.KillBill, creds, exists, m, fs)
	if err != nil {
		return nil, err
	}
	var storedMeters []MeterRecord
	var storedQuotas []QuotaRecord
	if p.record != nil {
		if storedMeters, err = ListMeters(ctx, deps.DB, p.brandID); err != nil {
			return nil, err
		}
		if storedQuotas, err = ListQuotas(ctx, deps.DB, p.brandID, ""); err != nil {
			return nil, err
		}
	}
	var problems []string
	if m.Billing.Meters != nil {
		problems = append(problems, CheckMetersAgainst("/billing/meters", *m.Billing.Meters, facts)...)
	}
	if m.Entitlements != nil {
		problems = append(problems, CheckQuotasAgainst("/entitlements", *m.Entitlements, facts)...)
		if m.Billing.Meters == nil {
			var active []string
			for _, r := range storedMeters {
				if r.Status == MeterActive {
					active = append(active, r.Slug)
				}
			}
			problems = append(problems, QuotaProblems("/entitlements", *m.Entitlements, active, true)...)
		}
	}
	if len(problems) > 0 {
		return nil, invalid(problems)
	}
	var changes []Change
	if m.Billing.Meters != nil {
		want := *m.Billing.Meters
		if fields := meterChanges(storedMeters, want); len(fields) > 0 {
			changes = append(changes, Change{Target: TargetMeters, Name: m.Slug, Action: ActionUpdate, Fields: fields,
				run: func(ctx context.Context, st *runState) error {
					return inTx(ctx, st.deps.DB, func(tx pgx.Tx) error { return storeMeters(ctx, tx, st.brandID, want) })
				}})
		}
	}
	if m.Entitlements != nil {
		want := *m.Entitlements
		if fields := quotaChanges(storedQuotas, want); len(fields) > 0 {
			changes = append(changes, Change{Target: TargetQuotas, Name: m.Slug, Action: ActionUpdate, Fields: fields,
				run: func(ctx context.Context, st *runState) error {
					return inTx(ctx, st.deps.DB, func(tx pgx.Tx) error { return storeQuotas(ctx, tx, st.brandID, want) })
				}})
		}
	}
	return changes, nil
}

// errManifestManaged is the answer to a change of meters or quotas that the
// brand's applied manifest declares. They change through the manifest.
func errManifestManaged(what string) error {
	return zserrors.New(zserrors.CodeConflict, "The brand manifest declares the "+what+": change the manifest and apply it.")
}

// ReplaceMeters makes the meters of a brand exactly want in tx, for the
// HTTP route. It refuses a brand whose applied manifest declares its
// meters, checks every meter against the catalogue facts, and audits the
// change as brand.meters.updated with its diff. It returns the meters and
// the field changes, none when nothing changed.
func ReplaceMeters(ctx context.Context, tx pgx.Tx, brandID idgen.UUID, want []Meter, facts CatalogueFacts, actor events.Actor, reason, correlationID string) ([]MeterRecord, []FieldChange, error) {
	rec, err := lockBrand(ctx, tx, brandID)
	if err != nil {
		return nil, nil, err
	}
	if rec.Manifest != nil && rec.Manifest.Billing.Meters != nil {
		return nil, nil, errManifestManaged("meters")
	}
	problems := MeterProblems("/meters", want)
	problems = append(problems, CheckMetersAgainst("/meters", want, facts)...)
	if len(problems) > 0 {
		return nil, nil, invalid(problems)
	}
	stored, err := ListMeters(ctx, tx, brandID)
	if err != nil {
		return nil, nil, err
	}
	fields := meterChanges(stored, want)
	if len(fields) > 0 {
		if err := storeMeters(ctx, tx, brandID, want); err != nil {
			return nil, nil, err
		}
		if err := auditExtension(ctx, tx, brandID, TargetMeters, rec.Slug, fields, actor, reason, correlationID); err != nil {
			return nil, nil, err
		}
	}
	out, err := ListMeters(ctx, tx, brandID)
	return out, fields, err
}

// ReplaceQuotas makes the quotas of a brand exactly want in tx, for the
// HTTP route. It refuses a brand whose applied manifest declares its
// quotas, checks every quota against the catalogue and the brand's active
// meters, and audits the change as brand.quotas.updated.
func ReplaceQuotas(ctx context.Context, tx pgx.Tx, brandID idgen.UUID, want []Entitlement, facts CatalogueFacts, actor events.Actor, reason, correlationID string) ([]QuotaRecord, []FieldChange, error) {
	rec, err := lockBrand(ctx, tx, brandID)
	if err != nil {
		return nil, nil, err
	}
	if rec.Manifest != nil && rec.Manifest.Entitlements != nil {
		return nil, nil, errManifestManaged("quotas")
	}
	meters, err := ListMeters(ctx, tx, brandID)
	if err != nil {
		return nil, nil, err
	}
	var active []string
	for _, r := range meters {
		if r.Status == MeterActive {
			active = append(active, r.Slug)
		}
	}
	problems := QuotaProblems("/quotas", want, active, true)
	problems = append(problems, CheckQuotasAgainst("/quotas", want, facts)...)
	if len(problems) > 0 {
		return nil, nil, invalid(problems)
	}
	stored, err := ListQuotas(ctx, tx, brandID, "")
	if err != nil {
		return nil, nil, err
	}
	fields := quotaChanges(stored, want)
	if len(fields) > 0 {
		if err := storeQuotas(ctx, tx, brandID, want); err != nil {
			return nil, nil, err
		}
		if err := auditExtension(ctx, tx, brandID, TargetQuotas, rec.Slug, fields, actor, reason, correlationID); err != nil {
			return nil, nil, err
		}
	}
	out, err := ListQuotas(ctx, tx, brandID, "")
	return out, fields, err
}

// lockBrand reads a brand row and locks it for the rest of tx, so a
// replacement and an apply of the same brand never interleave.
func lockBrand(ctx context.Context, tx pgx.Tx, brandID idgen.UUID) (Record, error) {
	rec, err := getOne(ctx, tx, "id = $1 FOR UPDATE", brandID)
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

func auditExtension(ctx context.Context, tx pgx.Tx, brandID idgen.UUID, target, slug string, fields []FieldChange, actor events.Actor, reason, correlationID string) error {
	ch := Change{Target: target, Name: slug, Action: ActionUpdate, Fields: fields}
	_, err := audit.WriteBrand(ctx, tx, brandID, audit.Entry{
		Actor: actor, Action: actionFor(ch), Reason: strings.TrimSpace(reason), Outcome: audit.OutcomeSuccess,
		CorrelationID: correlationID, Details: map[string]any{"change": changeDetails(ch)},
	})
	return err
}
