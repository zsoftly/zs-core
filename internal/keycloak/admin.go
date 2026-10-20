package keycloak

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/zsoftly/zs-core/internal/upstream"
)

// Object is a JSON object as Keycloak sends it. Realms and clients carry
// dozens of fields Core does not manage; keeping them as maps means an update
// sends only the fields Core manages and Keycloak keeps the rest.
type Object = map[string]any

// GetRealm returns the realm representation, or ErrNotFound.
func (c *Client) GetRealm(ctx context.Context, realm string) (Object, error) {
	var rep Object
	_, err := c.call(ctx, "get realm", http.MethodGet, realmPath(realm), nil, &rep)
	return rep, err
}

// CreateRealm creates a realm. rep must hold "realm".
func (c *Client) CreateRealm(ctx context.Context, rep Object) error {
	_, err := c.call(ctx, "create realm", http.MethodPost, "/admin/realms", rep, nil)
	return err
}

// UpdateRealm updates the fields present in rep and leaves the others.
func (c *Client) UpdateRealm(ctx context.Context, realm string, rep Object) error {
	_, err := c.call(ctx, "update realm", http.MethodPut, realmPath(realm), rep, nil)
	return err
}

// FindClient returns the client with the given clientId, or ErrNotFound.
func (c *Client) FindClient(ctx context.Context, realm, clientID string) (Object, error) {
	var list []Object
	q := url.Values{"clientId": {clientID}}
	if _, err := c.call(ctx, "find client", http.MethodGet, realmPath(realm)+"/clients?"+q.Encode(), nil, &list); err != nil {
		return nil, err
	}
	for _, cl := range list {
		if id, _ := cl["clientId"].(string); id == clientID {
			return cl, nil
		}
	}
	return nil, fmt.Errorf("keycloak find client: %w", ErrNotFound)
}

// CreateClient creates a client and returns its internal ID.
func (c *Client) CreateClient(ctx context.Context, realm string, rep Object) (string, error) {
	resp, err := c.call(ctx, "create client", http.MethodPost, realmPath(realm)+"/clients", rep, nil)
	if err != nil {
		return "", err
	}
	if id := locationID(resp); id != "" {
		return id, nil
	}
	clientID, _ := rep["clientId"].(string)
	created, err := c.FindClient(ctx, realm, clientID)
	if err != nil {
		return "", err
	}
	id, _ := created["id"].(string)
	return id, nil
}

// UpdateClient updates the fields present in rep.
func (c *Client) UpdateClient(ctx context.Context, realm, id string, rep Object) error {
	_, err := c.call(ctx, "update client", http.MethodPut, realmPath(realm)+"/clients/"+url.PathEscape(id), rep, nil)
	return err
}

// ClientSecret returns the current secret of a confidential client.
func (c *Client) ClientSecret(ctx context.Context, realm, id string) (string, error) {
	var cred struct {
		Value string `json:"value"`
	}
	_, err := c.call(ctx, "get client secret", http.MethodGet, realmPath(realm)+"/clients/"+url.PathEscape(id)+"/client-secret", nil, &cred)
	return cred.Value, err
}

// ProtocolMapper is a client protocol mapper.
type ProtocolMapper struct {
	ID             string            `json:"id,omitempty"`
	Name           string            `json:"name"`
	Protocol       string            `json:"protocol"`
	ProtocolMapper string            `json:"protocolMapper"`
	Config         map[string]string `json:"config"`
}

func mappersPath(realm, clientUUID string) string {
	return realmPath(realm) + "/clients/" + url.PathEscape(clientUUID) + "/protocol-mappers/models"
}

// ProtocolMappers lists the protocol mappers of a client.
func (c *Client) ProtocolMappers(ctx context.Context, realm, clientUUID string) ([]ProtocolMapper, error) {
	var list []ProtocolMapper
	_, err := c.call(ctx, "list protocol mappers", http.MethodGet, mappersPath(realm, clientUUID), nil, &list)
	return list, err
}

// CreateProtocolMapper adds a protocol mapper to a client.
func (c *Client) CreateProtocolMapper(ctx context.Context, realm, clientUUID string, m ProtocolMapper) error {
	m.ID = ""
	_, err := c.call(ctx, "create protocol mapper", http.MethodPost, mappersPath(realm, clientUUID), m, nil)
	return err
}

// UpdateProtocolMapper replaces a protocol mapper. m.ID is required.
func (c *Client) UpdateProtocolMapper(ctx context.Context, realm, clientUUID string, m ProtocolMapper) error {
	if m.ID == "" {
		return errors.New("keycloak update protocol mapper: ID is required")
	}
	_, err := c.call(ctx, "update protocol mapper", http.MethodPut, mappersPath(realm, clientUUID)+"/"+url.PathEscape(m.ID), m, nil)
	return err
}

