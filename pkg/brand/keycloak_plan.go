package brand

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/zsoftly/zs-core/internal/keycloak"
)

// Flow aliases Core manages in every brand realm. FlowOptional, FlowMFA and
// FlowStaffMFA are copies of the built-in browser flow. FlowMFA makes the
// conditional second-factor step required; FlowOptional keeps it
// conditional. FlowStaffMFA is FlowMFA with the Cookie step disabled: it is
// the browser flow of the staff client whenever mfa is required or
// required_for_staff. A Keycloak SSO session from the customer client may
// hold no second factor, and the Cookie step alone would satisfy the flow
// with it. Without it, the staff client always asks for the password and the
// second factor. A required OTP form at the top level next to Cookie cannot
// do this: Keycloak ignores the alternatives of a level that holds a
// required execution, so Cookie, the identity provider redirector and the
// login forms would all stop running.
// FlowPostBrokerMFA holds a required OTP form only. With mfa required it is
// the post-broker-login flow of every identity provider, so a login through
// a provider also needs the second factor. A copy of the browser flow cannot
// serve there: its username and password form would ask a brokered user for
// a local password.
const (
	FlowOptional      = "zs-browser"
	FlowMFA           = "zs-browser-mfa"
	FlowStaffMFA      = "zs-browser-staff-mfa"
	FlowPostBrokerMFA = "zs-post-broker-mfa"
	builtinFlow       = "browser"
	cookieProvider    = "auth-cookie"
	// amrMaxAge is how long, in seconds, an authenticator's AMR value stays
	// in the tokens of a session. It matches Keycloak's default SSO session
	// maximum; recency is judged from auth_time, not from amr.
	amrMaxAge = "36000"
	// idpSecretDigestKey stores the SHA-256 of the client secret Core last
	// set on an identity provider. Keycloak masks the secret itself, so
	// without it Core could not tell whether the secret changed.
	idpSecretDigestKey = "zs.core.clientSecretSha256"
)

// requiredActionAliases maps manifest names to Keycloak aliases.
var requiredActionAliases = map[string]string{
	"verify_email":         "VERIFY_EMAIL",
	"update_profile":       "UPDATE_PROFILE",
	"configure_totp":       "CONFIGURE_TOTP",
	"update_password":      "UPDATE_PASSWORD",
	"terms_and_conditions": "TERMS_AND_CONDITIONS",
	"webauthn_register":    "webauthn-register",
}

// field is one managed Keycloak field: the manifest path shown in a diff,
// the Keycloak key and the desired value.
type field struct {
	path  string
	key   string
	value any
}

func realmFields(m *Manifest) []field {
	id := m.Identity
	fs := []field{
		{"identity.realm_enabled", "enabled", true},
		{"identity.display_name", "displayName", id.DisplayName},
		{"identity.locales", "supportedLocales", sortedCopy(id.Locales)},
		{"identity.internationalization_enabled", "internationalizationEnabled", true},
		{"identity.default_locale", "defaultLocale", id.DefaultLocale},
		{"identity.required_actions.verify_email", "verifyEmail", containsString(id.RequiredActions, "verify_email")},
		// Tenants are backed by Keycloak Organizations, which answer 404
		// until the realm enables them.
		{"identity.organizations_enabled", "organizationsEnabled", true},
	}
	if id.LoginTheme != "" {
		fs = append(fs, field{"identity.login_theme", "loginTheme", id.LoginTheme})
	}
	if id.EmailTheme != "" {
		fs = append(fs, field{"identity.email_theme", "emailTheme", id.EmailTheme})
	}
	if id.SSLRequired != "" {
		fs = append(fs, field{"identity.ssl_required", "sslRequired", id.SSLRequired})
	}
	if tl := id.TokenLifetimes; tl != nil {
		if tl.AccessTokenSeconds != nil {
			fs = append(fs, field{"identity.token_lifetimes.access_token_seconds", "accessTokenLifespan", int64(*tl.AccessTokenSeconds)})
		}
		if tl.SSOSessionIdleSeconds != nil {
			fs = append(fs, field{"identity.token_lifetimes.sso_session_idle_seconds", "ssoSessionIdleTimeout", int64(*tl.SSOSessionIdleSeconds)})
		}
		if tl.SSOSessionMaxSeconds != nil {
			fs = append(fs, field{"identity.token_lifetimes.sso_session_max_seconds", "ssoSessionMaxLifespan", int64(*tl.SSOSessionMaxSeconds)})
		}
	}
	return fs
}

// normalize makes values read from Keycloak JSON comparable with desired
// values: integral numbers become int64 and string lists are sorted.
func normalize(v any) any {
	switch t := v.(type) {
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	case int:
		return int64(t)
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return v
			}
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	case []string:
		return sortedCopy(t)
	case nil:
		return nil
	}
	return v
}

func equalValue(current, desired any) bool {
	c, d := normalize(current), normalize(desired)
	// An unset string and an empty one are the same to Keycloak.
	if c == nil {
		if s, ok := d.(string); ok && s == "" {
			return true
		}
	}
	return reflect.DeepEqual(c, d)
}

// compareFields returns the differing fields and the update to send.
func compareFields(current keycloak.Object, fs []field) ([]FieldChange, keycloak.Object) {
	var changes []FieldChange
	update := keycloak.Object{}
	for _, f := range fs {
		var cur any
		if current != nil {
			cur = current[f.key]
		}
		if equalValue(cur, f.value) {
			continue
		}
		changes = append(changes, FieldChange{Field: f.path, Before: normalize(cur), After: normalize(f.value)})
		update[f.key] = f.value
	}
	return changes, update
}

