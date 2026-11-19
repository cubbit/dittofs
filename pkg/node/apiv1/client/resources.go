package client

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"net/url"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// Shares is the shares. A share is created with its export policy, by
// CreateShare.
func (c *Client) Shares() Resource[apiv1.ShareSpec, apiv1.ShareStatus] {
	return Resource[apiv1.ShareSpec, apiv1.ShareStatus]{c: c, path: "/api/v1/shares"}
}

// CreateShare creates the share s names, with its export policy.
func (c *Client) CreateShare(ctx context.Context, s apiv1.NewShare) (Tagged[apiv1.ShareSpec, apiv1.ShareStatus], error) {
	return tagged[apiv1.ShareSpec, apiv1.ShareStatus](c.write(ctx, http.MethodPost, "/api/v1/shares", s, "", nil))
}

// ExportPolicies is each share's export policy, named by the share's ID.
func (c *Client) ExportPolicies() Singleton[apiv1.ExportPolicySpec] {
	return Singleton[apiv1.ExportPolicySpec]{c: c, path: "/api/v1/shares", sub: "/export-policy"}
}

// Users is the users. A delete starts an operation. No route serves its
// Action yet.
func (c *Client) Users() Resource[apiv1.UserSpec, struct{}] {
	return Resource[apiv1.UserSpec, struct{}]{c: c, path: "/api/v1/users"}
}

// Groups is the groups. A delete starts an operation. No route serves its
// Action.
func (c *Client) Groups() Resource[apiv1.GroupSpec, struct{}] {
	return Resource[apiv1.GroupSpec, struct{}]{c: c, path: "/api/v1/groups"}
}

// Members is the members of the group whose ID is group, each named by the
// member's ID: put under "*" to add one, deleted to remove it. No route
// serves its List, Get, Create, Patch or Action.
func (c *Client) Members(group string) Resource[struct{}, struct{}] {
	return Resource[struct{}, struct{}]{c: c, path: "/api/v1/groups/" + url.PathEscape(group) + "/members"}
}

// Grants is the grants on the share whose ID is share, each named by its
// principal's ID: listed, read, put and deleted. No route serves its
// Create, Patch or Action.
func (c *Client) Grants(share string) Resource[apiv1.GrantSpec, struct{}] {
	return Resource[apiv1.GrantSpec, struct{}]{c: c, path: "/api/v1/shares/" + url.PathEscape(share) + "/grants"}
}

// Quotas is the quotas on the share whose ID is share on its users and
// groups, each named by the principal's ID: listed, read and put, a put of
// no limit deleting it. No route serves its Create, Patch, Delete or
// Action.
func (c *Client) Quotas(share string) Resource[apiv1.QuotaSpec, struct{}] {
	return Resource[apiv1.QuotaSpec, struct{}]{c: c, path: "/api/v1/shares/" + url.PathEscape(share) + "/quotas"}
}

// ShareQuotas is each share's quota on itself, named by the share's ID. No
// route serves its Patch.
func (c *Client) ShareQuotas() Singleton[apiv1.QuotaSpec] {
	return Singleton[apiv1.QuotaSpec]{c: c, path: "/api/v1/shares", sub: "/quota"}
}

// Netgroups is the netgroups, each named by its name, created by a put
// under "*". No route serves its Create, Patch or Action.
func (c *Client) Netgroups() Resource[apiv1.NetgroupSpec, struct{}] {
	return Resource[apiv1.NetgroupSpec, struct{}]{c: c, path: "/api/v1/netgroups"}
}

// Nodes is the nodes, read only.
func (c *Client) Nodes() Resource[apiv1.NodeSpec, apiv1.NodeStatus] {
	return Resource[apiv1.NodeSpec, apiv1.NodeStatus]{c: c, path: "/api/v1/nodes"}
}

// Operation reads the operation id names.
func (c *Client) Operation(ctx context.Context, id string) (apiv1.Operation, error) {
	return get[apiv1.Operation](ctx, c, "/api/v1/operations/"+url.PathEscape(id))
}

// Operations yields every operation the filter admits.
func (c *Client) Operations(ctx context.Context, filter url.Values) iter.Seq2[apiv1.Operation, error] {
	return list[apiv1.Operation](ctx, c, "/api/v1/operations", filter)
}