// IdentityProvider is a brokered identity provider. Keycloak masks secrets in
// the config it returns with SecretMask. PostBrokerLoginFlowAlias names the
// flow Keycloak runs after every login through the provider; empty means
// none, and an update without it removes the binding.
type IdentityProvider struct {
	Alias                    string            `json:"alias"`
	DisplayName              string            `json:"displayName,omitempty"`
	ProviderID               string            `json:"providerId"`
	Enabled                  bool              `json:"enabled"`
	TrustEmail               bool              `json:"trustEmail"`
	PostBrokerLoginFlowAlias string            `json:"postBrokerLoginFlowAlias,omitempty"`
	Config                   map[string]string `json:"config"`
}

// SecretMask is what Keycloak returns in place of a stored secret. Sending it
// back on update keeps the stored secret.
const SecretMask = "**********"

func idpPath(realm, alias string) string {
	return realmPath(realm) + "/identity-provider/instances/" + url.PathEscape(alias)
}

// GetIdentityProvider returns the provider with alias, or ErrNotFound.
func (c *Client) GetIdentityProvider(ctx context.Context, realm, alias string) (IdentityProvider, error) {
	var idp IdentityProvider
	_, err := c.call(ctx, "get identity provider", http.MethodGet, idpPath(realm, alias), nil, &idp)
	return idp, err
}

// CreateIdentityProvider creates a provider.
func (c *Client) CreateIdentityProvider(ctx context.Context, realm string, idp IdentityProvider) error {
	_, err := c.call(ctx, "create identity provider", http.MethodPost, realmPath(realm)+"/identity-provider/instances", idp, nil)
	return err
}

// UpdateIdentityProvider replaces a provider.
func (c *Client) UpdateIdentityProvider(ctx context.Context, realm string, idp IdentityProvider) error {
	_, err := c.call(ctx, "update identity provider", http.MethodPut, idpPath(realm, idp.Alias), idp, nil)
	return err
}

// Flow is a top-level authentication flow.
type Flow struct {
	ID         string `json:"id"`
	Alias      string `json:"alias"`
	ProviderID string `json:"providerId"`
	TopLevel   bool   `json:"topLevel"`
	BuiltIn    bool   `json:"builtIn"`
}

// Execution is one row of a flow's execution list, sub-flows included.
type Execution struct {
	ID                   string `json:"id"`
	Requirement          string `json:"requirement"`
	DisplayName          string `json:"displayName"`
	ProviderID           string `json:"providerId,omitempty"`
	AuthenticationFlow   bool   `json:"authenticationFlow,omitempty"`
	FlowID               string `json:"flowId,omitempty"`
	AuthenticationConfig string `json:"authenticationConfig,omitempty"`
	Level                int    `json:"level"`
	Index                int    `json:"index"`
	Priority             int    `json:"priority"`
}

// AuthenticatorConfig is the configuration of one execution.
type AuthenticatorConfig struct {
	ID     string            `json:"id,omitempty"`
	Alias  string            `json:"alias"`
	Config map[string]string `json:"config"`
}

// RequiredAction is a required action provider of a realm.
type RequiredAction struct {
	Alias         string            `json:"alias"`
	Name          string            `json:"name"`
	ProviderID    string            `json:"providerId"`
	Enabled       bool              `json:"enabled"`
	DefaultAction bool              `json:"defaultAction"`
	Priority      int               `json:"priority"`
	Config        map[string]string `json:"config,omitempty"`
}

func authPath(realm string) string { return realmPath(realm) + "/authentication" }

// Flows lists the top-level flows of a realm.
func (c *Client) Flows(ctx context.Context, realm string) ([]Flow, error) {
	var list []Flow
	_, err := c.call(ctx, "list flows", http.MethodGet, authPath(realm)+"/flows", nil, &list)
	return list, err
}

// CopyFlow copies the flow with alias from to a new top-level flow.
func (c *Client) CopyFlow(ctx context.Context, realm, from, newName string) error {
	_, err := c.call(ctx, "copy flow", http.MethodPost, authPath(realm)+"/flows/"+url.PathEscape(from)+"/copy",
		map[string]string{"newName": newName}, nil)
	return err
}

// CreateFlow creates an empty top-level flow of executions.
func (c *Client) CreateFlow(ctx context.Context, realm, alias, description string) error {
	_, err := c.call(ctx, "create flow", http.MethodPost, authPath(realm)+"/flows", Object{
		"alias": alias, "description": description, "providerId": "basic-flow", "topLevel": true, "builtIn": false,
	}, nil)
	return err
}

// AddExecution appends an execution of the authenticator provider to a
// flow. Keycloak adds it as DISABLED unless the provider allows only one
// requirement.
func (c *Client) AddExecution(ctx context.Context, realm, flowAlias, provider string) error {
	_, err := c.call(ctx, "add execution", http.MethodPost, authPath(realm)+"/flows/"+url.PathEscape(flowAlias)+"/executions/execution",
		map[string]string{"provider": provider}, nil)
	return err
}

// Executions lists every execution of a flow, sub-flows included, in order.
func (c *Client) Executions(ctx context.Context, realm, flowAlias string) ([]Execution, error) {
	var list []Execution
	_, err := c.call(ctx, "list executions", http.MethodGet, authPath(realm)+"/flows/"+url.PathEscape(flowAlias)+"/executions", nil, &list)
	return list, err
}