func allFields(fs []field) ([]FieldChange, keycloak.Object) {
	return compareFields(nil, fs)
}

func planKeycloak(ctx context.Context, kc *keycloak.Client, m *Manifest, sec resolved) ([]Change, error) {
	realm := m.Identity.Realm
	current, err := kc.GetRealm(ctx, realm)
	exists := true
	if errors.Is(err, keycloak.ErrNotFound) {
		exists, current = false, nil
	} else if err != nil {
		return nil, err
	}

	var changes []Change
	rf := realmFields(m)
	if !exists {
		fields, update := allFields(rf)
		changes = append(changes, Change{
			Target: TargetRealm, Name: realm, Action: ActionCreate, Fields: fields,
			run: func(ctx context.Context, st *runState) error {
				if err := kc.CreateRealm(ctx, keycloak.Object{"realm": realm, "enabled": true, "displayName": m.Identity.DisplayName}); err != nil {
					return err
				}
				return kc.UpdateRealm(ctx, realm, update)
			},
		})
	} else if fields, update := compareFields(current, rf); len(fields) > 0 {
		changes = append(changes, Change{
			Target: TargetRealm, Name: realm, Action: ActionUpdate, Fields: fields,
			run: func(ctx context.Context, st *runState) error { return kc.UpdateRealm(ctx, realm, update) },
		})
	}

	flows, err := planFlows(ctx, kc, realm, exists)
	if err != nil {
		return nil, err
	}
	changes = append(changes, flows...)

	ras, err := planRequiredActions(ctx, kc, m, exists)
	if err != nil {
		return nil, err
	}
	changes = append(changes, ras...)

	clients, staffClient, err := planClients(ctx, kc, m, sec, exists)
	if err != nil {
		return nil, err
	}
	changes = append(changes, clients...)

	staffScope, err := planStaffClientScope(ctx, kc, m, staffClient, exists)
	if err != nil {
		return nil, err
	}
	changes = append(changes, staffScope...)

	idps, err := planIdentityProviders(ctx, kc, m, sec, exists)
	if err != nil {
		return nil, err
	}
	changes = append(changes, idps...)

	mfa, err := planMFA(ctx, kc, m, current, staffClient, exists)
	if err != nil {
		return nil, err
	}
	return append(changes, mfa...), nil
}

// flowLayout locates the executions Core manages in a copy of the browser
// flow.
type flowLayout struct {
	password   keycloak.Execution
	otp        keycloak.Execution
	twoFactor  keycloak.Execution
	conditions []keycloak.Execution
	cookie     keycloak.Execution
	// twoFactorFirst is true when the second-factor sub-flow runs before
	// the password form at the same level.
	twoFactorFirst bool
}

func layoutOf(alias string, execs []keycloak.Execution) (flowLayout, error) {
	var l flowLayout
	otpAt, passwordAt := -1, -1
	for i, e := range execs {
		switch e.ProviderID {
		case "auth-username-password-form":
			l.password, passwordAt = e, i
		case "auth-otp-form":
			l.otp, otpAt = e, i
		case cookieProvider:
			if e.Level == 0 && l.cookie.ID == "" {
				l.cookie = e
			}
		}
	}
	if l.password.ID == "" || otpAt < 0 {
		return l, fmt.Errorf("flow %s has no username password form or no OTP form; it is not a copy of the browser flow", alias)
	}
	start := -1
	for i := otpAt - 1; i >= 0; i-- {
		if execs[i].AuthenticationFlow && execs[i].Level == l.otp.Level-1 {
			l.twoFactor, start = execs[i], i
			break
		}
	}
	if start < 0 {
		return l, fmt.Errorf("flow %s has no sub-flow around the OTP form", alias)
	}
	l.twoFactorFirst = l.twoFactor.Level == l.password.Level && start < passwordAt
	for i := start + 1; i < len(execs) && execs[i].Level >= l.otp.Level; i++ {
		e := execs[i]
		if e.Level == l.otp.Level && len(e.ProviderID) > len("conditional-") && e.ProviderID[:len("conditional-")] == "conditional-" {
			l.conditions = append(l.conditions, e)
		}
	}
	return l, nil
}

type flowOp struct {
	field FieldChange
	run   func(ctx context.Context) error
}

// browserFlowSpec is one copy of the browser flow Core manages: whether its
// second factor is required, and whether its Cookie step is disabled.
type browserFlowSpec struct {
	alias    string
	mfa      bool
	noCookie bool
}

var browserFlows = []browserFlowSpec{
	{alias: FlowOptional},
	{alias: FlowMFA, mfa: true},
	{alias: FlowStaffMFA, mfa: true, noCookie: true},
}

