package brand

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/killbill"
	zserrors "github.com/zsoftly/zs-core/pkg/errors"
)

// User keys under which Core records, in the Kill Bill tenant itself, the
// SHA-256 of uploaded catalogue and overdue files and of the email
// configuration whose restart completed. Kill Bill keeps every catalogue
// version and rejects a version whose effective date already exists, so
// Core uploads only when the file changed. The catalogue key is the one
// deploy/compose/killbill/bootstrap.sh also writes.
const (
	catalogueDigestKey = "zs-core-catalogue-sha256"
	overdueDigestKey   = "zs-core-overdue-sha256"
	// Written only after the email plugin has restarted successfully.
	emailConfigDigestKey = "zs-core-email-config-sha256"
)

// Plugin kinds a brand uses.
const (
	pluginKindPayment = "payment"
	pluginKindInvoice = "invoice"
	pluginKindEmail   = "email"
)

func fileDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pluginUse is one plugin a brand uses: its pin, its kind, the diff field it
// shows under, and the configuration apply uploads for it under key.
type pluginUse struct {
	pin    components.Plugin
	kind   string
	field  string
	key    string
	config string
}

// brandPlugins checks every plugin the manifest names against
// components.yaml and its test-mode rule, and returns them with their
// configurations. It runs before anything is read from Kill Bill, so a
// plugin that cannot be used in test mode stops the apply with no side
// effects.
func brandPlugins(deps Deps, m *Manifest, sec resolved) ([]pluginUse, error) {
	var uses []pluginUse
	for i, pp := range m.Billing.Plugins() {
		pin, err := approvedPin(deps.PaymentPlugins, pluginKindPayment, "approved_payment_plugins", m.Billing.pluginsPath(i), pp.Name, pp.Version)
		if err != nil {
			return nil, err
		}
		if err := CheckTestMode(pp.Name, sec[pp.ConfigRef]); err != nil {
			return nil, err
		}
		field := "billing.payment_plugins." + pp.Name
		if m.Billing.PaymentPlugin != nil {
			field = "billing.payment_plugin"
		}
		uses = append(uses, pluginUse{pin: pin, kind: pluginKindPayment, field: field, key: pp.Key(), config: sec[pp.ConfigRef]})
	}
	if ip := m.Billing.InvoicePlugin; ip != nil {
		pin, err := approvedPin(deps.InvoicePlugins, pluginKindInvoice, "approved_invoice_plugins", "/billing/invoice_plugin", ip.Name, ip.Version)
		if err != nil {
			return nil, err
		}
		if err := CheckTestMode(ip.Name, sec[ip.ConfigRef]); err != nil {
			return nil, err
		}
		uses = append(uses, pluginUse{pin: pin, kind: pluginKindInvoice, field: "billing.invoice_plugin", key: ip.Key(), config: sec[ip.ConfigRef]})
	}
	if e := m.Billing.Email; e != nil {
		pin, err := approvedPin(deps.InstallablePlugins, pluginKindEmail, "installable", "/billing/email", EmailPluginName, "")
		if err != nil {
			return nil, err
		}
		config, err := emailPluginConfig(e, sec)
		if err != nil {
			return nil, err
		}
		uses = append(uses, pluginUse{pin: pin, kind: pluginKindEmail, field: "billing.email", key: emailPluginService, config: config})
	}
	return uses, nil
}

// checkPluginTables refuses, when deps.PluginTables is set, a plugin whose
// tables the Kill Bill database lacks: the check doctor runs. The command
// line sets it from ZS_KILLBILL_DATABASE_URL. serve never does.
func checkPluginTables(ctx context.Context, deps Deps, uses []pluginUse) error {
	if deps.PluginTables == nil {
		return nil
	}
	for _, u := range uses {
		missing, err := deps.PluginTables(ctx, u.pin)
		if err != nil {
			return zserrors.Wrap(zserrors.CodeDatabaseUnavailable, "The Kill Bill database could not be read to check the plugin tables.", err)
		}
		if len(missing) > 0 {
			return zserrors.New(zserrors.CodePluginTablesMissing, fmt.Sprintf(
				"The Kill Bill database lacks the tables of plugin %s %s: %s. Run zs-core plugin ddl apply %s first.",
				u.pin.Name, u.pin.Version, strings.Join(missing, ", "), u.pin.Name))
		}
	}
	return nil
}

