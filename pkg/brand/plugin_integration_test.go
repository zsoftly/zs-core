//go:build integration

package brand

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/killbill"
	"github.com/zsoftly/zs-core/internal/postgrestest"
)

// kpmStub serves the node and callback endpoints brand apply uses, with the
// behaviour of Kill Bill 0.24.22: INSTALL_PLUGIN adds the plugin STOPPED,
// START_PLUGIN starts it, and registering a callback replaces the previous
// one.
type kpmStub struct {
	mu        sync.Mutex
	plugins   []killbill.PluginInfo
	callbacks []string
	commands  []string
	uris      []string
}

func (k *kpmStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case r.URL.Path == "/1.0/kb/nodesInfo" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode([]killbill.NodeInfo{{NodeName: "node-1", KBVersion: "0.24.22", PluginsInfo: k.plugins}})
	case r.URL.Path == "/1.0/kb/nodesInfo" && r.Method == http.MethodPost:
		var cmd killbill.NodeCommand
		_ = json.NewDecoder(r.Body).Decode(&cmd)
		props := map[string]string{}
		for _, p := range cmd.NodeCommandProperties {
			props[p.Key] = p.Value
		}
		k.commands = append(k.commands, cmd.NodeCommandType)
		switch cmd.NodeCommandType {
		case killbill.NodeCommandInstallPlugin:
			k.uris = append(k.uris, props["pluginUri"])
			k.plugins = append(k.plugins, killbill.PluginInfo{PluginKey: props["pluginKey"], Version: props["pluginVersion"], State: "STOPPED"})
		case killbill.NodeCommandStartPlugin:
			for i := range k.plugins {
				if k.plugins[i].PluginKey == props["pluginKey"] {
					k.plugins[i].State = "RUNNING"
				}
			}
		}
		w.WriteHeader(http.StatusAccepted)
	case r.URL.Path == "/1.0/kb/tenants/registerNotificationCallback" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"key": "PUSH_NOTIFICATION_CB", "values": k.callbacks})
	case r.URL.Path == "/1.0/kb/tenants/registerNotificationCallback" && r.Method == http.MethodPost:
		k.callbacks = []string{r.URL.Query().Get("cb")}
		w.WriteHeader(http.StatusCreated)
	default:
		http.NotFound(w, r)
	}
}

