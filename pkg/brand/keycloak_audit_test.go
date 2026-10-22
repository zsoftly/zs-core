package brand

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zsoftly/zs-core/internal/keycloak"
)

// auditPlannerFake supplies the realm, required-action, client and scope
// endpoints that planKeycloak reads. The established fakeKeycloak continues
// to supply the flow and identity-provider endpoints.
type auditPlannerFake struct {
	mu       sync.Mutex
	realm    keycloak.Object
	actions  map[string]keycloak.RequiredAction
	clients  map[string]keycloak.Object
	secrets  map[string]string
	mappers  map[string][]keycloak.ProtocolMapper
	scopes   []keycloak.ClientScope
	defaults map[string][]keycloak.ClientScope
	writes   int
	flows    *fakeKeycloak
}

func newAuditPlannerFake(t *testing.T, m *Manifest, sec resolved, listeners []string) (*auditPlannerFake, *keycloak.Client) {
	t.Helper()
	flows, _ := newFakeKeycloak(t)
	for _, spec := range browserFlows {
		flows.seedBrowserCopy(spec.alias)
	}

	f := &auditPlannerFake{
		realm:    keycloak.Object{"realm": m.Identity.Realm, "browserFlow": FlowOptional},
		actions:  map[string]keycloak.RequiredAction{},
		clients:  map[string]keycloak.Object{},
		secrets:  map[string]string{},
		mappers:  map[string][]keycloak.ProtocolMapper{},
		defaults: map[string][]keycloak.ClientScope{},
		flows:    flows,
	}
	for _, managed := range realmFields(m) {
		if managed.key != "eventsListeners" && !strings.HasPrefix(managed.key, "events") && managed.key != "adminEventsEnabled" && managed.key != "adminEventsDetailsEnabled" {
			f.realm[managed.key] = managed.value
		}
	}
	// Deliberately make every audit setting wrong. Assertions below use
	// literals rather than the planner's field list.
	f.realm["eventsListeners"] = stringsToAny(listeners)
	f.realm["eventsEnabled"] = false
	f.realm["adminEventsEnabled"] = false
	f.realm["adminEventsDetailsEnabled"] = true
	f.realm["eventsExpiration"] = float64(1)

	for _, alias := range requiredActionAliases {
		f.actions[alias] = keycloak.RequiredAction{Alias: alias, Enabled: true}
	}
	for _, spec := range clientSpecs(m, sec) {
		uuid := spec.clientID + "-id"
		client := keycloak.Object{"id": uuid, "clientId": spec.clientID}
		for _, managed := range spec.fields {
			client[managed.key] = managed.value
		}
		if spec.path == "identity.customer_client" || spec.path == "identity.staff_client" {
			client["attributes"] = map[string]string{pkceAttribute: "S256"}
		}
		f.clients[spec.clientID] = client
		f.secrets[uuid] = spec.secret
		f.mappers[uuid] = append([]keycloak.ProtocolMapper(nil), spec.mappers...)
	}
	staffUUID := m.Identity.StaffClient.ClientID + "-id"
	staffScope := keycloak.ClientScope{ID: "zs-staff-client-id", Name: StaffClientScope, Protocol: "openid-connect"}
	f.scopes = []keycloak.ClientScope{staffScope}
	f.defaults[staffUUID] = []keycloak.ClientScope{staffScope}

	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	kc, err := keycloak.New(keycloak.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return f, kc
}

func (f *auditPlannerFake) serve(w http.ResponseWriter, r *http.Request) {
	const base = "/admin/realms/r"
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	decode := func(v any) bool {
		body, err := io.ReadAll(r.Body)
		if err != nil || json.Unmarshal(body, v) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return false
		}
		return true
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		reply(f.realm)
		return
	case r.Method == http.MethodPut && r.URL.Path == base:
		update := keycloak.Object{}
		if !decode(&update) {
			return
		}
		for key, value := range update {
			f.realm[key] = value
		}
		f.writes++
		w.WriteHeader(http.StatusNoContent)
		return
	case strings.HasPrefix(r.URL.Path, base+"/authentication/required-actions/"):
		alias := strings.TrimPrefix(r.URL.Path, base+"/authentication/required-actions/")
		switch r.Method {
		case http.MethodGet:
			action, ok := f.actions[alias]
			if !ok {
				http.NotFound(w, r)
				return
			}
			reply(action)
		case http.MethodPut:
			var action keycloak.RequiredAction
			if !decode(&action) {
				return
			}
			f.actions[alias] = action
			f.writes++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected required action request", http.StatusTeapot)
		}
		return
	case r.Method == http.MethodGet && r.URL.Path == base+"/clients":
		if client, ok := f.clients[r.URL.Query().Get("clientId")]; ok {
			reply([]keycloak.Object{client})
		} else {
			reply([]keycloak.Object{})
		}
		return
	case r.Method == http.MethodGet && r.URL.Path == base+"/client-scopes":
		reply(f.scopes)
		return
	case strings.HasPrefix(r.URL.Path, base+"/clients/"):
		f.serveClient(w, r, reply, decode)
		return
	default:
		f.mu.Unlock()
		f.flows.serve(w, r)
		f.mu.Lock()
	}
}

