package brand

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zsoftly/zs-core/internal/keycloak"
)

// fakeKeycloak serves the admin endpoints the identity provider and
// post-broker flow plans use, for one realm, and records every write.
type fakeKeycloak struct {
	mu     sync.Mutex
	idps   map[string]keycloak.IdentityProvider
	flows  map[string][]keycloak.Execution
	writes []string
}

func newFakeKeycloak(t *testing.T) (*fakeKeycloak, *keycloak.Client) {
	t.Helper()
	f := &fakeKeycloak{idps: map[string]keycloak.IdentityProvider{}, flows: map[string][]keycloak.Execution{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	kc, err := keycloak.New(keycloak.Config{URL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return f, kc
}

func (f *fakeKeycloak) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	if path == "/realms/master/protocol/openid-connect/token" {
		reply(map[string]any{"access_token": "t", "expires_in": 300})
		return
	}
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+path+" "+string(body))
	}
	const base = "/admin/realms/r"
	idpPrefix := base + "/identity-provider/instances"
	flowsPath := base + "/authentication/flows"
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, idpPrefix+"/"):
		idp, ok := f.idps[strings.TrimPrefix(path, idpPrefix+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		reply(idp)
	case r.Method == http.MethodPut && strings.HasPrefix(path, idpPrefix+"/"), r.Method == http.MethodPost && path == idpPrefix:
		var idp keycloak.IdentityProvider
		_ = json.Unmarshal(body, &idp)
		f.idps[idp.Alias] = idp
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == flowsPath:
		var list []keycloak.Flow
		for alias := range f.flows {
			list = append(list, keycloak.Flow{ID: alias + "-id", Alias: alias, TopLevel: true})
		}
		reply(list)
	case r.Method == http.MethodPost && path == flowsPath:
		var flow map[string]any
		_ = json.Unmarshal(body, &flow)
		f.flows[flow["alias"].(string)] = []keycloak.Execution{}
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasPrefix(path, flowsPath+"/") && strings.HasSuffix(path, "/executions"):
		alias := strings.TrimSuffix(strings.TrimPrefix(path, flowsPath+"/"), "/executions")
		execs, ok := f.flows[alias]
		if !ok {
			http.NotFound(w, r)
			return
		}
		reply(execs)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/executions/execution"):
		alias := strings.TrimSuffix(strings.TrimPrefix(path, flowsPath+"/"), "/executions/execution")
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		f.flows[alias] = append(f.flows[alias], keycloak.Execution{ID: "exec-" + in["provider"], ProviderID: in["provider"], Requirement: "DISABLED"})
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.HasPrefix(path, flowsPath+"/") && strings.HasSuffix(path, "/executions"):
		alias := strings.TrimSuffix(strings.TrimPrefix(path, flowsPath+"/"), "/executions")
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		for i, e := range f.flows[alias] {
			if e.ID == in["id"] {
				f.flows[alias][i].Requirement = in["requirement"]
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasPrefix(path, base+"/authentication/executions/") && strings.HasSuffix(path, "/config"):
		execID := strings.TrimSuffix(strings.TrimPrefix(path, base+"/authentication/executions/"), "/config")
		for alias, execs := range f.flows {
			for i, e := range execs {
				if e.ID == execID {
					f.flows[alias][i].AuthenticationConfig = "cfg-" + execID
				}
			}
		}
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasPrefix(path, base+"/authentication/config/"):
		reply(keycloak.AuthenticatorConfig{ID: strings.TrimPrefix(path, base+"/authentication/config/"),
			Config: map[string]string{"default.reference.value": "otp", "default.reference.maxAge": amrMaxAge}})
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, http.StatusTeapot)
	}
}

func runChanges(t *testing.T, changes []Change) {
	t.Helper()
	for _, ch := range changes {
		if err := ch.run(context.Background(), nil); err != nil {
			t.Fatalf("run %s %s: %v", ch.Target, ch.Name, err)
		}
	}
}

func fieldNames(changes []Change) []string {
	var out []string
	for _, ch := range changes {
		for _, f := range ch.Fields {
			out = append(out, ch.Name+" "+f.Field)
		}
	}
	return out
}

// With mfa required, every identity provider runs the post-broker MFA flow,
// so a login through a provider cannot skip the second factor. Other
// policies remove the binding.
func TestRequiredMFABindsEveryIdentityProvider(t *testing.T) {
	f, kc := newFakeKeycloak(t)
	f.idps["corp"] = keycloak.IdentityProvider{Alias: "corp", ProviderID: "oidc", Enabled: true, Config: map[string]string{}}
	m := reference(t)
	m.Identity.Realm = "r"
	m.Identity.MFA = MFARequired
	m.Identity.IdentityProviders = []IdentityProvider{
		{Alias: "corp", Type: "oidc", ClientID: "x", Issuer: "https://idp.example.test", AuthorizationURL: "https://idp.example.test/a",
			TokenURL: "https://idp.example.test/t", JWKSURL: "https://idp.example.test/k"},
		{Alias: "saml", Type: "saml", IDPEntityID: "https://saml.example.test", SSOURL: "https://saml.example.test/sso"},
	}
	ctx := context.Background()
	changes, err := planIdentityProviders(ctx, kc, m, resolved{}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fieldNames(changes), "|")
	for _, want := range []string{"corp identity.identity_providers[corp].post_broker_login_flow", "saml identity.identity_providers[saml].post_broker_login_flow"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff %s lacks %s", got, want)
		}
	}
	runChanges(t, changes)
	for _, alias := range []string{"corp", "saml"} {
		if f.idps[alias].PostBrokerLoginFlowAlias != FlowPostBrokerMFA {
			t.Fatalf("%s post-broker flow = %q", alias, f.idps[alias].PostBrokerLoginFlowAlias)
		}
	}
	for _, w := range f.writes {
		if !strings.Contains(w, `"postBrokerLoginFlowAlias":"`+FlowPostBrokerMFA+`"`) {
			t.Fatalf("write without the binding: %s", w)
		}
	}
	if again, err := planIdentityProviders(ctx, kc, m, resolved{}, true); err != nil || len(again) != 0 {
		t.Fatalf("second plan = %v, %v", fieldNames(again), err)
	}

	for _, policy := range []string{MFAOptional, MFARequiredForStaff} {
		f.idps["corp"] = keycloak.IdentityProvider{Alias: "corp", ProviderID: "oidc", Enabled: true, PostBrokerLoginFlowAlias: FlowPostBrokerMFA,
			Config: idpConfig(m.Identity.IdentityProviders[0])}
		other := clone(t, m)
		other.Identity.MFA = policy
		other.Identity.IdentityProviders = other.Identity.IdentityProviders[:1]
		changes, err := planIdentityProviders(ctx, kc, other, resolved{}, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := fieldNames(changes); len(got) != 1 || got[0] != "corp identity.identity_providers[corp].post_broker_login_flow" {
			t.Fatalf("%s: diff = %v", policy, got)
		}
		runChanges(t, changes)
		if f.idps["corp"].PostBrokerLoginFlowAlias != "" {
			t.Fatalf("%s: binding kept", policy)
		}
	}
}

func clone(t *testing.T, m *Manifest) *Manifest {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out Manifest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// Core creates the post-broker flow with one required OTP form that sets
// the otp AMR value, and repairs a flow whose form is missing or disabled.
func TestPostBrokerFlowIsCreatedAndConverged(t *testing.T) {
	f, kc := newFakeKeycloak(t)
	plan := func() []Change { return planPostBroker(t, f, kc) }
	changes := plan()
	if len(changes) != 1 || changes[0].Action != ActionCreate {
		t.Fatalf("changes = %v", fieldNames(changes))
	}
	runChanges(t, changes)
	execs := f.flows[FlowPostBrokerMFA]
	if len(execs) != 1 || execs[0].ProviderID != "auth-otp-form" || execs[0].Requirement != "REQUIRED" || execs[0].AuthenticationConfig == "" {
		t.Fatalf("flow = %+v", execs)
	}
	if again := plan(); len(again) != 0 {
		t.Fatalf("second plan = %v", fieldNames(again))
	}

	// Someone disables the form: Core requires it again.
	f.flows[FlowPostBrokerMFA][0].Requirement = "DISABLED"
	changes = plan()
	if got := fieldNames(changes); len(got) != 1 || got[0] != FlowPostBrokerMFA+" executions.auth-otp-form.requirement" {
		t.Fatalf("disabled form diff = %v", got)
	}
	runChanges(t, changes)

	// Someone removes the form: Core adds it back.
	f.flows[FlowPostBrokerMFA] = []keycloak.Execution{}
	changes = plan()
	if got := strings.Join(fieldNames(changes), "|"); !strings.Contains(got, "executions.auth-otp-form|") || !strings.Contains(got, "requirement") {
		t.Fatalf("missing form diff = %v", got)
	}
	runChanges(t, changes)
	if execs := f.flows[FlowPostBrokerMFA]; len(execs) != 1 || execs[0].Requirement != "REQUIRED" {
		t.Fatalf("flow after repair = %+v", execs)
	}
}

func planPostBroker(t *testing.T, f *fakeKeycloak, kc *keycloak.Client) []Change {
	t.Helper()
	f.mu.Lock()
	_, exists := f.flows[FlowPostBrokerMFA]
	f.mu.Unlock()
	changes, err := planPostBrokerFlow(context.Background(), kc, "r", exists)
	if err != nil {
		t.Fatal(err)
	}
	return changes
}