// flowOps returns what a copy of the browser flow needs to match Core's
// layout. When the flow does not exist yet, every op is listed.
func flowOps(ctx context.Context, kc *keycloak.Client, realm string, spec browserFlowSpec, execs []keycloak.Execution) ([]flowOp, error) {
	alias, mfa := spec.alias, spec.mfa
	l, err := layoutOf(alias, execs)
	if err != nil {
		return nil, err
	}
	var ops []flowOp
	require := func(name string, e keycloak.Execution, want string) {
		if e.Requirement == want {
			return
		}
		id := e.ID
		ops = append(ops, flowOp{
			field: FieldChange{Field: "executions." + name + ".requirement", Before: e.Requirement, After: want},
			run: func(ctx context.Context) error {
				return kc.UpdateExecutionRequirement(ctx, realm, alias, id, want)
			},
		})
	}
	if l.twoFactorFirst {
		// An earlier apply set the requirement without the priority, and
		// Keycloak moved the sub-flow in front of the password form, where
		// it fails for want of a user. The built-in flow spaces them by 10.
		id, priority := l.twoFactor.ID, l.password.Priority+10
		ops = append(ops, flowOp{
			field: FieldChange{Field: "executions.conditional-2fa-subflow.priority", Before: l.twoFactor.Priority, After: priority},
			run: func(ctx context.Context) error {
				return kc.SetExecutionPriority(ctx, realm, alias, id, priority)
			},
		})
	}
	if spec.noCookie && l.cookie.ID != "" {
		require(cookieProvider, l.cookie, "DISABLED")
	}
	if mfa {
		require("conditional-2fa-subflow", l.twoFactor, "REQUIRED")
		for _, c := range l.conditions {
			require(c.ProviderID, c, "DISABLED")
		}
		require("auth-otp-form", l.otp, "REQUIRED")
	}
	for _, ref := range []struct {
		name  string
		exec  keycloak.Execution
		value string
	}{{"auth-username-password-form", l.password, "pwd"}, {"auth-otp-form", l.otp, "otp"}} {
		op, err := amrOp(ctx, kc, realm, alias, ref.name, ref.exec, ref.value)
		if err != nil {
			return nil, err
		}
		if op != nil {
			ops = append(ops, *op)
		}
	}
	return ops, nil
}

// amrOp returns the op that sets the AMR reference value of an execution, or
// nil when it is already set.
func amrOp(ctx context.Context, kc *keycloak.Client, realm, alias, name string, exec keycloak.Execution, value string) (*flowOp, error) {
	want := map[string]string{"default.reference.value": value, "default.reference.maxAge": amrMaxAge}
	var current keycloak.AuthenticatorConfig
	if exec.AuthenticationConfig != "" {
		var err error
		current, err = kc.GetAuthenticatorConfig(ctx, realm, exec.AuthenticationConfig)
		if err != nil {
			return nil, err
		}
	}
	if current.ID != "" && current.Config["default.reference.value"] == want["default.reference.value"] &&
		current.Config["default.reference.maxAge"] == want["default.reference.maxAge"] {
		return nil, nil
	}
	execID := exec.ID
	cfgAlias := alias + "-amr-" + value
	before := current.Config["default.reference.value"]
	return &flowOp{
		field: FieldChange{Field: "executions." + name + ".amr_reference", Before: nilIfEmpty(before), After: value},
		run: func(ctx context.Context) error {
			if current.ID == "" {
				return kc.CreateExecutionConfig(ctx, realm, execID, keycloak.AuthenticatorConfig{Alias: cfgAlias, Config: want})
			}
			merged := map[string]string{}
			for k, v := range current.Config {
				merged[k] = v
			}
			for k, v := range want {
				merged[k] = v
			}
			return kc.UpdateAuthenticatorConfig(ctx, realm, keycloak.AuthenticatorConfig{ID: current.ID, Alias: current.Alias, Config: merged})
		},
	}, nil
}

// otpFormProvider is the authenticator of the post-broker flow.
const otpFormProvider = "auth-otp-form"

// postBrokerOTP returns the top-level OTP form execution of the post-broker
// flow, if there is one.
func postBrokerOTP(execs []keycloak.Execution) (keycloak.Execution, bool) {
	for _, e := range execs {
		if e.Level == 0 && e.ProviderID == otpFormProvider {
			return e, true
		}
	}
	return keycloak.Execution{}, false
}

// postBrokerOps returns what an existing post-broker flow needs to match
// Core's layout: a required OTP form with the otp AMR reference. When the
// form is missing, the only op adds it; runPostBrokerFlow then converges
// the rest.
func postBrokerOps(ctx context.Context, kc *keycloak.Client, realm string, execs []keycloak.Execution) ([]flowOp, error) {
	otp, ok := postBrokerOTP(execs)
	if !ok {
		return []flowOp{{
			field: FieldChange{Field: "executions.auth-otp-form", After: "added"},
			run: func(ctx context.Context) error {
				return kc.AddExecution(ctx, realm, FlowPostBrokerMFA, otpFormProvider)
			},
		}}, nil
	}
	var ops []flowOp
	if otp.Requirement != "REQUIRED" {
		id := otp.ID
		ops = append(ops, flowOp{
			field: FieldChange{Field: "executions.auth-otp-form.requirement", Before: otp.Requirement, After: "REQUIRED"},
			run: func(ctx context.Context) error {
				return kc.UpdateExecutionRequirement(ctx, realm, FlowPostBrokerMFA, id, "REQUIRED")
			},
		})
	}
	op, err := amrOp(ctx, kc, realm, FlowPostBrokerMFA, otpFormProvider, otp, "otp")
	if err != nil {
		return nil, err
	}
	if op != nil {
		ops = append(ops, *op)
	}
	return ops, nil
}

// postBrokerFields lists the fields of a post-broker flow change. When the
// OTP form is missing, the requirement and AMR fields follow its addition.
func postBrokerFields(ops []flowOp) []FieldChange {
	fields := make([]FieldChange, 0, len(ops)+2)
	for _, op := range ops {
		fields = append(fields, op.field)
	}
	if len(ops) == 1 && ops[0].field.Field == "executions.auth-otp-form" {
		fields = append(fields, FieldChange{Field: "executions.auth-otp-form.requirement", After: "REQUIRED"},
			FieldChange{Field: "executions.auth-otp-form.amr_reference", After: "otp"})
	}
	return fields
}

