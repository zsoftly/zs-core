package brand

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/keycloak"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/secrets"
	"github.com/zsoftly/zs-core/pkg/audit"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
	"github.com/zsoftly/zs-core/pkg/events"
	"github.com/zsoftly/zs-core/pkg/idgen"
)

// DB is what Apply needs from the database: reads, and transactions for the
// brand row and the audit trail.
type DB interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Deps are the collaborators of Apply and Plan.
type Deps struct {
	DB       DB
	Keycloak *keycloak.Client
	KillBill *killbill.Client
	Secrets  secrets.Resolver
	// Files resolves the manifest's file references (catalogue, overdue).
	// Paths are relative to it and cannot leave it. When nil, a manifest
	// that references a file is refused.
	Files fs.FS
	// FilesPrefix, when set, is a directory every file reference must start
	// with, such as "zs-reference/". It is removed before the path is opened
	// in Files. The HTTP route sets it to the brand slug and Files to
	// ZS_BRANDS_DIR/<slug>, so one brand cannot read another brand's files.
	FilesPrefix string
}

// Options describe who applies and why.
type Options struct {
	Actor         events.Actor
	Reason        string
	CorrelationID string
	// CreatedBy is the X-Killbill-CreatedBy value, such as "zs-core-cli".
	CreatedBy string
	// ExpectBrand, when set, refuses a manifest for any other brand.
	ExpectBrand idgen.UUID
	// ConfirmSensitiveChange is called before anything changes when the
	// apply touches a field that decides how people authenticate or how
	// money moves: the MFA policy, the MFA acr values, a client, an identity
	// provider, ssl_required or the payment plugin. It receives the fields.
	// The HTTP route requires a reason and recent MFA here. The command line
	// passes nil: it runs as a service actor named by --operator.
	ConfirmSensitiveChange func(fields []string) error
	// RequireRealmSSL refuses a manifest whose identity.ssl_required is not
	// external or all. The HTTP route sets it.
	RequireRealmSSL bool
}

// Targets of a change.
const (
	TargetBrand            = "core.brand"
	TargetRealm            = "keycloak.realm"
	TargetFlow             = "keycloak.authentication_flow"
	TargetRequiredAction   = "keycloak.required_action"
	TargetClient           = "keycloak.client"
	TargetIdentityProvider = "keycloak.identity_provider"
	TargetMFAPolicy        = "keycloak.mfa_policy"
	TargetKillBillTenant   = "killbill.tenant"
	TargetCatalogue        = "killbill.catalogue"
	TargetOverdue          = "killbill.overdue"
	TargetPluginConfig     = "killbill.plugin_config"
)

// Actions of a change.
const (
	ActionCreate = "create"
	ActionUpdate = "update"
)

// FieldChange is one field that differs. Before and After are omitted, and
// Redacted is true, for secrets.
type FieldChange struct {
	Field    string `json:"field"`
	Before   any    `json:"before,omitempty"`
	After    any    `json:"after,omitempty"`
	Redacted bool   `json:"redacted,omitempty"`
}

// Change is one object Apply creates or updates.
type Change struct {
	Target string        `json:"target"`
	Name   string        `json:"name"`
	Action string        `json:"action"`
	Fields []FieldChange `json:"fields"`

	run func(ctx context.Context, st *runState) error
}

// Diff is the result of Plan and Apply. An empty Changes list with equal
// digests means the brand already matches the manifest.
type Diff struct {
	BrandID              string   `json:"brand_id,omitempty"`
	Slug                 string   `json:"slug"`
	ManifestDigestBefore string   `json:"manifest_digest_before,omitempty"`
	ManifestDigest       string   `json:"manifest_digest"`
	Changes              []Change `json:"changes"`
	Applied              bool     `json:"applied"`
}

// Empty reports whether nothing would change, including the recorded
// manifest.
func (d Diff) Empty() bool {
	return len(d.Changes) == 0 && d.ManifestDigestBefore == d.ManifestDigest
}