// UpdateExecutionRequirement sets the requirement of one execution and keeps
// its priority. Keycloak 26.8.1 takes the priority from the same request and
// sets it to 0 when the request leaves it out, which moves the execution to
// the front of its flow.
func (c *Client) UpdateExecutionRequirement(ctx context.Context, realm, flowAlias, executionID, requirement string) error {
	exec, err := c.execution(ctx, realm, flowAlias, executionID)
	if err != nil {
		return err
	}
	return c.updateExecution(ctx, realm, flowAlias, executionID, requirement, exec.Priority)
}

// SetExecutionPriority sets the priority of one execution and keeps its
// requirement. A lower priority runs first among the executions of a flow.
func (c *Client) SetExecutionPriority(ctx context.Context, realm, flowAlias, executionID string, priority int) error {
	exec, err := c.execution(ctx, realm, flowAlias, executionID)
	if err != nil {
		return err
	}
	return c.updateExecution(ctx, realm, flowAlias, executionID, exec.Requirement, priority)
}

func (c *Client) execution(ctx context.Context, realm, flowAlias, executionID string) (Execution, error) {
	execs, err := c.Executions(ctx, realm, flowAlias)
	if err != nil {
		return Execution{}, err
	}
	for _, e := range execs {
		if e.ID == executionID {
			return e, nil
		}
	}
	return Execution{}, fmt.Errorf("execution %s of flow %s: %w", executionID, flowAlias, ErrNotFound)
}

func (c *Client) updateExecution(ctx context.Context, realm, flowAlias, executionID, requirement string, priority int) error {
	_, err := c.call(ctx, "update execution", http.MethodPut, authPath(realm)+"/flows/"+url.PathEscape(flowAlias)+"/executions",
		map[string]any{"id": executionID, "requirement": requirement, "priority": priority}, nil)
	return err
}

// GetAuthenticatorConfig returns an execution configuration by ID.
func (c *Client) GetAuthenticatorConfig(ctx context.Context, realm, id string) (AuthenticatorConfig, error) {
	var cfg AuthenticatorConfig
	_, err := c.call(ctx, "get authenticator config", http.MethodGet, authPath(realm)+"/config/"+url.PathEscape(id), nil, &cfg)
	return cfg, err
}

// CreateExecutionConfig attaches a new configuration to an execution.
func (c *Client) CreateExecutionConfig(ctx context.Context, realm, executionID string, cfg AuthenticatorConfig) error {
	cfg.ID = ""
	_, err := c.call(ctx, "create execution config", http.MethodPost, authPath(realm)+"/executions/"+url.PathEscape(executionID)+"/config", cfg, nil)
	return err
}

// UpdateAuthenticatorConfig replaces a configuration. cfg.ID is required.
func (c *Client) UpdateAuthenticatorConfig(ctx context.Context, realm string, cfg AuthenticatorConfig) error {
	if cfg.ID == "" {
		return errors.New("keycloak update authenticator config: ID is required")
	}
	_, err := c.call(ctx, "update authenticator config", http.MethodPut, authPath(realm)+"/config/"+url.PathEscape(cfg.ID), cfg, nil)
	return err
}

// GetRequiredAction returns a required action by alias.
func (c *Client) GetRequiredAction(ctx context.Context, realm, alias string) (RequiredAction, error) {
	var ra RequiredAction
	_, err := c.call(ctx, "get required action", http.MethodGet, authPath(realm)+"/required-actions/"+url.PathEscape(alias), nil, &ra)
	return ra, err
}

// UpdateRequiredAction replaces a required action.
func (c *Client) UpdateRequiredAction(ctx context.Context, realm string, ra RequiredAction) error {
	_, err := c.call(ctx, "update required action", http.MethodPut, authPath(realm)+"/required-actions/"+url.PathEscape(ra.Alias), ra, nil)
	return err
}

// CreateUser creates a user and returns its ID. Used by operators and tests;
// brand apply does not create users.
func (c *Client) CreateUser(ctx context.Context, realm string, rep Object) (string, error) {
	resp, err := c.call(ctx, "create user", http.MethodPost, realmPath(realm)+"/users", rep, nil)
	if err != nil {
		return "", err
	}
	id := locationID(resp)
	if id == "" {
		return "", errors.New("keycloak create user: no Location header")
	}
	return id, nil
}

// DeleteUser deletes a user.
func (c *Client) DeleteUser(ctx context.Context, realm, id string) error {
	_, err := c.call(ctx, "delete user", http.MethodDelete, realmPath(realm)+"/users/"+url.PathEscape(id), nil, nil)
	return err
}

// locationID returns the last path segment of the Location header.
func locationID(resp upstream.Response) string {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil {
		return ""
	}
	id := path.Base(u.Path)
	if id == "." || id == "/" || strings.ContainsAny(id, "/?#") {
		return ""
	}
	return id
}