// runPostBrokerFlow reads the post-broker flow and converges it, adding the
// OTP form first when it is missing.
func runPostBrokerFlow(ctx context.Context, kc *keycloak.Client, realm string) error {
	for range 2 {
		execs, err := kc.Executions(ctx, realm, FlowPostBrokerMFA)
		if err != nil {
			return err
		}
		ops, err := postBrokerOps(ctx, kc, realm, execs)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if err := op.run(ctx); err != nil {
				return err
			}
		}
		if _, ok := postBrokerOTP(execs); ok {
			return nil
		}
	}
	return fmt.Errorf("flow %s has no OTP form after adding one: %w", FlowPostBrokerMFA, keycloak.ErrNotFound)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func planFlows(ctx context.Context, kc *keycloak.Client, realm string, realmExists bool) ([]Change, error) {
	existing := map[string]bool{}
	if realmExists {
		flows, err := kc.Flows(ctx, realm)
		if err != nil {
			return nil, err
		}
		for _, f := range flows {
			existing[f.Alias] = true
		}
	}
	var changes []Change
	for _, spec := range browserFlows {
		alias, mfa := spec.alias, spec.mfa
		if !existing[alias] {
			fields := []FieldChange{{Field: "copy_of", After: builtinFlow}}
			if spec.noCookie {
				fields = append(fields, FieldChange{Field: "executions." + cookieProvider + ".requirement", After: "DISABLED"})
			}
			if mfa {
				fields = append(fields, FieldChange{Field: "executions.conditional-2fa-subflow.requirement", After: "REQUIRED"},
					FieldChange{Field: "executions.auth-otp-form.requirement", After: "REQUIRED"})
			}
			fields = append(fields, FieldChange{Field: "executions.auth-username-password-form.amr_reference", After: "pwd"},
				FieldChange{Field: "executions.auth-otp-form.amr_reference", After: "otp"})
			changes = append(changes, Change{
				Target: TargetFlow, Name: alias, Action: ActionCreate, Fields: fields,
				run: func(ctx context.Context, st *runState) error {
					if err := kc.CopyFlow(ctx, realm, builtinFlow, alias); err != nil {
						return err
					}
					return runFlowOps(ctx, kc, realm, spec)
				},
			})
			continue
		}
		execs, err := kc.Executions(ctx, realm, alias)
		if err != nil {
			return nil, err
		}
		ops, err := flowOps(ctx, kc, realm, spec, execs)
		if err != nil {
			return nil, err
		}
		if len(ops) == 0 {
			continue
		}
		fields := make([]FieldChange, len(ops))
		for i, op := range ops {
			fields[i] = op.field
		}
		changes = append(changes, Change{
			Target: TargetFlow, Name: alias, Action: ActionUpdate, Fields: fields,
			run: func(ctx context.Context, st *runState) error { return runFlowOps(ctx, kc, realm, spec) },
		})
	}

	post, err := planPostBrokerFlow(ctx, kc, realm, existing[FlowPostBrokerMFA])
	if err != nil {
		return nil, err
	}
	changes = append(changes, post...)
	staffPost, err := planStaffPostBrokerFlow(ctx, kc, realm, existing[FlowPostBrokerStaffMFA])
	if err != nil {
		return nil, err
	}
	return append(changes, staffPost...), nil
}

// planPostBrokerFlow returns the change that creates or converges
// FlowPostBrokerMFA.
func planPostBrokerFlow(ctx context.Context, kc *keycloak.Client, realm string, exists bool) ([]Change, error) {
	run := func(ctx context.Context, st *runState) error { return runPostBrokerFlow(ctx, kc, realm) }
	if !exists {
		return []Change{{
			Target: TargetFlow, Name: FlowPostBrokerMFA, Action: ActionCreate,
			Fields: []FieldChange{
				{Field: "executions.auth-otp-form", After: "added"},
				{Field: "executions.auth-otp-form.requirement", After: "REQUIRED"},
				{Field: "executions.auth-otp-form.amr_reference", After: "otp"},
			},
			run: func(ctx context.Context, st *runState) error {
				if err := kc.CreateFlow(ctx, realm, FlowPostBrokerMFA, "Core: second factor after a login through an identity provider"); err != nil {
					return err
				}
				return run(ctx, st)
			},
		}}, nil
	}
	execs, err := kc.Executions(ctx, realm, FlowPostBrokerMFA)
	if err != nil {
		return nil, err
	}
	ops, err := postBrokerOps(ctx, kc, realm, execs)
	if err != nil || len(ops) == 0 {
		return nil, err
	}
	return []Change{{Target: TargetFlow, Name: FlowPostBrokerMFA, Action: ActionUpdate, Fields: postBrokerFields(ops), run: run}}, nil
}

// runFlowOps reads the flow again and converges it, so it is correct whether
// the flow was just copied or already existed.
func runFlowOps(ctx context.Context, kc *keycloak.Client, realm string, spec browserFlowSpec) error {
	execs, err := kc.Executions(ctx, realm, spec.alias)
	if err != nil {
		return err
	}
	ops, err := flowOps(ctx, kc, realm, spec, execs)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if err := op.run(ctx); err != nil {
			return err
		}
	}
	return nil
}