// CancelOperation cancels the operation id names.
func (c *Client) CancelOperation(ctx context.Context, id string) error {
	_, err := c.do(ctx, call{method: http.MethodPost, path: "/api/v1/operations/" + url.PathEscape(id) + ":cancel"})
	return err
}

// Audit yields every audit record the filter admits.
func (c *Client) Audit(ctx context.Context, filter url.Values) iter.Seq2[apiv1.AuditRecord, error] {
	return list[apiv1.AuditRecord](ctx, c, "/api/v1/audit", filter)
}

// Health reads the node's readiness and the health conditions it holds.
func (c *Client) Health(ctx context.Context) (apiv1.Health, error) {
	return get[apiv1.Health](ctx, c, "/api/v1/health")
}

// Stores is the remote stores: Create is POST /stores, Get is GET
// /stores/{id} and Delete is DELETE /stores/{id}; no route serves List, Put
// or Patch. A store's settings are each a setting at its scope, and its
// credential is set by SetStoreCredential.
func (c *Client) Stores() Resource[apiv1.StoreSpec, struct{}] {
	return Resource[apiv1.StoreSpec, struct{}]{c: c, path: "/api/v1/stores"}
}

// SetStoreCredential is PUT /stores/{id}/credential: it seals value as the
// first credential of the remote store id names, etag "*", and returns the
// credential's reference and version and its tag. A store that holds a
// credential is refused.
func (c *Client) SetStoreCredential(ctx context.Context, id string, value []byte, etag string) (apiv1.SecretRef, string, error) {
	rp, err := c.write(ctx, http.MethodPut, "/api/v1/stores/"+url.PathEscape(id)+"/credential", apiv1.Credential{Spec: apiv1.CredentialSpec{Value: value}}, etag, nil)
	if err != nil {
		return apiv1.SecretRef{}, "", err
	}
	var ref apiv1.SecretRef
	return ref, rp.header.Get("ETag"), json.Unmarshal(rp.body, &ref)
}

// DeleteStoreCredential is DELETE /stores/{id}/credential: it removes the
// credential of the remote store id names, whose credential's tag is etag.
func (c *Client) DeleteStoreCredential(ctx context.Context, id, etag string) error {
	_, err := c.write(ctx, http.MethodDelete, "/api/v1/stores/"+url.PathEscape(id)+"/credential", nil, etag, nil)
	return err
}

// Namespaces is the namespaces: List, Create, Get and Delete are GET and
// POST /namespaces and GET and DELETE /namespaces/{id}; no route serves Put
// or Patch.
func (c *Client) Namespaces() Resource[apiv1.NamespaceSpec, struct{}] {
	return Resource[apiv1.NamespaceSpec, struct{}]{c: c, path: "/api/v1/namespaces"}
}

// Adapters is the adapters, each named by its protocol: Get and Put are GET
// and PUT /adapters/{protocol}, a first Put naming the tag "0" its Get
// answered; no route serves the others.
func (c *Client) Adapters() Resource[apiv1.AdapterSpec, apiv1.AdapterStatus] {
	return Resource[apiv1.AdapterSpec, apiv1.AdapterStatus]{c: c, path: "/api/v1/adapters"}
}

// Scope is the scope a setting is written at: the installation, or the
// node, namespace, remote store or share an ID names.
type Scope string

const (
	ScopeInstallation Scope = ""
	ScopeNode         Scope = "nodes"
	ScopeNamespace    Scope = "namespaces"
	ScopeStore        Scope = "stores"
	ScopeShare        Scope = "shares"
)

// Settings is the settings at scope, each named by its key: the
// installation's, id unused, or those of the node, namespace, remote store
// or share id names. Get, Put and Delete are GET, PUT and DELETE of
// .../settings/{key}; no route serves List, Create or Patch.
func (c *Client) Settings(scope Scope, id string) Resource[apiv1.SettingSpec, apiv1.SettingStatus] {
	path := "/api/v1/settings"
	if scope != ScopeInstallation {
		path = "/api/v1/" + string(scope) + "/" + url.PathEscape(id) + "/settings"
	}
	return Resource[apiv1.SettingSpec, apiv1.SettingStatus]{c: c, path: path}
}
