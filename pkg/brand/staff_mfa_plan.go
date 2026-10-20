package brand

import (
	"context"
	"errors"
	"fmt"

	"github.com/zsoftly/zs-core/internal/keycloak"
)

// Keycloak binds a post-broker-login flow to an identity provider, never to a
// client: a client's authenticationFlowBindingOverrides accept only the
// browser and direct grant flows. With mfa required_for_staff, every identity
// provider therefore runs FlowPostBrokerStaffMFA. Its one sub-flow is
// CONDITIONAL on the "Condition - client scope" authenticator, which matches
// when StaffClientScope is a default client scope of the client that asked
// for the login. Only the staff client has that scope, so a brokered login
// through the staff client must pass the OTP form and a brokered login
// through any other client is not asked for one. A required "Allow access"
// execution follows the sub-flow: Keycloak fails a top-level flow in which
// nothing succeeded, which is what a skipped sub-flow alone would leave.
// Verified on Keycloak 26.8.1 with a brokered login through each client.
const (
	FlowPostBrokerStaffMFA = "zs-post-broker-staff-mfa"
	staffMFASubFlow        = "zs-post-broker-staff-mfa-staff"
	// StaffClientScope is the client scope that marks the staff client. It
	// adds nothing to tokens.
	StaffClientScope        = "zs-staff-client"
	clientScopeCondition    = "conditional-client-scope"
	allowAccess             = "allow-access-authenticator"
	clientScopeConditionCfg = FlowPostBrokerStaffMFA + "-client-scope"
)

// TargetClientScope is a client scope Core manages.
const TargetClientScope = "keycloak.client_scope"

// staffFlowLayout locates the executions of FlowPostBrokerStaffMFA.
type staffFlowLayout struct {
	sub, condition, otp, allow keycloak.Execution
	hasSub, hasCond            bool
	hasOTP, hasAllow           bool
}

func staffLayoutOf(execs []keycloak.Execution) staffFlowLayout {
	var l staffFlowLayout
	in := false
	for _, e := range execs {
		if e.Level == 0 {
			in = e.AuthenticationFlow && e.DisplayName == staffMFASubFlow
			if in && !l.hasSub {
				l.sub, l.hasSub = e, true
			}
			if e.ProviderID == allowAccess && !l.hasAllow {
				l.allow, l.hasAllow = e, true
			}
			continue
		}
		if !in || e.Level != 1 {
			continue
		}
		switch e.ProviderID {
		case clientScopeCondition:
			if !l.hasCond {
				l.condition, l.hasCond = e, true
			}
		case otpFormProvider:
			if !l.hasOTP {
				l.otp, l.hasOTP = e, true
			}
		}
	}
	return l
}

