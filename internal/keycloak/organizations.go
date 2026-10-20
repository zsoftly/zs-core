package keycloak

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Organization is a Keycloak Organization. The realm must have
// organizationsEnabled set, or every Organizations endpoint answers 404.
// Keycloak 26.8.1 requires a unique name and a unique alias per realm, does
// not require a domain, and refuses an update that changes or omits the
// alias. Attributes are the only searchable custom field: the list endpoint
// matches q=<name>:<value> on them, while search matches names only.
type Organization struct {
	ID          string              `json:"id,omitempty"`
	Name        string              `json:"name"`
	Alias       string              `json:"alias"`
	Enabled     bool                `json:"enabled"`
	Description string              `json:"description,omitempty"`
	Attributes  map[string][]string `json:"attributes,omitempty"`
}

func orgsPath(realm string) string { return realmPath(realm) + "/organizations" }

func orgPath(realm, id string) string { return orgsPath(realm) + "/" + url.PathEscape(id) }

// CreateOrganization creates an organization and returns its ID. A name or
// alias that is already taken returns ErrConflict.
func (c *Client) CreateOrganization(ctx context.Context, realm string, org Organization) (string, error) {
	org.ID = ""
	resp, err := c.call(ctx, "create organization", http.MethodPost, orgsPath(realm), org, nil)
	if err != nil {
		return "", err
	}
	if id := locationID(resp); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("keycloak create organization: no Location header")
}

// GetOrganization returns the organization with id, or ErrNotFound.
func (c *Client) GetOrganization(ctx context.Context, realm, id string) (Organization, error) {
	var org Organization
	_, err := c.call(ctx, "get organization", http.MethodGet, orgPath(realm, id), nil, &org)
	return org, err
}

// FindOrganizationByAttribute returns the organization whose attribute name
// holds value, or ErrNotFound. More than one match is an error.
func (c *Client) FindOrganizationByAttribute(ctx context.Context, realm, name, value string) (Organization, error) {
	q := url.Values{"q": {name + ":" + value}, "first": {"0"}, "max": {"2"}, "briefRepresentation": {"false"}}
	var list []Organization
	if _, err := c.call(ctx, "find organization", http.MethodGet, orgsPath(realm)+"?"+q.Encode(), nil, &list); err != nil {
		return Organization{}, err
	}
	var found []Organization
	for _, o := range list {
		for _, v := range o.Attributes[name] {
			if v == value {
				found = append(found, o)
				break
			}
		}
	}
	switch len(found) {
	case 0:
		return Organization{}, fmt.Errorf("keycloak find organization: %w", ErrNotFound)
	case 1:
		return found[0], nil
	}
	return Organization{}, fmt.Errorf("keycloak find organization: %d organizations carry %s", len(found), name)
}

// UpdateOrganization replaces an organization. org.ID and org.Alias are
// required; Keycloak refuses to change the alias.
func (c *Client) UpdateOrganization(ctx context.Context, realm string, org Organization) error {
	if org.ID == "" || org.Alias == "" {
		return fmt.Errorf("keycloak update organization: ID and alias are required")
	}
	_, err := c.call(ctx, "update organization", http.MethodPut, orgPath(realm, org.ID), org, nil)
	return err
}

// OrganizationMember is a user that belongs to an organization.
type OrganizationMember struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// OrganizationMembers lists every member of an organization.
func (c *Client) OrganizationMembers(ctx context.Context, realm, id string) ([]OrganizationMember, error) {
	const page = 100
	var all []OrganizationMember
	for first := 0; ; first += page {
		q := url.Values{"first": {strconv.Itoa(first)}, "max": {strconv.Itoa(page)}}
		var list []OrganizationMember
		if _, err := c.call(ctx, "list organization members", http.MethodGet, orgPath(realm, id)+"/members?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		all = append(all, list...)
		if len(list) < page {
			return all, nil
		}
	}
}

// AddOrganizationMember adds a user to an organization. A user that is
// already a member returns ErrConflict, and an unknown user ErrNotFound.
func (c *Client) AddOrganizationMember(ctx context.Context, realm, id, userID string) error {
	_, err := c.call(ctx, "add organization member", http.MethodPost, orgPath(realm, id)+"/members", userID, nil)
	return err
}

// RemoveOrganizationMember removes a user from an organization. A user that
// is not a member returns ErrNotFound.
func (c *Client) RemoveOrganizationMember(ctx context.Context, realm, id, userID string) error {
	_, err := c.call(ctx, "remove organization member", http.MethodDelete, orgPath(realm, id)+"/members/"+url.PathEscape(userID), nil, nil)
	return err
}

// ClientScope is a realm client scope.
type ClientScope struct {
	ID          string            `json:"id,omitempty"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Protocol    string            `json:"protocol"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// FindClientScope returns the client scope with name, or ErrNotFound.
func (c *Client) FindClientScope(ctx context.Context, realm, name string) (ClientScope, error) {
	var list []ClientScope
	if _, err := c.call(ctx, "list client scopes", http.MethodGet, realmPath(realm)+"/client-scopes", nil, &list); err != nil {
		return ClientScope{}, err
	}
	for _, s := range list {
		if s.Name == name {
			return s, nil
		}
	}
	return ClientScope{}, fmt.Errorf("keycloak find client scope: %w", ErrNotFound)
}

// CreateClientScope creates a client scope.
func (c *Client) CreateClientScope(ctx context.Context, realm string, s ClientScope) error {
	s.ID = ""
	_, err := c.call(ctx, "create client scope", http.MethodPost, realmPath(realm)+"/client-scopes", s, nil)
	return err
}

// DefaultClientScopes lists the default client scopes of a client.
func (c *Client) DefaultClientScopes(ctx context.Context, realm, clientUUID string) ([]ClientScope, error) {
	var list []ClientScope
	_, err := c.call(ctx, "list default client scopes", http.MethodGet,
		realmPath(realm)+"/clients/"+url.PathEscape(clientUUID)+"/default-client-scopes", nil, &list)
	return list, err
}

// AddDefaultClientScope makes a client scope a default scope of a client.
func (c *Client) AddDefaultClientScope(ctx context.Context, realm, clientUUID, scopeID string) error {
	_, err := c.call(ctx, "add default client scope", http.MethodPut,
		realmPath(realm)+"/clients/"+url.PathEscape(clientUUID)+"/default-client-scopes/"+url.PathEscape(scopeID), nil, nil)
	return err
}

// AddSubFlow appends a sub-flow of executions to a flow. Keycloak adds it as
// DISABLED.
func (c *Client) AddSubFlow(ctx context.Context, realm, flowAlias, alias, description string) error {
	_, err := c.call(ctx, "add sub-flow", http.MethodPost, authPath(realm)+"/flows/"+url.PathEscape(flowAlias)+"/executions/flow",
		map[string]string{"alias": alias, "description": description, "type": "basic-flow", "provider": "registration-page-form"}, nil)
	return err
}