func (f *auditPlannerFake) serveClient(w http.ResponseWriter, r *http.Request, reply func(any), decode func(any) bool) {
	const prefix = "/admin/realms/r/clients/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	uuid := parts[0]
	switch {
	case len(parts) == 2 && parts[1] == "client-secret" && r.Method == http.MethodGet:
		reply(map[string]string{"value": f.secrets[uuid]})
	case len(parts) == 3 && parts[1] == "protocol-mappers" && parts[2] == "models" && r.Method == http.MethodGet:
		reply(f.mappers[uuid])
	case len(parts) == 2 && parts[1] == "default-client-scopes" && r.Method == http.MethodGet:
		reply(f.defaults[uuid])
	case len(parts) == 1 && r.Method == http.MethodPut:
		update := keycloak.Object{}
		if !decode(&update) {
			return
		}
		for _, client := range f.clients {
			if client["id"] != uuid {
				continue
			}
			for key, value := range update {
				client[key] = value
			}
			f.writes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	default:
		http.Error(w, "unexpected client request", http.StatusTeapot)
	}
}

func (f *auditPlannerFake) snapshot() (keycloak.Object, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := keycloak.Object{}
	for key, value := range f.realm {
		copy[key] = value
	}
	return copy, f.writes
}

func (f *auditPlannerFake) writeCount() int {
	_, writes := f.snapshot()
	f.flows.mu.Lock()
	defer f.flows.mu.Unlock()
	return writes + len(f.flows.writes)
}

func stringsToAny(in []string) []any {
	out := make([]any, len(in))
	for i, value := range in {
		out[i] = value
	}
	return out
}

func TestKeycloakAuditRealmConfigurationConverges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		listeners []string
		want      []string
	}{
		{name: "appends audit listener", listeners: []string{"existing-a", "existing-b"}, want: []string{"existing-a", "existing-b", "zs-audit"}},
		{name: "preserves existing audit listener once", listeners: []string{"existing-a", "zs-audit", "existing-b"}, want: []string{"existing-a", "zs-audit", "existing-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			m := reference(t)
			m.Identity.Realm = "r"
			sec := resolved{m.Identity.StaffClient.SecretRef: "staff-secret"}
			fake, kc := newAuditPlannerFake(t, m, sec, tc.listeners)

			changes, err := planKeycloak(ctx, kc, m, sec)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) == 0 {
				t.Fatal("first plan is empty")
			}
			runAuditPlannerChanges(t, ctx, changes)

			realm, realmWrites := fake.snapshot()
			if realmWrites == 0 {
				t.Fatal("audit realm settings were not written")
			}
			gotListeners, ok := realm["eventsListeners"].([]any)
			if !ok || !slices.Equal(anyToStrings(gotListeners), tc.want) {
				t.Fatalf("eventsListeners = %#v, want %v", realm["eventsListeners"], tc.want)
			}
			if got, ok := realm["eventsEnabled"].(bool); !ok || !got {
				t.Fatalf("eventsEnabled = %#v, want true", realm["eventsEnabled"])
			}
			if got, ok := realm["adminEventsEnabled"].(bool); !ok || !got {
				t.Fatalf("adminEventsEnabled = %#v, want true", realm["adminEventsEnabled"])
			}
			if got, ok := realm["adminEventsDetailsEnabled"].(bool); !ok || got {
				t.Fatalf("adminEventsDetailsEnabled = %#v, want false", realm["adminEventsDetailsEnabled"])
			}
			if got, ok := realm["eventsExpiration"].(float64); !ok || got != 604800 {
				t.Fatalf("eventsExpiration = %#v, want 604800", realm["eventsExpiration"])
			}

			writes := fake.writeCount()
			again, err := planKeycloak(ctx, kc, m, sec)
			if err != nil {
				t.Fatal(err)
			}
			if len(again) != 0 {
				t.Fatalf("second plan = %v", fieldNames(again))
			}
			runAuditPlannerChanges(t, ctx, again)
			if got := fake.writeCount(); got != writes {
				t.Fatalf("second plan wrote %d times, want %d", got, writes)
			}
		})
	}
}

func runAuditPlannerChanges(t *testing.T, ctx context.Context, changes []Change) {
	t.Helper()
	for _, change := range changes {
		if err := change.run(ctx, nil); err != nil {
			t.Fatalf("run %s %s: %v", change.Target, change.Name, err)
		}
	}
}

func anyToStrings(in []any) []string {
	out := make([]string, 0, len(in))
	for _, value := range in {
		stringValue, ok := value.(string)
		if !ok {
			return nil
		}
		out = append(out, stringValue)
	}
	return out
}
