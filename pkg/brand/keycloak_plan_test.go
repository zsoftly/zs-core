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

	"github.com/zsoftly/zs-core/internal/keycloak"
)

// fakeKeycloak serves the admin endpoints the identity provider and
// post-broker flow plans use, for one realm, and records every write.
type fakeKeycloak struct {
	mu    sync.Mutex
	idps  map[string]keycloak.IdentityProvider
	flows map[string][]keycloak.Execution
	// parents maps a sub-flow alias to the flow that holds it.
	parents map[string]string
	configs map[string]keycloak.AuthenticatorConfig
	writes  []string
}

func newFakeKeycloak(t *testing.T) (*fakeKeycloak, *keycloak.Client) {
	t.Helper()
	f := &fakeKeycloak{idps: map[string]keycloak.IdentityProvider{}, flows: map[string][]keycloak.Execution{},
		parents: map[string]string{}, configs: map[string]keycloak.AuthenticatorConfig{}}
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
			if _, sub := f.parents[alias]; !sub {
				list = append(list, keycloak.Flow{ID: alias + "-id", Alias: alias, TopLevel: true})
			}
		}
		reply(list)
	case r.Method == http.MethodPost && path == flowsPath:
		var flow map[string]any
		_ = json.Unmarshal(body, &flow)
		f.flows[flow["alias"].(string)] = []keycloak.Execution{}
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasPrefix(path, flowsPath+"/") && strings.HasSuffix(path, "/executions"):
		alias := strings.TrimSuffix(strings.TrimPrefix(path, flowsPath+"/"), "/executions")
		if _, ok := f.flows[alias]; !ok {
			http.NotFound(w, r)
			return
		}
		reply(f.expand(alias, 0))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/executions/flow"):
		alias := strings.TrimSuffix(strings.TrimPrefix(path, flowsPath+"/"), "/executions/flow")
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		f.flows[alias] = append(f.flows[alias], keycloak.Execution{ID: "exec-sub-" + in["alias"], AuthenticationFlow: true,
			DisplayName: in["alias"], Requirement: "DISABLED"})
		f.flows[in["alias"]] = []keycloak.Execution{}
		f.parents[in["alias"]] = alias
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/executions/execution"):
		alias := strings.TrimSuffix(strings.TrimPrefix(path, flowsPath+"/"), "/executions/execution")
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		f.flows[alias] = append(f.flows[alias], keycloak.Execution{ID: "exec-" + in["provider"], ProviderID: in["provider"], Requirement: "DISABLED"})
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPut && strings.HasPrefix(path, flowsPath+"/") && strings.HasSuffix(path, "/executions"):
		var in struct {
			ID          string `json:"id"`
			Requirement string `json:"requirement"`
			Priority    *int   `json:"priority"`
		}
		_ = json.Unmarshal(body, &in)
		for flow := range f.flows {
			for i, e := range f.flows[flow] {
				if e.ID == in.ID {
					f.flows[flow][i].Requirement = in.Requirement
					// Keycloak 26.8.1 takes the priority from the request
					// and uses 0 when it is missing.
					f.flows[flow][i].Priority = 0
					if in.Priority != nil {
						f.flows[flow][i].Priority = *in.Priority
					}
					slices.SortStableFunc(f.flows[flow], func(a, b keycloak.Execution) int { return a.Priority - b.Priority })
				}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasPrefix(path, base+"/authentication/executions/") && strings.HasSuffix(path, "/config"):
		execID := strings.TrimSuffix(strings.TrimPrefix(path, base+"/authentication/executions/"), "/config")
		var cfg keycloak.AuthenticatorConfig
		_ = json.Unmarshal(body, &cfg)
		cfg.ID = "cfg-" + execID
		f.configs[cfg.ID] = cfg
		for alias, execs := range f.flows {
			for i, e := range execs {
				if e.ID == execID {
					f.flows[alias][i].AuthenticationConfig = "cfg-" + execID
				}
			}
		}
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasPrefix(path, base+"/authentication/config/"):
		id := strings.TrimPrefix(path, base+"/authentication/config/")
		if cfg, ok := f.configs[id]; ok {
			reply(cfg)
			return
		}
		reply(keycloak.AuthenticatorConfig{ID: id,
			Config: map[string]string{"default.reference.value": "otp", "default.reference.maxAge": amrMaxAge}})
	case r.Method == http.MethodPut && strings.HasPrefix(path, base+"/authentication/config/"):
		var cfg keycloak.AuthenticatorConfig
		_ = json.Unmarshal(body, &cfg)
		f.configs[cfg.ID] = cfg
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, http.StatusTeapot)
	}
}