func planRequiredActions(ctx context.Context, kc *keycloak.Client, m *Manifest, realmExists bool) ([]Change, error) {
	realm := m.Identity.Realm
	names := make([]string, 0, len(requiredActionAliases))
	for name := range requiredActionAliases {
		names = append(names, name)
	}
	sort.Strings(names)
	var changes []Change
	for _, name := range names {
		alias := requiredActionAliases[name]
		listed := containsString(m.Identity.RequiredActions, name)
		// verify_email is enforced through the realm's verifyEmail switch;
		// as a default action it would also hit users whose address an
		// identity provider already verified.
		wantDefault := listed && name != "verify_email"
		wantEnabled := listed || (name == "configure_totp" && m.Identity.MFA != MFAOptional)

		var current keycloak.RequiredAction
		if realmExists {
			var err error
			current, err = kc.GetRequiredAction(ctx, realm, alias)
			if errors.Is(err, keycloak.ErrNotFound) {
				return nil, fmt.Errorf("keycloak required action %s is not registered in realm %s: %w", alias, realm, err)
			}
			if err != nil {
				return nil, err
			}
		}
		var fields []FieldChange
		if current.DefaultAction != wantDefault && (realmExists || wantDefault) {
			fields = append(fields, FieldChange{Field: "identity.required_actions." + name + ".default_action", Before: current.DefaultAction, After: wantDefault})
		}
		if wantEnabled && !current.Enabled && realmExists {
			fields = append(fields, FieldChange{Field: "identity.required_actions." + name + ".enabled", Before: false, After: true})
		}
		if len(fields) == 0 {
			continue
		}
		changes = append(changes, Change{
			Target: TargetRequiredAction, Name: alias, Action: ActionUpdate, Fields: fields,
			run: func(ctx context.Context, st *runState) error {
				ra, err := kc.GetRequiredAction(ctx, realm, alias)
				if err != nil {
					return err
				}
				ra.DefaultAction = wantDefault
				if wantEnabled {
					ra.Enabled = true
				}
				return kc.UpdateRequiredAction(ctx, realm, ra)
			},
		})
	}
	return changes, nil
}

// clientSpec is the desired state of one client.
type clientSpec struct {
	path     string
	clientID string
	fields   []field
	secret   string // empty for a public client
	mappers  []keycloak.ProtocolMapper
}

func audienceMapper(clientID string) keycloak.ProtocolMapper {
	return keycloak.ProtocolMapper{
		Name: "zs-audience", Protocol: "openid-connect", ProtocolMapper: "oidc-audience-mapper",
		Config: map[string]string{
			"included.client.audience":  clientID,
			"access.token.claim":        "true",
			"id.token.claim":            "false",
			"introspection.token.claim": "true",
		},
	}
}

func amrMapper() keycloak.ProtocolMapper {
	return keycloak.ProtocolMapper{
		Name: "zs-amr", Protocol: "openid-connect", ProtocolMapper: "oidc-amr-mapper",
		Config: map[string]string{
			"access.token.claim":        "true",
			"id.token.claim":            "true",
			"introspection.token.claim": "true",
		},
	}
}

func clientSpecs(m *Manifest, sec resolved) []clientSpec {
	id := m.Identity
	cc := id.CustomerClient
	customer := clientSpec{
		path: "identity.customer_client", clientID: cc.ClientID,
		fields: []field{
			{"identity.customer_client.enabled", "enabled", true},
			{"identity.customer_client.protocol", "protocol", "openid-connect"},
			{"identity.customer_client.public_client", "publicClient", true},
			{"identity.customer_client.standard_flow", "standardFlowEnabled", true},
			{"identity.customer_client.implicit_flow", "implicitFlowEnabled", false},
			{"identity.customer_client.direct_access_grants", "directAccessGrantsEnabled", cc.DirectAccessGrants},
			{"identity.customer_client.service_accounts", "serviceAccountsEnabled", false},
			{"identity.customer_client.redirect_uris", "redirectUris", sortedCopy(cc.RedirectURIs)},
		},
		mappers: []keycloak.ProtocolMapper{audienceMapper(cc.ClientID), amrMapper()},
	}
	if cc.Name != "" {
		customer.fields = append(customer.fields, field{"identity.customer_client.name", "name", cc.Name})
	}
	if cc.WebOrigins != nil {
		customer.fields = append(customer.fields, field{"identity.customer_client.web_origins", "webOrigins", sortedCopy(cc.WebOrigins)})
	}

	sc := id.StaffClient
	staff := clientSpec{
		path: "identity.staff_client", clientID: sc.ClientID, secret: sec[sc.SecretRef],
		fields: []field{
			{"identity.staff_client.enabled", "enabled", true},
			{"identity.staff_client.protocol", "protocol", "openid-connect"},
			{"identity.staff_client.public_client", "publicClient", false},
			{"identity.staff_client.client_authenticator", "clientAuthenticatorType", "client-secret"},
			{"identity.staff_client.standard_flow", "standardFlowEnabled", true},
			{"identity.staff_client.implicit_flow", "implicitFlowEnabled", false},
			{"identity.staff_client.direct_access_grants", "directAccessGrantsEnabled", sc.DirectAccessGrants},
			{"identity.staff_client.service_accounts", "serviceAccountsEnabled", sc.ServiceAccounts},
			{"identity.staff_client.redirect_uris", "redirectUris", sortedCopy(sc.RedirectURIs)},
		},
		mappers: []keycloak.ProtocolMapper{audienceMapper(sc.ClientID), amrMapper()},
	}
	if sc.Name != "" {
		staff.fields = append(staff.fields, field{"identity.staff_client.name", "name", sc.Name})
	}
	if sc.WebOrigins != nil {
		staff.fields = append(staff.fields, field{"identity.staff_client.web_origins", "webOrigins", sortedCopy(sc.WebOrigins)})
	}

	specs := []clientSpec{customer, staff}
	for _, svc := range id.ServiceClients {
		path := fmt.Sprintf("identity.service_clients[%s]", svc.ClientID)
		s := clientSpec{
			path: path, clientID: svc.ClientID, secret: sec[svc.SecretRef],
			fields: []field{
				{path + ".enabled", "enabled", true},
				{path + ".protocol", "protocol", "openid-connect"},
				{path + ".public_client", "publicClient", false},
				{path + ".client_authenticator", "clientAuthenticatorType", "client-secret"},
				{path + ".standard_flow", "standardFlowEnabled", false},
				{path + ".implicit_flow", "implicitFlowEnabled", false},
				{path + ".direct_access_grants", "directAccessGrantsEnabled", false},
				{path + ".service_accounts", "serviceAccountsEnabled", true},
			},
			mappers: []keycloak.ProtocolMapper{audienceMapper(svc.ClientID)},
		}
		if svc.Name != "" {
			s.fields = append(s.fields, field{path + ".name", "name", svc.Name})
		}
		specs = append(specs, s)
	}
	return specs
}