// plan is a computed diff plus what executing it needs.
type plan struct {
	diff                Diff
	record              *Record // nil when the brand row does not exist yet
	brandID             idgen.UUID
	paymentPluginChange bool
	sensitive           []string
}

// runState carries values that earlier changes produce for later ones.
type runState struct {
	deps     Deps
	opts     Options
	manifest *Manifest
	brandID  idgen.UUID
	kbCreds  killbill.TenantCredentials
}

func (st *runState) callContext() killbill.CallContext {
	createdBy := st.opts.CreatedBy
	if createdBy == "" {
		createdBy = "zs-core"
	}
	return killbill.CallContext{CreatedBy: createdBy, Reason: st.opts.Reason, Comment: st.opts.CorrelationID}
}

// Plan computes the diff between the manifest and the current state of Core,
// Keycloak and Kill Bill without changing anything. It resolves the secret
// references, so a missing secret fails here too.
func Plan(ctx context.Context, deps Deps, m *Manifest, opts Options) (Diff, error) {
	p, err := makePlan(ctx, deps, m, opts)
	if err != nil {
		return Diff{}, err
	}
	return p.diff, nil
}

// Apply converges Keycloak and Kill Bill to m and records the brand. It
// holds a database lock per brand slug for its duration, so two applies of
// one brand never interleave. With opts.ExpectBrand set, the lock is taken
// only after the slug is known to belong to that brand, so a manifest for
// another brand cannot hold that brand's lock. Every change is audited with
// its diff after it succeeds, or with outcome failure when it fails, and
// Apply stops at the first failure; running it again continues from the
// state reached.
func Apply(ctx context.Context, deps Deps, m *Manifest, opts Options) (Diff, error) {
	if opts.CorrelationID == "" {
		opts.CorrelationID = idgen.MustNewV7().String()
	}
	if !opts.Actor.Type.Valid() {
		return Diff{}, errors.New("brand apply: actor is required")
	}
	if deps.DB == nil {
		return Diff{}, zserrors.New(zserrors.CodeNotReady, "Brand apply needs the database, Keycloak and Kill Bill admin settings.")
	}
	if err := checkExpectedBrand(ctx, deps.DB, m.Slug, opts.ExpectBrand); err != nil {
		return Diff{}, err
	}
	lock, err := deps.DB.Begin(ctx)
	if err != nil {
		return Diff{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	defer func() { _ = lock.Rollback(context.WithoutCancel(ctx)) }()
	var locked bool
	if err := lock.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('zs-core.brand.apply:' || $1, 0))`, m.Slug).Scan(&locked); err != nil {
		return Diff{}, zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	if !locked {
		return Diff{}, zserrors.New(zserrors.CodeBrandApplyInProgress, "")
	}

	p, err := makePlan(ctx, deps, m, opts)
	if err != nil {
		return Diff{}, err
	}
	if err := p.checkGates(opts); err != nil {
		return p.diff, err
	}

	st := &runState{deps: deps, opts: opts, manifest: m, brandID: p.brandID}
	for i := range p.diff.Changes {
		ch := &p.diff.Changes[i]
		if err := ch.run(ctx, st); err != nil {
			mapped := mapUpstream(err)
			if ch.Target != TargetBrand || p.record != nil {
				_ = writeAudit(context.WithoutCancel(ctx), deps.DB, st.brandID, opts, actionFor(*ch), audit.OutcomeFailure, *ch, p.diff.ManifestDigest, codeName(mapped))
			}
			return p.diff, mapped
		}
		if err := writeAudit(ctx, deps.DB, st.brandID, opts, actionFor(*ch), audit.OutcomeSuccess, *ch, p.diff.ManifestDigest, ""); err != nil {
			return p.diff, err
		}
	}
	if len(p.diff.Changes) > 0 || p.diff.ManifestDigestBefore != p.diff.ManifestDigest {
		if err := recordManifest(ctx, deps.DB, st.brandID, m, p.diff, opts); err != nil {
			return p.diff, err
		}
	}
	p.diff.BrandID = idgen.Format(idgen.Brand, st.brandID)
	p.diff.Applied = true
	return p.diff, nil
}

func makePlan(ctx context.Context, deps Deps, m *Manifest, opts Options) (*plan, error) {
	if deps.DB == nil || deps.Keycloak == nil || deps.KillBill == nil || deps.Secrets == nil {
		return nil, zserrors.New(zserrors.CodeNotReady, "Brand apply needs the database, Keycloak and Kill Bill admin settings.")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := checkRealmSSL(m, opts); err != nil {
		return nil, err
	}
	if err := m.CheckSecretRefs(deps.Secrets); err != nil {
		return nil, err
	}
	p := &plan{diff: Diff{Slug: m.Slug, ManifestDigest: m.Digest(), Changes: []Change{}}}

	rec, err := GetBySlug(ctx, deps.DB, m.Slug)
	switch {
	case err == nil:
		p.record = &rec
		p.brandID = rec.ID
		p.diff.BrandID = rec.PublicID()
		p.diff.ManifestDigestBefore = rec.ManifestDigest
		if rec.KeycloakRealm != m.Identity.Realm {
			return nil, invalid([]string{"/identity/realm: brand " + m.Slug + " uses realm " + rec.KeycloakRealm + " and the realm cannot change"})
		}
		if rec.KillBillAPIKey != m.Billing.KillBillTenantKey {
			return nil, invalid([]string{"/billing/killbill_tenant_key: brand " + m.Slug + " uses tenant key " + rec.KillBillAPIKey + " and the key cannot change"})
		}
	case errors.Is(err, ErrNotFound):
		p.brandID = idgen.MustNewV7()
	default:
		return nil, err
	}
	if !opts.ExpectBrand.IsNil() && opts.ExpectBrand != p.brandID {
		return nil, errOtherBrand()
	}
	if p.record == nil {
		if err := checkUnique(ctx, deps.DB, m); err != nil {
			return nil, err
		}
	}
	if err := checkForeignNamespaces(ctx, deps.DB, m); err != nil {
		return nil, err
	}

	resolved, err := resolveSecrets(ctx, deps.Secrets, m)
	if err != nil {
		return nil, err
	}
	files, err := readFiles(deps.Files, deps.FilesPrefix, m)
	if err != nil {
		return nil, err
	}

	p.diff.Changes = append(p.diff.Changes, brandChanges(p, m)...)
	kc, err := planKeycloak(ctx, deps.Keycloak, m, resolved)
	if err != nil {
		return nil, mapUpstream(err)
	}
	p.diff.Changes = append(p.diff.Changes, kc...)
	kb, err := planKillBill(ctx, deps.KillBill, p, m, resolved, files)
	if err != nil {
		return nil, mapUpstream(err)
	}
	p.diff.Changes = append(p.diff.Changes, kb...)

	var prev *Manifest
	if p.record != nil {
		prev = p.record.Manifest
	}
	p.paymentPluginChange = paymentPluginChanged(prev, m, p.diff.Changes)
	p.sensitive = sensitiveFields(prev, m, p.diff.Changes)
	return p, nil
}

func errOtherBrand() error {
	return invalid([]string{"/slug: the manifest belongs to another brand"})
}

// checkExpectedBrand refuses, when expect is set, a slug that is not the slug
// of brand expect. It reads the brand row only and takes no lock.
func checkExpectedBrand(ctx context.Context, db Querier, slug string, expect idgen.UUID) error {
	if expect.IsNil() {
		return nil
	}
	rec, err := GetBySlug(ctx, db, slug)
	switch {
	case errors.Is(err, ErrNotFound):
		return errOtherBrand()
	case err != nil:
		return err
	case rec.ID != expect:
		return errOtherBrand()
	}
	return nil
}

// checkRealmSSL refuses, when opts.RequireRealmSSL is set, a manifest whose
// ssl_required is not external or all.
func checkRealmSSL(m *Manifest, opts Options) error {
	if opts.RequireRealmSSL && m.Identity.SSLRequired != "external" && m.Identity.SSLRequired != "all" {
		return invalid([]string{"/identity/ssl_required: must be external or all; only the command line may apply none"})
	}
	return nil
}

// checkGates refuses a plan that changes the payment plugin without a
// reason, on every path, and passes a plan that changes a sensitive field to
// opts.ConfirmSensitiveChange.
func (p *plan) checkGates(opts Options) error {
	if p.paymentPluginChange && strings.TrimSpace(opts.Reason) == "" {
		return zserrors.New(zserrors.CodeReasonRequired, "Changing the payment plugin requires a reason.")
	}
	if len(p.sensitive) > 0 && opts.ConfirmSensitiveChange != nil {
		return opts.ConfirmSensitiveChange(p.sensitive)
	}
	return nil
}

// paymentPluginChanged reports whether the apply changes the brand's payment
// plugin: its configuration in Kill Bill, or its name, configuration key or
// configuration reference in the manifest, or adds or removes it.
func paymentPluginChanged(prev, m *Manifest, changes []Change) bool {
	for _, ch := range changes {
		if ch.Target == TargetPluginConfig {
			return true
		}
	}
	var before *PaymentPlugin
	if prev != nil {
		before = prev.Billing.PaymentPlugin
	}
	next := m.Billing.PaymentPlugin
	switch {
	case before == nil && next == nil:
		return false
	case before == nil || next == nil:
		return true
	}
	return before.Name != next.Name || before.ConfigKey != next.ConfigKey || before.ConfigRef != next.ConfigRef
}

// sensitiveFields lists the fields of the apply that decide how people
// authenticate or how money moves, sorted. prev is the manifest of the last
// apply, nil for a new brand, in which case every such field counts.
func sensitiveFields(prev, m *Manifest, changes []Change) []string {
	set := map[string]bool{}
	for _, ch := range changes {
		switch ch.Target {
		case TargetMFAPolicy, TargetFlow, TargetClient, TargetClientScope, TargetIdentityProvider, TargetPluginConfig:
			for _, f := range ch.Fields {
				set[f.Field] = true
			}
			if len(ch.Fields) == 0 {
				set[ch.Target+"."+ch.Name] = true
			}
		case TargetRealm:
			for _, f := range ch.Fields {
				if f.Field == "identity.ssl_required" {
					set[f.Field] = true
				}
			}
		}
	}
	before := &Manifest{}
	if prev != nil {
		before = prev
	}
	for _, c := range []struct {
		field      string
		old, value any
	}{
		{"identity.mfa", before.Identity.MFA, m.Identity.MFA},
		{"identity.mfa_acr_values", before.Identity.MFAACRValues, m.Identity.MFAACRValues},
		{"identity.ssl_required", before.Identity.SSLRequired, m.Identity.SSLRequired},
		{"identity.customer_client", before.Identity.CustomerClient, m.Identity.CustomerClient},
		{"identity.staff_client", before.Identity.StaffClient, m.Identity.StaffClient},
		{"identity.service_clients", before.Identity.ServiceClients, m.Identity.ServiceClients},
		{"identity.identity_providers", before.Identity.IdentityProviders, m.Identity.IdentityProviders},
		{"billing.payment_plugin", before.Billing.PaymentPlugin, m.Billing.PaymentPlugin},
	} {
		if !jsonEqual(c.old, c.value) {
			set[c.field] = true
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// jsonEqual compares two manifest values by their JSON form, so a nil and an
// empty list are equal, as they are in the stored manifest.
func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return normalizeJSON(ja) == normalizeJSON(jb)
}

func normalizeJSON(b []byte) string {
	s := string(b)
	if s == "null" || s == "[]" || s == `""` {
		return ""
	}
	return s
}

// checkForeignNamespaces refuses an env:// reference that falls inside the
// namespace of another brand. Valid slugs give prefixes that never overlap,
// BRAND_ZS__ and BRAND_ZS_REFERENCE__ for zs and zs-reference, so the
// namespace check alone already refuses such a reference. This check reads
// the brands table and is kept as defence in depth.
func checkForeignNamespaces(ctx context.Context, db Querier, m *Manifest) error {
	rows, err := db.Query(ctx, `SELECT slug FROM brands WHERE slug <> $1`, m.Slug)
	if err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	if problems := foreignNamespaceRefs(m, slugs); len(problems) > 0 {
		return zserrors.New(zserrors.CodeSecretRefInvalid, "Secret references outside the brand's namespace: "+strings.Join(problems, "; ")+".")
	}
	return nil
}

func foreignNamespaceRefs(m *Manifest, otherSlugs []string) []string {
	own := secrets.EnvPrefix(m.Slug)
	var problems []string
	for _, ref := range m.secretRefs() {
		name, ok := strings.CutPrefix(ref.ref, "env://")
		if !ok {
			continue
		}
		for _, other := range otherSlugs {
			prefix := secrets.EnvPrefix(other)
			if other != m.Slug && prefix != own && strings.HasPrefix(name, prefix) {
				problems = append(problems, ref.path+": "+prefix+" is the namespace of brand "+other)
			}
		}
	}
	sort.Strings(problems)
	return problems
}

// checkUnique refuses a new brand whose realm or Kill Bill tenant key
// another brand already uses.
func checkUnique(ctx context.Context, db Querier, m *Manifest) error {
	var other string
	err := db.QueryRow(ctx, `SELECT slug FROM brands WHERE keycloak_realm = $1 OR killbill_api_key = $2 LIMIT 1`,
		m.Identity.Realm, m.Billing.KillBillTenantKey).Scan(&other)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return invalid([]string{"brand " + other + " already uses this realm or Kill Bill tenant key"})
}

// resolved holds secret values by reference. It never leaves this package.
type resolved map[string]string

func resolveSecrets(ctx context.Context, r secrets.Resolver, m *Manifest) (resolved, error) {
	// Webhook secrets are checked with the others but resolved only when
	// notifications are applied.
	refs := []string{m.Identity.StaffClient.SecretRef, m.Billing.KillBillAPISecretRef}
	for _, sc := range m.Identity.ServiceClients {
		refs = append(refs, sc.SecretRef)
	}
	for _, p := range m.Identity.IdentityProviders {
		if p.ClientSecretRef != "" {
			refs = append(refs, p.ClientSecretRef)
		}
	}
	if m.Billing.PaymentPlugin != nil {
		refs = append(refs, m.Billing.PaymentPlugin.ConfigRef)
	}
	out := resolved{}
	for _, ref := range refs {
		if _, ok := out[ref]; ok {
			continue
		}
		v, err := r.Resolve(ctx, m.Slug, ref)
		if errors.Is(err, secrets.ErrNotAllowed) {
			return nil, zserrors.Wrap(zserrors.CodeSecretRefInvalid, "The secret reference "+ref+" is outside the brand's namespace.", err)
		}
		if err != nil {
			// The reference is safe to name; the error never holds a value,
			// and every failure of a valid reference gives this one message.
			return nil, zserrors.Wrap(zserrors.CodeSecretUnresolved, "The secret reference "+ref+" could not be resolved.", err)
		}
		out[ref] = v
	}
	return out, nil
}

// files holds the manifest's referenced files by manifest path.
type files map[string][]byte

const maxReferencedFileBytes = 4 << 20

func readFiles(root fs.FS, prefix string, m *Manifest) (files, error) {
	out := files{}
	for _, f := range []struct{ field, path string }{
		{"/billing/catalogue", m.Billing.Catalogue},
		{"/billing/overdue", m.Billing.Overdue},
	} {
		if f.path == "" {
			continue
		}
		if root == nil {
			return nil, invalid([]string{f.field + ": file references are not available here; set ZS_BRANDS_DIR or apply the manifest from a file"})
		}
		name := f.path
		if prefix != "" {
			rest, ok := strings.CutPrefix(name, prefix)
			if !ok || rest == "" {
				return nil, invalid([]string{f.field + ": must be inside the brand's directory " + prefix})
			}
			name = rest
		}
		fh, err := root.Open(name)
		if err != nil {
			return nil, invalid([]string{f.field + ": cannot open " + f.path})
		}
		data, err := io.ReadAll(io.LimitReader(fh, maxReferencedFileBytes+1))
		_ = fh.Close()
		if err != nil || len(data) > maxReferencedFileBytes {
			return nil, invalid([]string{f.field + ": cannot read " + f.path + " or it is larger than 4 MiB"})
		}
		out[f.path] = data
	}
	return out, nil
}

func brandChanges(p *plan, m *Manifest) []Change {
	if p.record == nil {
		return []Change{{
			Target: TargetBrand, Name: m.Slug, Action: ActionCreate,
			Fields: []FieldChange{
				{Field: "slug", After: m.Slug},
				{Field: "name", After: m.Name},
				{Field: "identity.realm", After: m.Identity.Realm},
				{Field: "billing.killbill_tenant_key", After: m.Billing.KillBillTenantKey},
				{Field: "billing.killbill_api_secret_ref", After: m.Billing.KillBillAPISecretRef},
			},
			run: func(ctx context.Context, st *runState) error {
				return inTx(ctx, st.deps.DB, func(tx pgx.Tx) error {
					return insertRecord(ctx, tx, Record{
						ID: st.brandID, Slug: m.Slug, Name: m.Name, Status: StatusActive,
						KeycloakRealm: m.Identity.Realm, KillBillAPIKey: m.Billing.KillBillTenantKey,
						KillBillAPISecretRef: m.Billing.KillBillAPISecretRef, Manifest: m, ManifestDigest: m.Digest(),
					})
				})
			},
		}}
	}
	rec := p.record
	var fields []FieldChange
	if rec.Name != m.Name {
		fields = append(fields, FieldChange{Field: "name", Before: rec.Name, After: m.Name})
	}
	if rec.KillBillAPISecretRef != m.Billing.KillBillAPISecretRef {
		fields = append(fields, FieldChange{Field: "billing.killbill_api_secret_ref", Before: rec.KillBillAPISecretRef, After: m.Billing.KillBillAPISecretRef})
	}
	if len(fields) == 0 {
		return nil
	}
	return []Change{{
		Target: TargetBrand, Name: m.Slug, Action: ActionUpdate, Fields: fields,
		run: func(ctx context.Context, st *runState) error {
			return inTx(ctx, st.deps.DB, func(tx pgx.Tx) error {
				return updateIdentityFields(ctx, tx, st.brandID, m.Name, m.Billing.KillBillAPISecretRef)
			})
		},
	}}
}

func inTx(ctx context.Context, db DB, fn func(pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
	}
	return nil
}

var auditActions = map[string]string{
	TargetBrand:            "brand",
	TargetRealm:            "brand.keycloak.realm",
	TargetFlow:             "brand.keycloak.flow",
	TargetRequiredAction:   "brand.keycloak.required_action",
	TargetClient:           "brand.keycloak.client",
	TargetClientScope:      "brand.keycloak.client_scope",
	TargetIdentityProvider: "brand.keycloak.identity_provider",
	TargetMFAPolicy:        "brand.keycloak.mfa_policy",
	TargetKillBillTenant:   "brand.killbill.tenant",
	TargetCatalogue:        "brand.killbill.catalogue",
	TargetOverdue:          "brand.killbill.overdue",
	TargetPluginConfig:     "brand.killbill.plugin_config",
}

func actionFor(ch Change) string {
	verb := "updated"
	if ch.Action == ActionCreate {
		verb = "created"
	}
	return auditActions[ch.Target] + "." + verb
}

func writeAudit(ctx context.Context, db DB, brandID idgen.UUID, opts Options, action string, outcome audit.Outcome, ch Change, digest, errorCode string) error {
	details := map[string]any{"change": changeDetails(ch), "manifest_digest": digest}
	if errorCode != "" {
		details["error"] = errorCode
	}
	return inTx(ctx, db, func(tx pgx.Tx) error {
		_, err := audit.WriteBrand(ctx, tx, brandID, audit.Entry{
			Actor: opts.Actor, Action: action, Reason: opts.Reason, Outcome: outcome,
			CorrelationID: opts.CorrelationID, Details: details,
		})
		return err
	})
}

// changeDetails is the JSON form of a change for the audit trail. Redacted
// fields carry no value by construction.
func changeDetails(ch Change) map[string]any {
	fields := make([]map[string]any, 0, len(ch.Fields))
	for _, f := range ch.Fields {
		item := map[string]any{"field": f.Field}
		if f.Redacted {
			item["redacted"] = true
		} else {
			item["before"] = f.Before
			item["after"] = f.After
		}
		fields = append(fields, item)
	}
	return map[string]any{"target": ch.Target, "name": ch.Name, "action": ch.Action, "fields": fields}
}

func recordManifest(ctx context.Context, db DB, brandID idgen.UUID, m *Manifest, d Diff, opts Options) error {
	return inTx(ctx, db, func(tx pgx.Tx) error {
		if err := recordApplied(ctx, tx, brandID, m, d.ManifestDigest); err != nil {
			return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "", err)
		}
		_, err := audit.WriteBrand(ctx, tx, brandID, audit.Entry{
			Actor: opts.Actor, Action: "brand.manifest.recorded", Reason: opts.Reason, Outcome: audit.OutcomeSuccess,
			CorrelationID: opts.CorrelationID,
			Details: map[string]any{
				"manifest_digest_before": d.ManifestDigestBefore,
				"manifest_digest":        d.ManifestDigest,
				"changes":                len(d.Changes),
			},
		})
		return err
	})
}

// mapUpstream turns engine errors into typed errors. Upstream messages never
// reach a caller: only the operation and the HTTP status do.
func mapUpstream(err error) error {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return err
	}
	var kcErr *keycloak.Error
	var kbErr *killbill.Error
	switch {
	case keycloak.IsUnavailable(err):
		return zserrors.Wrap(zserrors.CodeIdentityProviderUnavailable, "", err)
	case killbill.IsUnavailable(err):
		return zserrors.Wrap(zserrors.CodeBillingUnavailable, "", err)
	case errors.As(err, &kcErr):
		return zserrors.Wrap(zserrors.CodeBrandApplyRejected, fmt.Sprintf("Keycloak rejected %s (HTTP %d).", kcErr.Op, kcErr.Status), err)
	case errors.Is(err, keycloak.ErrConflict), errors.Is(err, keycloak.ErrNotFound):
		return zserrors.Wrap(zserrors.CodeBrandApplyRejected, "Keycloak rejected a change: the object is missing or conflicts with another.", err)
	case errors.Is(err, killbill.ErrCredentialsRejected):
		return zserrors.Wrap(zserrors.CodeBrandApplyRejected, "Kill Bill rejected the credentials: check the admin settings and the tenant secret reference.", err)
	case errors.As(err, &kbErr):
		msg := fmt.Sprintf("Kill Bill rejected %s (HTTP %d", kbErr.Op, kbErr.Status)
		if kbErr.Code != 0 {
			msg += fmt.Sprintf(", error code %d", kbErr.Code)
		}
		return zserrors.Wrap(zserrors.CodeBrandApplyRejected, msg+").", err)
	case errors.Is(err, killbill.ErrConflict), errors.Is(err, killbill.ErrNotFound):
		return zserrors.Wrap(zserrors.CodeBrandApplyRejected, "Kill Bill rejected a change: the object is missing or conflicts with another.", err)
	}
	return zserrors.Wrap(zserrors.CodeInternal, "", err)
}

func codeName(err error) string {
	var zsErr *zserrors.Error
	if errors.As(err, &zsErr) {
		return zserrors.Lookup(zsErr.Code).Name
	}
	return "internal_error"
}

// secretEqual compares two secret values in constant time.
func secretEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}
