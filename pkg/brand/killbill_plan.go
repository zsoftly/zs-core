package brand

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/killbill"
)

// User keys under which Core records, in the Kill Bill tenant itself, the
// SHA-256 of the catalogue and overdue files it last uploaded. Kill Bill
// keeps every catalogue version and rejects a version whose effective date
// already exists, so Core uploads only when the file changed. The catalogue
// key is the one deploy/compose/killbill/bootstrap.sh also writes.
const (
	catalogueDigestKey = "zs-core-catalogue-sha256"
	overdueDigestKey   = "zs-core-overdue-sha256"
)

func fileDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func planKillBill(ctx context.Context, deps Deps, p *plan, m *Manifest, sec resolved, fs files) ([]Change, error) {
	kb := deps.KillBill
	creds := killbill.TenantCredentials{APIKey: m.Billing.KillBillTenantKey, APISecret: sec[m.Billing.KillBillAPISecretRef]}
	var changes []Change

	// The payment plugin is checked before anything else is read, so a
	// plugin that cannot be used in test mode stops the apply with no side
	// effects.
	var pin *components.Plugin
	if pp := m.Billing.PaymentPlugin; pp != nil {
		approved, err := approvedPlugin(deps.PaymentPlugins, pp)
		if err != nil {
			return nil, err
		}
		if err := CheckTestMode(pp.Name, sec[pp.ConfigRef]); err != nil {
			return nil, err
		}
		pin = &approved
	}

	tenant, err := kb.TenantByAPIKey(ctx, creds.APIKey)
	exists := true
	switch {
	case errors.Is(err, killbill.ErrNotFound):
		exists = false
	case err != nil:
		return nil, err
	default:
		// The tenant exists: the secret the manifest references must be
		// its secret. Core never overwrites another party's tenant.
		if err := kb.CheckTenantCredentials(ctx, creds); err != nil {
			return nil, err
		}
	}

	if !exists {
		changes = append(changes, Change{
			Target: TargetKillBillTenant, Name: creds.APIKey, Action: ActionCreate,
			Fields: []FieldChange{
				{Field: "billing.killbill_tenant_key", After: creds.APIKey},
				{Field: "billing.killbill_api_secret", Redacted: true},
			},
			run: func(ctx context.Context, st *runState) error {
				t, err := kb.CreateTenant(ctx, st.callContext(), creds, m.Slug)
				if errors.Is(err, killbill.ErrConflict) {
					t, err = kb.TenantByAPIKey(ctx, creds.APIKey)
				}
				if err != nil {
					return err
				}
				return inTx(ctx, st.deps.DB, func(tx pgx.Tx) error { return setKillBillTenant(ctx, tx, st.brandID, t.TenantID) })
			},
		})
	} else if p.record == nil || p.record.KillBillTenantID != tenant.TenantID {
		before := any(nil)
		if p.record != nil && p.record.KillBillTenantID != "" {
			before = p.record.KillBillTenantID
		}
		tenantID := tenant.TenantID
		changes = append(changes, Change{
			Target: TargetBrand, Name: m.Slug, Action: ActionUpdate,
			Fields: []FieldChange{{Field: "killbill_tenant_id", Before: before, After: tenantID}},
			run: func(ctx context.Context, st *runState) error {
				return inTx(ctx, st.deps.DB, func(tx pgx.Tx) error { return setKillBillTenant(ctx, tx, st.brandID, tenantID) })
			},
		})
	}

	for _, doc := range []struct {
		target, field, path, key string
		upload                   func(context.Context, killbill.TenantCredentials, killbill.CallContext, []byte) error
	}{
		{TargetCatalogue, "billing.catalogue", m.Billing.Catalogue, catalogueDigestKey, kb.UploadCatalogue},
		{TargetOverdue, "billing.overdue", m.Billing.Overdue, overdueDigestKey, kb.UploadOverdue},
	} {
		if doc.path == "" {
			continue
		}
		data := fs[doc.path]
		want := fileDigest(data)
		var have string
		if exists {
			values, err := kb.UserKeyValues(ctx, creds, doc.key)
			if err != nil {
				return nil, err
			}
			if len(values) > 0 {
				have = values[len(values)-1]
			}
		}
		if have == want {
			continue
		}
		doc := doc
		changes = append(changes, Change{
			Target: doc.target, Name: doc.path, Action: ActionUpdate,
			Fields: []FieldChange{{Field: doc.field + ".sha256", Before: nilIfEmpty(have), After: want}},
			run: func(ctx context.Context, st *runState) error {
				cc := st.callContext()
				if err := doc.upload(ctx, creds, cc, data); err != nil {
					return err
				}
				return kb.SetUserKeyValue(ctx, creds, cc, doc.key, want)
			},
		})
	}

	if pin != nil {
		install, err := planPlugin(ctx, deps, m, *pin)
		if err != nil {
			return nil, err
		}
		changes = append(changes, install...)
	}

	if pp := m.Billing.PaymentPlugin; pp != nil {
		want := sec[pp.ConfigRef]
		var have string
		var found bool
		if exists {
			have, found, err = kb.PluginConfig(ctx, creds, pp.Key())
			if err != nil {
				return nil, err
			}
		}
		if !found || !secretEqual(have, want) {
			key := pp.Key()
			action := ActionUpdate
			if !found {
				action = ActionCreate
			}
			changes = append(changes, Change{
				Target: TargetPluginConfig, Name: key, Action: action,
				Fields: []FieldChange{{Field: "billing.payment_plugin.config", Redacted: true}},
				run: func(ctx context.Context, st *runState) error {
					return kb.UploadPluginConfig(ctx, creds, st.callContext(), key, want)
				},
			})
		}
	}
	callback, err := planCallback(ctx, deps, creds, p.brandID, exists)
	if err != nil {
		return nil, err
	}
	return append(changes, callback...), nil
}