const pkceAttribute = "pkce.code.challenge.method"

// planClients returns the client changes and the current staff client
// representation, which planMFA needs.
func planClients(ctx context.Context, kc *keycloak.Client, m *Manifest, sec resolved, realmExists bool) ([]Change, keycloak.Object, error) {
	realm := m.Identity.Realm
	var changes []Change
	var staffCurrent keycloak.Object
	for _, spec := range clientSpecs(m, sec) {
		spec := spec
		public := spec.secret == ""
		var current keycloak.Object
		if realmExists {
			cl, err := kc.FindClient(ctx, realm, spec.clientID)
			if err != nil && !errors.Is(err, keycloak.ErrNotFound) {
				return nil, nil, err
			}
			current = cl
		}
		if spec.clientID == m.Identity.StaffClient.ClientID {
			staffCurrent = current
		}
		wantPKCE := spec.path == "identity.customer_client" || spec.path == "identity.staff_client"

		if current == nil {
			fields, rep := allFields(spec.fields)
			rep["clientId"] = spec.clientID
			if wantPKCE {
				rep["attributes"] = map[string]string{pkceAttribute: "S256"}
				fields = append(fields, FieldChange{Field: spec.path + ".pkce", After: "S256"})
			}
			if !public {
				rep["secret"] = spec.secret
				fields = append(fields, FieldChange{Field: spec.path + ".secret", Redacted: true})
			}
			for _, mp := range spec.mappers {
				fields = append(fields, FieldChange{Field: spec.path + ".protocol_mappers." + mp.Name, After: mp.ProtocolMapper})
			}
			changes = append(changes, Change{
				Target: TargetClient, Name: spec.clientID, Action: ActionCreate, Fields: fields,
				run: func(ctx context.Context, st *runState) error {
					uuid, err := kc.CreateClient(ctx, realm, rep)
					if err != nil {
						return err
					}
					for _, mp := range spec.mappers {
						if err := kc.CreateProtocolMapper(ctx, realm, uuid, mp); err != nil {
							return err
						}
					}
					return nil
				},
			})
			continue
		}

		uuid, _ := current["id"].(string)
		fields, update := compareFields(current, spec.fields)
		if wantPKCE {
			attrs, _ := current["attributes"].(map[string]any)
			if got, _ := attrs[pkceAttribute].(string); got != "S256" {
				fields = append(fields, FieldChange{Field: spec.path + ".pkce", Before: nilIfEmpty(got), After: "S256"})
				update["attributes"] = map[string]string{pkceAttribute: "S256"}
			}
		}
		if !public {
			got, err := kc.ClientSecret(ctx, realm, uuid)
			if err != nil {
				return nil, nil, err
			}
			if !secretEqual(got, spec.secret) {
				fields = append(fields, FieldChange{Field: spec.path + ".secret", Redacted: true})
				update["secret"] = spec.secret
			}
		}
		mappers, err := kc.ProtocolMappers(ctx, realm, uuid)
		if err != nil {
			return nil, nil, err
		}
		var mapperOps []func(ctx context.Context) error
		for _, want := range spec.mappers {
			want := want
			var have *keycloak.ProtocolMapper
			for i := range mappers {
				if mappers[i].Name == want.Name {
					have = &mappers[i]
				}
			}
			switch {
			case have == nil:
				fields = append(fields, FieldChange{Field: spec.path + ".protocol_mappers." + want.Name, After: want.ProtocolMapper})
				mapperOps = append(mapperOps, func(ctx context.Context) error { return kc.CreateProtocolMapper(ctx, realm, uuid, want) })
			case !mapperMatches(*have, want):
				fields = append(fields, FieldChange{Field: spec.path + ".protocol_mappers." + want.Name, Before: have.ProtocolMapper, After: want.ProtocolMapper})
				merged := *have
				merged.ProtocolMapper = want.ProtocolMapper
				merged.Protocol = want.Protocol
				merged.Config = map[string]string{}
				for k, v := range have.Config {
					merged.Config[k] = v
				}
				for k, v := range want.Config {
					merged.Config[k] = v
				}
				mapperOps = append(mapperOps, func(ctx context.Context) error { return kc.UpdateProtocolMapper(ctx, realm, uuid, merged) })
			}
		}
		if len(fields) == 0 {
			continue
		}
		changes = append(changes, Change{
			Target: TargetClient, Name: spec.clientID, Action: ActionUpdate, Fields: fields,
			run: func(ctx context.Context, st *runState) error {
				if len(update) > 0 {
					if err := kc.UpdateClient(ctx, realm, uuid, update); err != nil {
						return err
					}
				}
				for _, op := range mapperOps {
					if err := op(ctx); err != nil {
						return err
					}
				}
				return nil
			},
		})
	}
	return changes, staffCurrent, nil
}