// staffFlowOps returns what FlowPostBrokerStaffMFA needs to match Core's
// layout. A missing sub-flow or execution is one op that adds it; the run
// then reads the flow again and converges the rest.
func staffFlowOps(ctx context.Context, kc *keycloak.Client, realm string, execs []keycloak.Execution) ([]flowOp, error) {
	l := staffLayoutOf(execs)
	if !l.hasSub {
		return []flowOp{{
			field: FieldChange{Field: "executions." + staffMFASubFlow, After: "added"},
			run: func(ctx context.Context) error {
				return kc.AddSubFlow(ctx, realm, FlowPostBrokerStaffMFA, staffMFASubFlow, "Core: second factor when the staff client asks for the login")
			},
		}}, nil
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
				return kc.UpdateExecutionRequirement(ctx, realm, FlowPostBrokerStaffMFA, id, want)
			},
		})
	}
	require(staffMFASubFlow, l.sub, "CONDITIONAL")
	if !l.hasCond {
		ops = append(ops, flowOp{
			field: FieldChange{Field: "executions." + clientScopeCondition, After: "added"},
			run: func(ctx context.Context) error {
				return kc.AddExecution(ctx, realm, staffMFASubFlow, clientScopeCondition)
			},
		})
	} else {
		require(clientScopeCondition, l.condition, "REQUIRED")
		op, err := configOp(ctx, kc, realm, clientScopeCondition, l.condition, clientScopeConditionCfg,
			map[string]string{"client_scope": StaffClientScope, "negate": "false"})
		if err != nil {
			return nil, err
		}
		if op != nil {
			ops = append(ops, *op)
		}
	}
	if !l.hasOTP {
		ops = append(ops, flowOp{
			field: FieldChange{Field: "executions." + otpFormProvider, After: "added"},
			run:   func(ctx context.Context) error { return kc.AddExecution(ctx, realm, staffMFASubFlow, otpFormProvider) },
		})
	} else {
		require(otpFormProvider, l.otp, "REQUIRED")
		op, err := amrOp(ctx, kc, realm, FlowPostBrokerStaffMFA, otpFormProvider, l.otp, "otp")
		if err != nil {
			return nil, err
		}
		if op != nil {
			ops = append(ops, *op)
		}
	}
	if !l.hasAllow {
		ops = append(ops, flowOp{
			field: FieldChange{Field: "executions." + allowAccess, After: "added"},
			run: func(ctx context.Context) error {
				return kc.AddExecution(ctx, realm, FlowPostBrokerStaffMFA, allowAccess)
			},
		})
	} else {
		require(allowAccess, l.allow, "REQUIRED")
	}
	return ops, nil
}