func planKillBill(ctx context.Context, deps Deps, p *plan, m *Manifest, sec resolved, fs files) ([]Change, error) {
	kb := deps.KillBill
	creds := killbill.TenantCredentials{APIKey: m.Billing.KillBillTenantKey, APISecret: sec[m.Billing.KillBillAPISecretRef]}
	var changes []Change

	uses, err := brandPlugins(deps, m, sec)
	if err != nil {
		return nil, err
	}
	if err := checkPluginTables(ctx, deps, uses); err != nil {
		return nil, err
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

	docs, err := planInvoiceDocuments(ctx, kb, creds, exists, m, fs)
	if err != nil {
		return nil, err
	}
	changes = append(changes, docs...)

	installed := map[string]bool{}
	for _, u := range uses {
		if installed[u.pin.Name] {
			continue
		}
		installed[u.pin.Name] = true
		install, err := planPlugin(ctx, deps, u)
		if err != nil {
			return nil, err
		}
		changes = append(changes, install...)
	}

	for _, u := range uses {
		var have string
		var found bool
		if exists {
			have, found, err = kb.PluginConfig(ctx, creds, u.key)
			if err != nil {
				return nil, err
			}
		}
		configMatches := found && secretEqual(have, u.config)
		restarted := true
		if u.kind == pluginKindEmail {
			restarted = false
			if exists {
				digest, _, err := kb.TenantValue(ctx, creds, emailConfigDigestKey)
				if err != nil {
					return nil, err
				}
				restarted = secretEqual(digest, fileDigest([]byte(u.config)))
			}
		}
		if configMatches && restarted {
			continue
		}
		action := ActionUpdate
		if !found {
			action = ActionCreate
		}
		u := u
		changes = append(changes, Change{
			Target: TargetPluginConfig, Name: u.key, Action: action,
			Fields: []FieldChange{{Field: u.field + ".config", Redacted: true}},
			run: func(ctx context.Context, st *runState) error {
				if !configMatches {
					if err := kb.UploadPluginConfig(ctx, creds, st.callContext(), u.key, u.config); err != nil {
						return err
					}
				}
				if u.kind == pluginKindEmail {
					if err := restartPlugin(ctx, st, u.pin); err != nil {
						return err
					}
					return kb.SetUserKeyValue(ctx, creds, st.callContext(), emailConfigDigestKey, fileDigest([]byte(u.config)))
				}
				return nil
			},
		})
	}

	email, err := planEmailTemplates(ctx, kb, creds, exists, m, fs)
	if err != nil {
		return nil, err
	}
	changes = append(changes, email...)

	callback, err := planCallback(ctx, deps, creds, p.brandID, exists)
	if err != nil {
		return nil, err
	}
	return append(changes, callback...), nil
}

// restartPlugin reloads a plugin after Core changes its email configuration.
// Separate STOP_PLUGIN and START_PLUGIN commands expose persisted STOPPED then
// RUNNING states, unlike combined RESTART_PLUGIN. Completion requires the
// original node set to reach each state.
func restartPlugin(ctx context.Context, st *runState, pin components.Plugin) error {
	kb := st.deps.KillBill
	nodes, err := kb.NodesInfo(ctx)
	if err != nil {
		return err
	}
	targets := map[string]bool{}
	for _, n := range nodes {
		if n.NodeName == "" || targets[n.NodeName] {
			return &killbill.Unavailable{Op: "restart plugin " + pin.Name, Err: errors.New("Kill Bill returned an ambiguous node inventory")}
		}
		targets[n.NodeName] = true
	}
	if len(targets) == 0 {
		return &killbill.Unavailable{Op: "restart plugin " + pin.Name, Err: errors.New("Kill Bill reports no target nodes")}
	}
	if err := kb.SendNodeCommand(ctx, st.callContext(), killbill.NodeCommand{
		NodeCommandType: killbill.NodeCommandStopPlugin,
		NodeCommandProperties: []killbill.NodeCommandProperty{
			{Key: "pluginKey", Value: pin.Name}, {Key: "pluginVersion", Value: pin.Version},
		},
	}); err != nil {
		return err
	}
	// STOP_PLUGIN is asynchronous. Every original target must currently report
	// the pinned version STOPPED before START_PLUGIN is sent.
	stopCtx, cancel := context.WithTimeout(ctx, restartSettle)
	_, err = waitForPluginNodes(stopCtx, kb, pin, targets, func(_ string, p killbill.PluginInfo) bool { return p.State == pluginStateStopped })
	cancel()
	if err != nil {
		return err
	}
	if err := kb.SendNodeCommand(ctx, st.callContext(), killbill.NodeCommand{
		NodeCommandType: killbill.NodeCommandStartPlugin,
		NodeCommandProperties: []killbill.NodeCommandProperty{
			{Key: "pluginKey", Value: pin.Name}, {Key: "pluginVersion", Value: pin.Version},
		},
	}); err != nil {
		return err
	}
	_, err = waitForPluginNodes(ctx, kb, pin, targets, func(_ string, p killbill.PluginInfo) bool { return p.State == pluginStateRunning })
	return err
}

// restartSettle bounds how long restartPlugin waits to observe every stop.
var restartSettle = 15 * time.Second