func mapperMatches(have, want keycloak.ProtocolMapper) bool {
	if have.ProtocolMapper != want.ProtocolMapper || have.Protocol != want.Protocol {
		return false
	}
	for k, v := range want.Config {
		if have.Config[k] != v {
			return false
		}
	}
	return true
}

var samlNameIDFormats = map[string]string{
	"email":       "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress",
	"persistent":  "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent",
	"unspecified": "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified",
	"transient":   "urn:oasis:names:tc:SAML:2.0:nameid-format:transient",
}

// idpConfig returns the managed config keys of a provider, without the
// secret.
func idpConfig(p IdentityProvider) map[string]string {
	cfg := map[string]string{}
	switch p.Type {
	case "oidc":
		cfg["clientId"] = p.ClientID
		cfg["issuer"] = p.Issuer
		cfg["authorizationUrl"] = p.AuthorizationURL
		cfg["tokenUrl"] = p.TokenURL
		cfg["jwksUrl"] = p.JWKSURL
		cfg["useJwksUrl"] = "true"
		cfg["validateSignature"] = "true"
		cfg["clientAuthMethod"] = "client_secret_post"
	case "google", "microsoft", "github":
		cfg["clientId"] = p.ClientID
	case "saml":
		cfg["idpEntityId"] = p.IDPEntityID
		cfg["singleSignOnServiceUrl"] = p.SSOURL
		cfg["postBindingResponse"] = "true"
		if p.SPEntityID != "" {
			cfg["entityId"] = p.SPEntityID
		}
		if p.NameIDFormat != "" {
			cfg["nameIDPolicyFormat"] = samlNameIDFormats[p.NameIDFormat]
		}
		if p.SigningCertificate != "" {
			cfg["signingCertificate"] = p.SigningCertificate
			cfg["validateSignature"] = "true"
		} else {
			cfg["validateSignature"] = "false"
		}
	}
	if p.DefaultScopes != "" {
		cfg["defaultScope"] = p.DefaultScopes
	}
	return cfg
}

func secretDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func planIdentityProviders(ctx context.Context, kc *keycloak.Client, m *Manifest, sec resolved, realmExists bool) ([]Change, error) {
	realm := m.Identity.Realm
	postBroker := postBrokerFlow(m)
	var changes []Change
	for _, p := range m.Identity.IdentityProviders {
		p := p
		path := "identity.identity_providers[" + p.Alias + "]"
		cfg := idpConfig(p)
		secret := ""
		if p.ClientSecretRef != "" {
			secret = sec[p.ClientSecretRef]
		}
		var current keycloak.IdentityProvider
		found := false
		if realmExists {
			var err error
			current, err = kc.GetIdentityProvider(ctx, realm, p.Alias)
			switch {
			case err == nil:
				found = true
			case errors.Is(err, keycloak.ErrNotFound):
			default:
				return nil, err
			}
		}
		if !found {
			full := map[string]string{}
			for k, v := range cfg {
				full[k] = v
			}
			fields := []FieldChange{{Field: path + ".type", After: p.Type}, {Field: path + ".enabled", After: p.IsEnabled()}}
			if p.DisplayName != "" {
				fields = append(fields, FieldChange{Field: path + ".display_name", After: p.DisplayName})
			}
			for _, k := range sortedKeys(cfg) {
				fields = append(fields, FieldChange{Field: path + ".config." + k, After: cfg[k]})
			}
			if postBroker != "" {
				fields = append(fields, FieldChange{Field: path + ".post_broker_login_flow", After: postBroker})
			}
			if secret != "" {
				full["clientSecret"] = secret
				full[idpSecretDigestKey] = secretDigest(secret)
				fields = append(fields, FieldChange{Field: path + ".client_secret", Redacted: true})
			}
			rep := keycloak.IdentityProvider{Alias: p.Alias, DisplayName: p.DisplayName, ProviderID: p.Type, Enabled: p.IsEnabled(),
				PostBrokerLoginFlowAlias: postBroker, Config: full}
			changes = append(changes, Change{
				Target: TargetIdentityProvider, Name: p.Alias, Action: ActionCreate, Fields: fields,
				run: func(ctx context.Context, st *runState) error { return kc.CreateIdentityProvider(ctx, realm, rep) },
			})
			continue
		}
		if current.ProviderID != p.Type {
			return nil, fmt.Errorf("identity provider %s is a %s provider in Keycloak and the manifest declares %s; rename the alias to change the type: %w",
				p.Alias, current.ProviderID, p.Type, keycloak.ErrConflict)
		}
		var fields []FieldChange
		if current.Enabled != p.IsEnabled() {
			fields = append(fields, FieldChange{Field: path + ".enabled", Before: current.Enabled, After: p.IsEnabled()})
		}
		if p.DisplayName != "" && current.DisplayName != p.DisplayName {
			fields = append(fields, FieldChange{Field: path + ".display_name", Before: nilIfEmpty(current.DisplayName), After: p.DisplayName})
		}
		for _, k := range sortedKeys(cfg) {
			if current.Config[k] != cfg[k] {
				fields = append(fields, FieldChange{Field: path + ".config." + k, Before: nilIfEmpty(current.Config[k]), After: cfg[k]})
			}
		}
		if current.PostBrokerLoginFlowAlias != postBroker {
			fields = append(fields, FieldChange{Field: path + ".post_broker_login_flow", Before: nilIfEmpty(current.PostBrokerLoginFlowAlias), After: nilIfEmpty(postBroker)})
		}
		secretChanged := secret != "" && !secretEqual(current.Config[idpSecretDigestKey], secretDigest(secret))
		if secretChanged {
			fields = append(fields, FieldChange{Field: path + ".client_secret", Redacted: true})
		}
		if len(fields) == 0 {
			continue
		}
		merged := map[string]string{}
		for k, v := range current.Config {
			merged[k] = v
		}
		for k, v := range cfg {
			merged[k] = v
		}
		// Keycloak 26.8.1 refuses an update that changes the token URL, the
		// client ID or the authentication method without the secret, so
		// every update carries the resolved secret when the manifest has
		// one. The mask keeps the stored secret of a provider without one.
		if secret != "" {
			merged["clientSecret"] = secret
			merged[idpSecretDigestKey] = secretDigest(secret)
		} else if _, ok := merged["clientSecret"]; ok {
			merged["clientSecret"] = keycloak.SecretMask
		}
		rep := current
		rep.DisplayName = p.DisplayName
		if rep.DisplayName == "" {
			rep.DisplayName = current.DisplayName
		}
		rep.Enabled = p.IsEnabled()
		rep.PostBrokerLoginFlowAlias = postBroker
		rep.Config = merged
		changes = append(changes, Change{
			Target: TargetIdentityProvider, Name: p.Alias, Action: ActionUpdate, Fields: fields,
			run: func(ctx context.Context, st *runState) error { return kc.UpdateIdentityProvider(ctx, realm, rep) },
		})
	}
	return changes, nil
}

