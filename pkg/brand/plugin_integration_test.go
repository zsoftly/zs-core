//go:build integration

package brand

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/components"
	"github.com/zsoftly/zs-core/internal/kbplugin"
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

// This planner regression needs only HTTP fixtures, not a database or stack.
func TestEmailConfigurationRetriesUntilRestartAcknowledged(t *testing.T) {
	previousPoll, previousSettle := pluginPoll, restartSettle
	pluginPoll, restartSettle = time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pluginPoll, restartSettle = previousPoll, previousSettle })
	for _, failure := range []string{
		"stop command", "start command", "marker", "existing configuration without marker",
		"stop stays running", "one node keeps running", "stops never overlap", "start stays stopped",
		"original node disappears", "new node appears", "duplicate node appears",
		"original node disappears after start", "new node appears after start", "duplicate node appears after start",
	} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			pins, err := components.Load("../../components.yaml")
			if err != nil {
				t.Fatal(err)
			}
			pin, ok := pins.PinnedPlugin(EmailPluginName)
			if !ok {
				t.Fatal("email plugin is not pinned")
			}
			m := &Manifest{Slug: "zs-email", Billing: Billing{KillBillTenantKey: "zs-email", KillBillAPISecretRef: "test-secret",
				Email: &Email{Sender: "billing@example.test", SMTP: SMTP{Host: "mailpit", Port: 1025}}}}
			want, err := emailPluginConfig(m.Billing.Email, nil)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var config, marker string
			var uploads, stops, starts, phaseReads, inventoryChanges int
			var partialStops, allStops, allRunning int
			var commands []string
			var commandErrors []string
			var phase string
			recovered := false
			twoNodes := failure == "one node keeps running" || failure == "stops never overlap" || strings.Contains(failure, "node appears") || strings.Contains(failure, "node disappears")
			states := [2]string{"RUNNING", "RUNNING"}
			lastNodeCount := 1
			if twoNodes {
				lastNodeCount = 2
			}
			if failure == "existing configuration without marker" {
				config = want
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/1.0/kb/tenants":
					_ = json.NewEncoder(w).Encode(killbill.Tenant{TenantID: "email-tenant", APIKey: "zs-email"})
				case r.Method == http.MethodGet && r.URL.Path == "/1.0/kb/nodesInfo":
					if phase == "stop" {
						phaseReads++
						states[0] = "STOPPED"
						if twoNodes {
							states[1] = "RUNNING"
							if phaseReads > 1 {
								states[1] = "STOPPED"
							}
						}
						if !recovered {
							switch failure {
							case "stop stays running":
								states[0] = "RUNNING"
							case "one node keeps running":
								states[1] = "RUNNING"
							case "stops never overlap":
								states = [2]string{"STOPPED", "RUNNING"}
								if phaseReads%2 == 0 {
									states = [2]string{"RUNNING", "STOPPED"}
								}
							}
						}
					} else if phase == "start" {
						phaseReads++
						states[0] = "RUNNING"
						if twoNodes && phaseReads > 1 {
							states[1] = "RUNNING"
						}
						if failure == "start stays stopped" && !recovered {
							states[0] = "STOPPED"
						}
					}
					nodes := []killbill.NodeInfo{{NodeName: "node-1", PluginsInfo: []killbill.PluginInfo{{PluginKey: pin.Name, Version: pin.Version, State: states[0]}}}}
					if twoNodes {
						nodes = append(nodes, killbill.NodeInfo{NodeName: "node-2", PluginsInfo: []killbill.PluginInfo{{PluginKey: pin.Name, Version: pin.Version, State: states[1]}}})
					}
					changePhase := "stop"
					if strings.HasSuffix(failure, " after start") {
						changePhase = "start"
					}
					if !recovered && phase == changePhase && phaseReads > 1 {
						switch strings.TrimSuffix(failure, " after start") {
						case "original node disappears":
							nodes = nodes[:1]
							inventoryChanges++
						case "new node appears":
							nodes = append(nodes, killbill.NodeInfo{NodeName: "node-3", PluginsInfo: nodes[1].PluginsInfo})
							inventoryChanges++
						case "duplicate node appears":
							nodes[1].NodeName = "node-1"
							inventoryChanges++
						}
					}
					lastNodeCount = len(nodes)
					if phase == "stop" {
						if states[0] == "STOPPED" && (!twoNodes || states[1] == "STOPPED") {
							allStops++
						} else if twoNodes && (states[0] == "STOPPED" || states[1] == "STOPPED") {
							partialStops++
						}
					} else if phase == "start" && states[0] == "RUNNING" && (!twoNodes || states[1] == "RUNNING") {
						allRunning++
					}
					_ = json.NewEncoder(w).Encode(nodes)
				case r.Method == http.MethodPost && r.URL.Path == "/1.0/kb/nodesInfo":
					var cmd killbill.NodeCommand
					if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
						commandErrors = append(commandErrors, "invalid command body")
						http.Error(w, "invalid command body", http.StatusBadRequest)
						return
					}
					commands = append(commands, cmd.NodeCommandType)
					props := map[string]string{}
					for _, p := range cmd.NodeCommandProperties {
						props[p.Key] = p.Value
					}
					if cmd.IsSystemCommandType || len(cmd.NodeCommandProperties) != 2 || len(props) != 2 || props["pluginKey"] != pin.Name || props["pluginVersion"] != pin.Version {
						commandErrors = append(commandErrors, "incorrect lifecycle command scope")
						http.Error(w, "incorrect lifecycle command scope", http.StatusBadRequest)
						return
					}
					switch cmd.NodeCommandType {
					case killbill.NodeCommandStopPlugin:
						stops++
						if failure == "stop command" && !recovered {
							http.Error(w, "stop failed", http.StatusServiceUnavailable)
							return
						}
						phase, phaseReads = "stop", 0
					case killbill.NodeCommandStartPlugin:
						starts++
						if phase != "stop" || states[0] != "STOPPED" || (twoNodes && (states[1] != "STOPPED" || lastNodeCount != 2)) {
							commandErrors = append(commandErrors, "START preceded a complete STOPPED observation")
							http.Error(w, "START preceded a complete STOPPED observation", http.StatusBadRequest)
							return
						}
						if failure == "start command" && !recovered {
							http.Error(w, "start failed", http.StatusServiceUnavailable)
							return
						}
						phase, phaseReads = "start", 0
					default:
						commandErrors = append(commandErrors, "unexpected command: "+cmd.NodeCommandType)
						http.Error(w, "expected separate STOP and START commands", http.StatusBadRequest)
						return
					}
					w.WriteHeader(http.StatusAccepted)
				case r.URL.Path == "/1.0/kb/tenants/uploadPluginConfig/"+emailPluginService:
					switch r.Method {
					case http.MethodGet:
						var values []string
						if config != "" {
							values = []string{config}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"values": values})
					case http.MethodPost:
						data, err := io.ReadAll(r.Body)
						if err != nil {
							http.Error(w, "read config", http.StatusBadRequest)
							return
						}
						config = string(data)
						uploads++
						w.WriteHeader(http.StatusCreated)
					default:
						http.NotFound(w, r)
					}
				case r.URL.Path == "/1.0/kb/tenants/userKeyValue/"+emailConfigDigestKey:
					switch r.Method {
					case http.MethodGet:
						var values []string
						if marker != "" {
							values = []string{marker}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"values": values})
					case http.MethodDelete:
						marker = ""
						w.WriteHeader(http.StatusNoContent)
					case http.MethodPost:
						if failure == "marker" && !recovered {
							http.Error(w, "marker failed", http.StatusServiceUnavailable)
							return
						}
						if phase != "start" || allRunning == 0 || states[0] != "RUNNING" || (twoNodes && states[1] != "RUNNING") {
							commandErrors = append(commandErrors, "marker preceded completed start")
							http.Error(w, "marker preceded completed start", http.StatusBadRequest)
							return
						}
						data, _ := io.ReadAll(r.Body)
						marker = string(data)
						w.WriteHeader(http.StatusCreated)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/1.0/kb/tenants/userKeyValue/zs-core.credential-check":
					_ = json.NewEncoder(w).Encode(map[string]any{"values": []string{}})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			kb, err := killbill.New(killbill.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "test"})
			if err != nil {
				t.Fatal(err)
			}
			deps := Deps{KillBill: kb, InstallablePlugins: []components.Plugin{pin}}
			p := &plan{record: &Record{KillBillTenantID: "email-tenant"}}
			st := &runState{deps: deps, opts: integrationActor, manifest: m}
			planConfig := func() []Change {
				t.Helper()
				mu.Lock()
				wantStart := states[0] != "RUNNING" || (twoNodes && states[1] != "RUNNING")
				mu.Unlock()
				changes, err := planKillBill(ctx, deps, p, m, resolved{"test-secret": "test"}, nil)
				if err != nil {
					t.Fatal(err)
				}
				var configs []Change
				startChanges := 0
				for _, c := range changes {
					if c.Target == TargetPlugin {
						if !wantStart || c.Name != pin.Name || c.Action != ActionUpdate || len(c.Fields) != 1 || c.Fields[0].Field != "billing.email.state" || c.Fields[0].Before != "stopped" || c.Fields[0].After != "running" {
							t.Fatalf("unexpected recovery start change: %+v", c)
						}
						startChanges++
						continue
					}
					if c.Target != TargetPluginConfig || c.Name != emailPluginService || len(c.Fields) != 1 || !c.Fields[0].Redacted || c.Fields[0].Before != nil || c.Fields[0].After != nil {
						t.Fatalf("unexpected or unredacted change: %+v", c)
					}
					configs = append(configs, c)
				}
				if (wantStart && startChanges != 1) || (!wantStart && startChanges != 0) {
					t.Fatalf("recovery start changes %d, wanted %v", startChanges, wantStart)
				}
				// Execute the email change directly. The generic install/start change
				// also writes a database record, covered by the DB-backed fixture below.
				return configs
			}
			changes := planConfig()
			if len(changes) != 1 {
				t.Fatalf("initial pending restart changes = %d", len(changes))
			}
			runChange := func() error {
				t.Helper()
				limit := 2 * time.Second
				if failure == "start stays stopped" {
					limit = 200 * time.Millisecond
				}
				runCtx, cancel := context.WithTimeout(ctx, limit)
				defer cancel()
				return changes[0].run(runCtx, st)
			}
			firstStarts := 0
			if failure == "start command" || failure == "start stays stopped" || failure == "marker" || strings.HasSuffix(failure, " after start") {
				firstStarts = 1
			}
			if failure != "existing configuration without marker" {
				runErr := runChange()
				if runErr == nil {
					t.Fatal("incomplete stop/start or marker write was accepted")
				}
				mu.Lock()
				uploaded, acknowledged := config == want, marker != ""
				gotStops, gotStarts := stops, starts
				gotPartial, gotAllStops, gotInventory := partialStops, allStops, inventoryChanges
				gotCommandErrors := append([]string(nil), commandErrors...)
				// Restore the original node set before planning the retry.
				recovered, phase = true, ""
				mu.Unlock()
				if !uploaded || acknowledged {
					t.Fatalf("after failure: configuration uploaded %v, restart acknowledged %v", uploaded, acknowledged)
				}
				if gotStops != 1 || gotStarts != firstStarts || len(gotCommandErrors) != 0 {
					t.Fatalf("failed attempt commands: stop %d, start %d, errors %v", gotStops, gotStarts, gotCommandErrors)
				}
				if (failure == "one node keeps running" || failure == "stops never overlap") && (gotPartial == 0 || gotAllStops != 0) {
					t.Fatalf("expected partial stops without simultaneous completion: partial %d, complete %d", gotPartial, gotAllStops)
				}
				if failure == "stop stays running" && gotAllStops != 0 {
					t.Fatal("unchanged RUNNING node was counted as stopped")
				}
				if strings.Contains(failure, "node appears") || strings.Contains(failure, "node disappears") {
					wantError := "node set changed"
					if strings.HasPrefix(failure, "original node disappears") {
						wantError = "node disappeared"
					}
					if gotInventory == 0 || !strings.Contains(runErr.Error(), wantError) {
						t.Fatalf("changed targets did not produce the inventory failure: changes %d, error %v", gotInventory, runErr)
					}
				}
				changes = planConfig()
				if len(changes) != 1 {
					t.Fatalf("retry after partial success planned %d changes", len(changes))
				}
			}
			if err := runChange(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			gotUploads, gotStops, gotStarts, gotMarker := uploads, stops, starts, marker
			gotPartial, gotAllStops, gotRunning := partialStops, allStops, allRunning
			gotCommands := append([]string(nil), commands...)
			gotCommandErrors := append([]string(nil), commandErrors...)
			mu.Unlock()
			wantUploads, wantStops, wantStarts := 1, 2, firstStarts+1
			wantCommands := []string{killbill.NodeCommandStopPlugin}
			if firstStarts == 1 {
				wantCommands = append(wantCommands, killbill.NodeCommandStartPlugin)
			}
			if failure == "existing configuration without marker" {
				wantUploads, wantStops, wantStarts = 0, 1, 1
				wantCommands = nil
			}
			wantCommands = append(wantCommands, killbill.NodeCommandStopPlugin, killbill.NodeCommandStartPlugin)
			if gotUploads != wantUploads || gotStops != wantStops || gotStarts != wantStarts || gotMarker != fileDigest([]byte(want)) {
				t.Fatalf("uploads %d, stops %d, starts %d, marker %q", gotUploads, gotStops, gotStarts, gotMarker)
			}
			if strings.Join(gotCommands, ",") != strings.Join(wantCommands, ",") || len(gotCommandErrors) != 0 {
				t.Fatalf("commands %v, want %v, errors %v", gotCommands, wantCommands, gotCommandErrors)
			}
			if gotAllStops == 0 || gotRunning == 0 || (twoNodes && gotPartial == 0) {
				t.Fatalf("retry lacks observed lifecycle progress: partial stops %d, all stopped %d, all running %d", gotPartial, gotAllStops, gotRunning)
			}
			if changes := planConfig(); len(changes) != 0 {
				t.Fatalf("acknowledged configuration still has changes: %+v", changes)
			}
		})
	}
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
	// The mirror serves a stand-in jar. New installs must never read the
	// repository directly, even if it is configured and reachable.
	jar := []byte("stand-in stripe plugin jar")
	var repoHits atomic.Int32
	repo := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repoHits.Add(1)
		http.Error(w, "direct repository installs are forbidden", http.StatusBadGateway)
	}))
	t.Cleanup(repo.Close)
	var mirrorHits atomic.Int32
	mirror := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/plugins/stripe/8.0.3.jar" {
			http.NotFound(w, r)
			return
		}
		mirrorHits.Add(1)
		_, _ = w.Write(jar)
	}))
	t.Cleanup(mirror.Close)
	sum := sha256.Sum256(jar)
	plugins := append([]components.Plugin(nil), pins.KillBillPlugins.ApprovedPaymentPlugins...)
	for i := range plugins {
		if plugins[i].Name == "stripe" {
			plugins[i].Sha256 = hex.EncodeToString(sum[:])
		}
	}
	deps := Deps{DB: db.Pool, KillBill: kb, PaymentPlugins: plugins, PluginRepository: repo.URL + "/maven2", PluginHTTP: mirror.Client(),
		PluginMirrorURL: mirror.URL, PluginMirrorSecret: []byte(strings.Repeat("m", 32)),
		WebhookBaseURL: "http://core:8080/", WebhookSecret: []byte(strings.Repeat("w", 32))}
	m := brandManifest(t, "zs-plugin", "env://BRAND_ZS_PLUGIN__STAFF")
	m.Billing.PaymentPlugin = &PaymentPlugin{Name: "stripe", Mode: PaymentModeTest, ConfigRef: "env://BRAND_ZS_PLUGIN__STRIPE"}
	brandID := insertBrand(t, db, m)
	pin, err := approvedPlugin(deps.PaymentPlugins, m.Billing.PaymentPlugin)
	if err != nil {
		t.Fatal(err)
	}
	st := &runState{deps: deps, opts: integrationActor, manifest: m, brandID: brandID}
	use := func(p components.Plugin) pluginUse {
		return pluginUse{pin: p, kind: pluginKindPayment, field: "billing.payment_plugin"}
	}
	noMirror := deps
	noMirror.PluginMirrorURL, noMirror.PluginMirrorSecret = "", nil
	if changes, err := planPlugin(ctx, noMirror, use(pin)); codeOf(err) != "plugin_unavailable" || len(changes) != 0 || len(stub.commands) != 0 {
		t.Fatalf("new install without a mirror = %v, changes %v, commands %v", err, changes, stub.commands)
	}

	// A jar that does not match the pin is never installed.
	wrong := pin
	wrong.Sha256 = strings.Repeat("0", 64)
	changes, err := planPlugin(ctx, deps, use(wrong))
	if err != nil || len(changes) != 1 {
		t.Fatalf("plan = %+v %v", changes, err)
	}
	if err := changes[0].run(ctx, st); codeOf(err) != "brand_apply_rejected" || len(stub.commands) != 0 {
		t.Fatalf("mismatched jar = %v, commands %v", err, stub.commands)
	}

	changes, err = planPlugin(ctx, deps, use(pin))
	if err != nil || len(changes) != 1 || changes[0].Action != ActionCreate || changes[0].Target != TargetPlugin {
		t.Fatalf("plan = %+v %v", changes, err)
	}
	if err := changes[0].run(ctx, st); err != nil {
		t.Fatal(err)
	}
	wantURI := mirror.URL + "/v1/plugins/stripe/8.0.3.jar?sha256=" + pin.Sha256 + "&token=" + kbplugin.Token(deps.PluginMirrorSecret, "stripe", "8.0.3", pin.Sha256)
	if strings.Join(stub.commands, ",") != "INSTALL_PLUGIN,START_PLUGIN" || len(stub.uris) != 1 ||
		stub.uris[0] != wantURI || repoHits.Load() != 0 {
		t.Fatalf("commands = %v %v", stub.commands, stub.uris)
	}
	var node, version string
	var started *time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT node, version, started_at FROM killbill_plugin_installs WHERE plugin = 'stripe' AND brand_id = $1`, brandID).Scan(&node, &version, &started); err != nil ||
		node != "node-1" || version != "8.0.3" || started == nil {
		t.Fatalf("install record = %s %s %v %v", node, version, started, err)
	}
	if changes, err := planPlugin(ctx, noMirror, use(pin)); err != nil || len(changes) != 0 {
		t.Fatalf("plan after install = %+v %v", changes, err)
	}
	// A stopped plugin starts without a mirror or another download.
	stub.plugins[0].State = "STOPPED"
	changes, err = planPlugin(ctx, noMirror, use(pin))
	if err != nil || len(changes) != 1 || changes[0].Action != ActionUpdate {
		t.Fatalf("plan for a stopped plugin = %+v %v", changes, err)
	}
	beforeStart := mirrorHits.Load()
	if err := changes[0].run(ctx, &runState{deps: noMirror, opts: integrationActor, manifest: m, brandID: brandID}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stub.commands, ",") != "INSTALL_PLUGIN,START_PLUGIN,START_PLUGIN" || mirrorHits.Load() != beforeStart || repoHits.Load() != 0 {
		t.Fatalf("start-only operation downloaded or installed again: %v", stub.commands)
	}
	// Another installed version is refused.
	stub.plugins[0].Version = "7.0.0"
	if _, err := planPlugin(ctx, deps, use(pin)); codeOf(err) != "brand_apply_rejected" {
		t.Fatalf("other installed version: %v", err)
	}

	// A later mirror install keeps the expected digest and token bound to
	// the KPM download URL and still makes no repository request.
	stub.plugins, stub.commands, stub.uris = nil, nil, nil
	hits := repoHits.Load()
	mirrored := deps
	mst := &runState{deps: mirrored, opts: integrationActor, manifest: m, brandID: brandID}
	changes, err = planPlugin(ctx, mirrored, use(pin))
	if err != nil || len(changes) != 1 || changes[0].Fields[1].After != "mirror" {
		t.Fatalf("mirror plan = %+v %v", changes, err)
	}
	if err := changes[0].run(ctx, mst); err != nil {
		t.Fatal(err)
	}
	if len(stub.uris) != 1 || stub.uris[0] != wantURI || repoHits.Load() != hits {
		t.Fatalf("mirror install uris = %v, repository hits %d then %d", stub.uris, hits, repoHits.Load())
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