func TestPluginInstallAndCallbackRegistration(t *testing.T) {
	ctx := context.Background()
	db := postgrestest.New(t)
	stub := &kpmStub{}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	pins, err := components.Load("../../components.yaml")
	if err != nil {
		t.Fatal(err)
	}
	prevPoll := pluginPoll
	pluginPoll = 10 * time.Millisecond
	t.Cleanup(func() { pluginPoll = prevPoll })
	// The repository serves a stand-in jar over TLS. Its digest replaces
	// the pin of components.yaml.
	jar := []byte("stand-in stripe plugin jar")
	repo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(jar) }))
	t.Cleanup(repo.Close)
	sum := sha256.Sum256(jar)
	plugins := append([]components.Plugin(nil), pins.KillBillPlugins.ApprovedPaymentPlugins...)
	for i := range plugins {
		if plugins[i].Name == "stripe" {
			plugins[i].Sha256 = hex.EncodeToString(sum[:])
		}
	}
	deps := Deps{DB: db.Pool, KillBill: kb, PaymentPlugins: plugins, PluginRepository: repo.URL + "/maven2", PluginHTTP: repo.Client(),
		WebhookBaseURL: "http://core:8080/", WebhookSecret: []byte(strings.Repeat("w", 32))}
	m := brandManifest(t, "zs-plugin", "env://BRAND_ZS_PLUGIN__STAFF")
	m.Billing.PaymentPlugin = &PaymentPlugin{Name: "stripe", Mode: PaymentModeTest, ConfigRef: "env://BRAND_ZS_PLUGIN__STRIPE"}
	brandID := insertBrand(t, db, m)
	pin, err := approvedPlugin(deps.PaymentPlugins, m.Billing.PaymentPlugin)
	if err != nil {
		t.Fatal(err)
	}
	st := &runState{deps: deps, opts: integrationActor, manifest: m, brandID: brandID}

	// A jar that does not match the pin is never installed.
	wrong := pin
	wrong.Sha256 = strings.Repeat("0", 64)
	changes, err := planPlugin(ctx, deps, m, wrong)
	if err != nil || len(changes) != 1 {
		t.Fatalf("plan = %+v %v", changes, err)
	}
	if err := changes[0].run(ctx, st); codeOf(err) != "brand_apply_rejected" || len(stub.commands) != 0 {
		t.Fatalf("mismatched jar = %v, commands %v", err, stub.commands)
	}

	changes, err = planPlugin(ctx, deps, m, pin)
	if err != nil || len(changes) != 1 || changes[0].Action != ActionCreate || changes[0].Target != TargetPlugin {
		t.Fatalf("plan = %+v %v", changes, err)
	}
	if err := changes[0].run(ctx, st); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stub.commands, ",") != "INSTALL_PLUGIN,START_PLUGIN" ||
		stub.uris[0] != repo.URL+"/maven2/org/kill-bill/billing/plugin/java/stripe-plugin/8.0.3/stripe-plugin-8.0.3.jar" {
		t.Fatalf("commands = %v %v", stub.commands, stub.uris)
	}
	var node, version string
	var started *time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT node, version, started_at FROM killbill_plugin_installs WHERE plugin = 'stripe' AND brand_id = $1`, brandID).Scan(&node, &version, &started); err != nil ||
		node != "node-1" || version != "8.0.3" || started == nil {
		t.Fatalf("install record = %s %s %v %v", node, version, started, err)
	}
	if changes, err := planPlugin(ctx, deps, m, pin); err != nil || len(changes) != 0 {
		t.Fatalf("plan after install = %+v %v", changes, err)
	}
	// A stopped plugin is started, not installed again.
	stub.plugins[0].State = "STOPPED"
	changes, err = planPlugin(ctx, deps, m, pin)
	if err != nil || len(changes) != 1 || changes[0].Action != ActionUpdate {
		t.Fatalf("plan for a stopped plugin = %+v %v", changes, err)
	}
	// Another installed version is refused.
	stub.plugins[0].Version = "7.0.0"
	if _, err := planPlugin(ctx, deps, m, pin); codeOf(err) != "brand_apply_rejected" {
		t.Fatalf("other installed version: %v", err)
	}

	creds := killbill.TenantCredentials{APIKey: "zs-plugin", APISecret: "s"}
	cb, err := planCallback(ctx, deps, creds, brandID, true)
	if err != nil || len(cb) != 1 || cb[0].Action != ActionCreate || !cb[0].Fields[0].Redacted {
		t.Fatalf("callback plan = %+v %v", cb, err)
	}
	if err := cb[0].run(ctx, st); err != nil {
		t.Fatal(err)
	}
	want := "http://core:8080" + WebhookPath(deps.WebhookSecret, brandID)
	if len(stub.callbacks) != 1 || stub.callbacks[0] != want {
		t.Fatalf("callbacks = %v, want %s", stub.callbacks, want)
	}
	if cb, err := planCallback(ctx, deps, creds, brandID, true); err != nil || len(cb) != 0 {
		t.Fatalf("callback plan after registration = %+v %v", cb, err)
	}
	// Another party's callback is replaced: Kill Bill keeps one.
	stub.callbacks = []string{"http://elsewhere.example.test/cb"}
	if cb, err := planCallback(ctx, deps, creds, brandID, true); err != nil || len(cb) != 1 || cb[0].Action != ActionUpdate {
		t.Fatalf("callback plan with another callback = %+v %v", cb, err)
	}
	noURL := deps
	noURL.WebhookBaseURL = ""
	if cb, err := planCallback(ctx, noURL, creds, brandID, true); err != nil || len(cb) != 0 {
		t.Fatalf("callback plan without a base URL = %+v %v", cb, err)
	}
}