// postBrokerFlow returns the post-broker-login flow every identity provider
// of the brand must have: FlowPostBrokerMFA when mfa is required, so a login
// through a provider needs the second factor too, FlowPostBrokerStaffMFA when
// mfa is required_for_staff, so a login through a provider for the staff
// client needs it, and none otherwise.
func postBrokerFlow(m *Manifest) string {
	switch m.Identity.MFA {
	case MFARequired:
		return FlowPostBrokerMFA
	case MFARequiredForStaff:
		return FlowPostBrokerStaffMFA
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// currentMFA describes the policy the realm's flow bindings implement. With
// required and required_for_staff the staff client is bound to FlowStaffMFA,
// so a realm that binds it to anything else, such as FlowMFA before Core
// added FlowStaffMFA, does not implement the policy and apply converges it.
func currentMFA(realmRep, staffClient keycloak.Object, flowIDs map[string]string) string {
	browser, _ := realmRep["browserFlow"].(string)
	staffOverride := ""
	if staffClient != nil {
		if overrides, ok := staffClient["authenticationFlowBindingOverrides"].(map[string]any); ok {
			staffOverride, _ = overrides["browser"].(string)
		}
	}
	staffID := flowIDs[FlowStaffMFA]
	switch {
	case browser == FlowMFA && staffID != "" && staffOverride == staffID:
		return MFARequired
	case browser == FlowOptional && staffOverride == "":
		return MFAOptional
	case browser == FlowOptional && staffID != "" && staffOverride == staffID:
		return MFARequiredForStaff
	}
	if staffOverride != "" {
		return fmt.Sprintf("unmanaged (browser flow %q, staff override)", browser)
	}
	return fmt.Sprintf("unmanaged (browser flow %q)", browser)
}

func planMFA(ctx context.Context, kc *keycloak.Client, m *Manifest, realmRep, staffClient keycloak.Object, realmExists bool) ([]Change, error) {
	realm := m.Identity.Realm
	want := m.Identity.MFA
	var before any
	if realmExists {
		flows, err := kc.Flows(ctx, realm)
		if err != nil {
			return nil, err
		}
		ids := map[string]string{}
		for _, f := range flows {
			ids[f.Alias] = f.ID
		}
		cur := currentMFA(realmRep, staffClient, ids)
		if cur == want {
			return nil, nil
		}
		before = cur
	}
	staffID := m.Identity.StaffClient.ClientID
	return []Change{{
		Target: TargetMFAPolicy, Name: realm, Action: ActionUpdate,
		Fields: []FieldChange{{Field: "identity.mfa", Before: before, After: want}},
		run: func(ctx context.Context, st *runState) error {
			flows, err := kc.Flows(ctx, realm)
			if err != nil {
				return err
			}
			staffFlowID := ""
			for _, f := range flows {
				if f.Alias == FlowStaffMFA {
					staffFlowID = f.ID
				}
			}
			if staffFlowID == "" {
				return fmt.Errorf("flow %s is missing: %w", FlowStaffMFA, keycloak.ErrNotFound)
			}
			browser, override := FlowOptional, ""
			switch want {
			case MFARequired:
				browser, override = FlowMFA, staffFlowID
			case MFARequiredForStaff:
				override = staffFlowID
			}
			if err := kc.UpdateRealm(ctx, realm, keycloak.Object{"browserFlow": browser}); err != nil {
				return err
			}
			staff, err := kc.FindClient(ctx, realm, staffID)
			if err != nil {
				return err
			}
			uuid, _ := staff["id"].(string)
			// An empty value removes the override.
			return kc.UpdateClient(ctx, realm, uuid, keycloak.Object{"authenticationFlowBindingOverrides": map[string]string{"browser": override}})
		},
	}}, nil
}