// configOp returns the op that sets the configuration of an execution to
// want, or nil when it already holds want.
func configOp(ctx context.Context, kc *keycloak.Client, realm, name string, exec keycloak.Execution, alias string, want map[string]string) (*flowOp, error) {
	var current keycloak.AuthenticatorConfig
	if exec.AuthenticationConfig != "" {
		var err error
		if current, err = kc.GetAuthenticatorConfig(ctx, realm, exec.AuthenticationConfig); err != nil {
			return nil, err
		}
	}
	same := current.ID != ""
	for k, v := range want {
		if current.Config[k] != v {
			same = false
		}
	}
	if same {
		return nil, nil
	}
	execID := exec.ID
	return &flowOp{
		field: FieldChange{Field: "executions." + name + ".config", Before: nilIfEmptyMap(current.Config), After: want},
		run: func(ctx context.Context) error {
			if current.ID == "" {
				return kc.CreateExecutionConfig(ctx, realm, execID, keycloak.AuthenticatorConfig{Alias: alias, Config: want})
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

func nilIfEmptyMap(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// staffFlowCreateFields are the fields of a new FlowPostBrokerStaffMFA.
func staffFlowCreateFields() []FieldChange {
	return []FieldChange{
		{Field: "executions." + staffMFASubFlow, After: "added"},
		{Field: "executions." + staffMFASubFlow + ".requirement", After: "CONDITIONAL"},
		{Field: "executions." + clientScopeCondition, After: "added"},
		{Field: "executions." + clientScopeCondition + ".requirement", After: "REQUIRED"},
		{Field: "executions." + clientScopeCondition + ".config", After: map[string]string{"client_scope": StaffClientScope, "negate": "false"}},
		{Field: "executions." + otpFormProvider, After: "added"},
		{Field: "executions." + otpFormProvider + ".requirement", After: "REQUIRED"},
		{Field: "executions." + otpFormProvider + ".amr_reference", After: "otp"},
		{Field: "executions." + allowAccess, After: "added"},
		{Field: "executions." + allowAccess + ".requirement", After: "REQUIRED"},
	}
}

// runStaffFlow reads FlowPostBrokerStaffMFA and converges it. Each pass adds
// at most the missing pieces it can see, so a new flow takes a few passes.
func runStaffFlow(ctx context.Context, kc *keycloak.Client, realm string) error {
	for range 4 {
		execs, err := kc.Executions(ctx, realm, FlowPostBrokerStaffMFA)
		if err != nil {
			return err
		}
		ops, err := staffFlowOps(ctx, kc, realm, execs)
		if err != nil {
			return err
		}
		if len(ops) == 0 {
			return nil
		}
		for _, op := range ops {
			if err := op.run(ctx); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("flow %s does not converge: %w", FlowPostBrokerStaffMFA, keycloak.ErrConflict)
}

// planStaffPostBrokerFlow returns the change that creates or converges
// FlowPostBrokerStaffMFA.
func planStaffPostBrokerFlow(ctx context.Context, kc *keycloak.Client, realm string, exists bool) ([]Change, error) {
	run := func(ctx context.Context, st *runState) error { return runStaffFlow(ctx, kc, realm) }
	if !exists {
		return []Change{{
			Target: TargetFlow, Name: FlowPostBrokerStaffMFA, Action: ActionCreate, Fields: staffFlowCreateFields(),
			run: func(ctx context.Context, st *runState) error {
				if err := kc.CreateFlow(ctx, realm, FlowPostBrokerStaffMFA, "Core: second factor after a login through an identity provider, for the staff client"); err != nil {
					return err
				}
				return run(ctx, st)
			},
		}}, nil
	}
	execs, err := kc.Executions(ctx, realm, FlowPostBrokerStaffMFA)
	if err != nil {
		return nil, err
	}
	ops, err := staffFlowOps(ctx, kc, realm, execs)
	if err != nil || len(ops) == 0 {
		return nil, err
	}
	fields := make([]FieldChange, len(ops))
	for i, op := range ops {
		fields[i] = op.field
	}
	return []Change{{Target: TargetFlow, Name: FlowPostBrokerStaffMFA, Action: ActionUpdate, Fields: fields, run: run}}, nil
}

// planStaffClientScope returns the changes that create StaffClientScope and
// make it a default scope of the staff client.
func planStaffClientScope(ctx context.Context, kc *keycloak.Client, m *Manifest, staffClient keycloak.Object, realmExists bool) ([]Change, error) {
	realm := m.Identity.Realm
	staffID := m.Identity.StaffClient.ClientID
	var changes []Change
	scopeExists := false
	var scope keycloak.ClientScope
	if realmExists {
		var err error
		scope, err = kc.FindClientScope(ctx, realm, StaffClientScope)
		switch {
		case err == nil:
			scopeExists = true
		case errors.Is(err, keycloak.ErrNotFound):
		default:
			return nil, err
		}
	}
	if !scopeExists {
		changes = append(changes, Change{
			Target: TargetClientScope, Name: StaffClientScope, Action: ActionCreate,
			Fields: []FieldChange{
				{Field: "protocol", After: "openid-connect"},
				{Field: "include_in_token_scope", After: false},
			},
			run: func(ctx context.Context, st *runState) error {
				return kc.CreateClientScope(ctx, realm, keycloak.ClientScope{
					Name: StaffClientScope, Protocol: "openid-connect",
					Description: "Core: marks the staff client for the post-broker MFA condition",
					Attributes:  map[string]string{"include.in.token.scope": "false", "display.on.consent.screen": "false"},
				})
			},
		})
	}
	assigned := false
	if scopeExists && staffClient != nil {
		uuid, _ := staffClient["id"].(string)
		defaults, err := kc.DefaultClientScopes(ctx, realm, uuid)
		if err != nil {
			return nil, err
		}
		for _, s := range defaults {
			if s.ID == scope.ID {
				assigned = true
			}
		}
	}
	if !assigned {
		changes = append(changes, Change{
			Target: TargetClient, Name: staffID, Action: ActionUpdate,
			Fields: []FieldChange{{Field: "identity.staff_client.default_client_scopes." + StaffClientScope, After: "added"}},
			run: func(ctx context.Context, st *runState) error {
				s, err := kc.FindClientScope(ctx, realm, StaffClientScope)
				if err != nil {
					return err
				}
				cl, err := kc.FindClient(ctx, realm, staffID)
				if err != nil {
					return err
				}
				uuid, _ := cl["id"].(string)
				return kc.AddDefaultClientScope(ctx, realm, uuid, s.ID)
			},
		})
	}
	return changes, nil
}