// expand lists a flow's executions with its sub-flows' executions inline,
// one level deeper, as Keycloak does.
func (f *fakeKeycloak) expand(alias string, level int) []keycloak.Execution {
	out := []keycloak.Execution{}
	for _, e := range f.flows[alias] {
		e.Level = level
		out = append(out, e)
		if e.AuthenticationFlow {
			out = append(out, f.expand(e.DisplayName, level+1)...)
		}
	}
	return out
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

	for policy, want := range map[string]string{MFAOptional: "", MFARequiredForStaff: FlowPostBrokerStaffMFA} {
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
		if f.idps["corp"].PostBrokerLoginFlowAlias != want {
			t.Fatalf("%s: binding = %q, want %q", policy, f.idps["corp"].PostBrokerLoginFlowAlias, want)
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

// The staff post-broker flow holds one CONDITIONAL sub-flow: the client
// scope condition on StaffClientScope and a required OTP form. Core creates
// it, finds nothing to do the second time, and repairs a tampered condition.
func TestStaffPostBrokerFlowIsCreatedAndConverged(t *testing.T) {
	f, kc := newFakeKeycloak(t)
	plan := func() []Change {
		t.Helper()
		f.mu.Lock()
		_, exists := f.flows[FlowPostBrokerStaffMFA]
		f.mu.Unlock()
		changes, err := planStaffPostBrokerFlow(context.Background(), kc, "r", exists)
		if err != nil {
			t.Fatal(err)
		}
		return changes
	}
	changes := plan()
	if len(changes) != 1 || changes[0].Action != ActionCreate || len(changes[0].Fields) != 10 {
		t.Fatalf("changes = %v", fieldNames(changes))
	}
	runChanges(t, changes)
	execs := f.expand(FlowPostBrokerStaffMFA, 0)
	if len(execs) != 4 || !execs[0].AuthenticationFlow || execs[0].Requirement != "CONDITIONAL" ||
		execs[1].ProviderID != clientScopeCondition || execs[1].Requirement != "REQUIRED" || execs[1].Level != 1 ||
		execs[2].ProviderID != otpFormProvider || execs[2].Requirement != "REQUIRED" || execs[2].AuthenticationConfig == "" ||
		execs[3].ProviderID != allowAccess || execs[3].Requirement != "REQUIRED" || execs[3].Level != 0 {
		t.Fatalf("flow = %+v", execs)
	}
	cond := f.configs[execs[1].AuthenticationConfig]
	if cond.Config["client_scope"] != StaffClientScope || cond.Config["negate"] != "false" {
		t.Fatalf("condition config = %+v", cond)
	}
	if again := plan(); len(again) != 0 {
		t.Fatalf("second plan = %v", fieldNames(again))
	}

	// Someone negates the condition: Core sets it back.
	cond.Config["negate"] = "true"
	f.configs[cond.ID] = cond
	changes = plan()
	if got := fieldNames(changes); len(got) != 1 || got[0] != FlowPostBrokerStaffMFA+" executions."+clientScopeCondition+".config" {
		t.Fatalf("negated condition diff = %v", got)
	}
	runChanges(t, changes)
	if f.configs[cond.ID].Config["negate"] != "false" {
		t.Fatalf("condition after repair = %+v", f.configs[cond.ID])
	}
	if again := plan(); len(again) != 0 {
		t.Fatalf("plan after repair = %v", fieldNames(again))
	}
}

// Keycloak refuses a provider update that changes the token URL without the
// secret, so Core sends the resolved secret with every update of a provider
// that has one, and the diff shows the secret only when it changed.
func TestProviderUpdateCarriesTheSecret(t *testing.T) {
	f, kc := newFakeKeycloak(t)
	m := reference(t)
	m.Identity.Realm = "r"
	ref := "env://BRAND_ZS_REFERENCE__IDP"
	p := IdentityProvider{Alias: "corp", Type: "oidc", ClientID: "x", ClientSecretRef: ref, Issuer: "https://idp.example.test",
		AuthorizationURL: "https://idp.example.test/a", TokenURL: "https://idp.example.test/t", JWKSURL: "https://idp.example.test/k"}
	m.Identity.IdentityProviders = []IdentityProvider{p}
	cfg := idpConfig(p)
	cfg["tokenUrl"] = "https://drifted.example.test/t"
	cfg["clientSecret"] = keycloak.SecretMask
	cfg[idpSecretDigestKey] = secretDigest("s3cret")
	f.idps["corp"] = keycloak.IdentityProvider{Alias: "corp", ProviderID: "oidc", Enabled: true, Config: cfg}
	changes, err := planIdentityProviders(context.Background(), kc, m, resolved{ref: "s3cret"}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fieldNames(changes), "|")
	if got != "corp identity.identity_providers[corp].config.tokenUrl" {
		t.Fatalf("diff = %s", got)
	}
	runChanges(t, changes)
	if f.idps["corp"].Config["clientSecret"] != "s3cret" || f.idps["corp"].Config["tokenUrl"] != p.TokenURL {
		t.Fatalf("update = %+v", f.idps["corp"].Config)
	}
}

// seedBrowserCopy stores a copy of the built-in browser flow under alias, as
// Keycloak 26.8.1 copies it: Cookie and the identity provider redirector as
// alternatives, then the login forms with a conditional second factor.
func (f *fakeKeycloak) seedBrowserCopy(alias string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	forms, twoFactor := alias+" forms", alias+" Browser - Conditional 2FA"
	f.flows[alias] = []keycloak.Execution{
		{ID: alias + "-cookie", ProviderID: "auth-cookie", Requirement: "ALTERNATIVE", Priority: 10},
		{ID: alias + "-redirector", ProviderID: "identity-provider-redirector", Requirement: "ALTERNATIVE", Priority: 25},
		{ID: alias + "-forms", AuthenticationFlow: true, DisplayName: forms, Requirement: "ALTERNATIVE", Priority: 30},
	}
	f.flows[forms] = []keycloak.Execution{
		{ID: alias + "-password", ProviderID: "auth-username-password-form", Requirement: "REQUIRED", Priority: 10},
		{ID: alias + "-2fa", AuthenticationFlow: true, DisplayName: twoFactor, Requirement: "CONDITIONAL", Priority: 20},
	}
	f.flows[twoFactor] = []keycloak.Execution{
		{ID: alias + "-condition", ProviderID: "conditional-user-configured", Requirement: "REQUIRED", Priority: 10},
		{ID: alias + "-otp", ProviderID: "auth-otp-form", Requirement: "ALTERNATIVE", Priority: 30},
	}
	f.parents[forms], f.parents[twoFactor] = alias, forms
}

func requirementOf(execs []keycloak.Execution, provider string) string {
	for _, e := range execs {
		if e.ProviderID == provider && e.Level == 0 {
			return e.Requirement
		}
	}
	for _, e := range execs {
		if e.ProviderID == provider {
			return e.Requirement
		}
	}
	return ""
}

// The staff client's flow cannot be satisfied by a Keycloak SSO session
// alone: its Cookie step is disabled and its second factor required. The
// customer flows keep Cookie, so single sign-on still works for them.
func TestStaffBrowserFlowDisablesCookie(t *testing.T) {
	ctx := context.Background()
	f, kc := newFakeKeycloak(t)
	for _, spec := range browserFlows {
		f.seedBrowserCopy(spec.alias)
		execs, err := kc.Executions(ctx, "r", spec.alias)
		if err != nil {
			t.Fatal(err)
		}
		ops, err := flowOps(ctx, kc, "r", spec, execs)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range ops {
			if err := op.run(ctx); err != nil {
				t.Fatalf("%s: %s: %v", spec.alias, op.field.Field, err)
			}
		}
		execs, err = kc.Executions(ctx, "r", spec.alias)
		if err != nil {
			t.Fatal(err)
		}
		wantCookie := "ALTERNATIVE"
		if spec.alias == FlowStaffMFA {
			wantCookie = "DISABLED"
		}
		if got := requirementOf(execs, "auth-cookie"); got != wantCookie {
			t.Errorf("%s: cookie %s, want %s", spec.alias, got, wantCookie)
		}
		wantOTP := "ALTERNATIVE"
		if spec.mfa {
			wantOTP = "REQUIRED"
		}
		if got := requirementOf(execs, "auth-otp-form"); got != wantOTP {
			t.Errorf("%s: otp %s, want %s", spec.alias, got, wantOTP)
		}
		again, err := flowOps(ctx, kc, "r", spec, execs)
		if err != nil || len(again) != 0 {
			t.Errorf("%s does not converge: %d ops, %v", spec.alias, len(again), err)
		}
		// The password form still runs before the second factor.
		if !passwordBeforeTwoFactor(execs) {
			t.Errorf("%s: the second-factor sub-flow runs before the password form: %+v", spec.alias, execs)
		}
	}
	if !(browserFlows[2].alias == FlowStaffMFA && browserFlows[2].noCookie && browserFlows[2].mfa) {
		t.Fatal("FlowStaffMFA must require the second factor and disable Cookie")
	}
}

func passwordBeforeTwoFactor(execs []keycloak.Execution) bool {
	password, twoFactor := -1, -1
	for i, e := range execs {
		switch {
		case e.ProviderID == "auth-username-password-form":
			password = i
		case e.AuthenticationFlow && strings.HasSuffix(e.DisplayName, "Conditional 2FA"):
			twoFactor = i
		}
	}
	return password >= 0 && twoFactor > password
}

// An earlier apply that set requirements without priorities left the
// second-factor sub-flow in front of the password form, where Keycloak fails
// every password login. Apply moves it back behind the form.
func TestBrowserFlowOrderIsRepaired(t *testing.T) {
	ctx := context.Background()
	f, kc := newFakeKeycloak(t)
	f.seedBrowserCopy(FlowMFA)
	f.mu.Lock()
	forms := FlowMFA + " forms"
	f.flows[forms][1].Priority, f.flows[forms][1].Requirement = 0, "REQUIRED"
	slices.SortStableFunc(f.flows[forms], func(a, b keycloak.Execution) int { return a.Priority - b.Priority })
	f.mu.Unlock()
	spec := browserFlows[1]
	execs, err := kc.Executions(ctx, "r", FlowMFA)
	if err != nil {
		t.Fatal(err)
	}
	if passwordBeforeTwoFactor(execs) {
		t.Fatal("the seed is not broken")
	}
	ops, err := flowOps(ctx, kc, "r", spec, execs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(opFields(ops), "executions.conditional-2fa-subflow.priority") {
		t.Fatalf("no priority repair in %v", opFields(ops))
	}
	for _, op := range ops {
		if err := op.run(ctx); err != nil {
			t.Fatal(err)
		}
	}
	execs, _ = kc.Executions(ctx, "r", FlowMFA)
	if !passwordBeforeTwoFactor(execs) {
		t.Fatalf("order not repaired: %+v", execs)
	}
	if again, _ := flowOps(ctx, kc, "r", spec, execs); len(again) != 0 {
		t.Fatalf("not converged: %v", opFields(again))
	}
}

func opFields(ops []flowOp) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.field.Field
	}
	return out
}
