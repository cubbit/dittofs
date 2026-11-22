package client

import (
	"context"
	"encoding/json"
	"fmt"
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

// InstallationAction runs verb on the installation, POST
// /api/v1/installation:verb, with body, nil for none, and returns the
// operation it started. join-domain refuses an Idempotency-Key, so it is
// called with WithoutIdempotencyKey.
func (c *Client) InstallationAction(ctx context.Context, verb string, body any, opts ...CallOption) (*apiv1.Operation, error) {
	rp, err := c.write(ctx, http.MethodPost, "/api/v1/installation:"+verb, body, "", opts)
	if err != nil {
		return nil, err
	}
	op, err := accepted(rp)
	if op == nil && err == nil {
		err = fmt.Errorf("installation:%s answered %d, not an operation", verb, rp.status)
	}
	return op, err
}

// Users is the users. A delete starts an operation, and so does a create,
// put or patch naming a Kerberos name a machine holds, which moves the name
// and starts the machine's deletion. No route serves its Action yet.
func (c *Client) Users() UserResource {
	return UserResource{Resource[apiv1.UserSpec, struct{}]{c: c, path: "/api/v1/users"}}
}

// UserResource is the users, whose create, put or patch may start an
// operation, as moving a machine's Kerberos name starts the machine's
// deletion.
type UserResource struct {
	Resource[apiv1.UserSpec, struct{}]
}

// Create creates a user of spec, returning the operation it started, or the
// user.
func (r UserResource) Create(ctx context.Context, spec apiv1.UserSpec, opts ...CallOption) (Result[apiv1.UserSpec, struct{}], error) {
	return result[apiv1.UserSpec, struct{}](r.c.write(ctx, http.MethodPost, r.path, map[string]any{"spec": spec}, "", opts))
}

// Put replaces the spec of the user id names, whose tag is etag, returning
// the operation it started, or the user.
func (r UserResource) Put(ctx context.Context, id string, spec apiv1.UserSpec, etag string) (Result[apiv1.UserSpec, struct{}], error) {
	return result[apiv1.UserSpec, struct{}](r.c.write(ctx, http.MethodPut, r.at(id), map[string]any{"spec": spec}, etag, nil))
}

// Patch merges patch, a JSON merge patch of the user's body, into the user
// id names, whose tag is etag, returning the operation it started, or the
// user.
func (r UserResource) Patch(ctx context.Context, id string, patch any, etag string) (Result[apiv1.UserSpec, struct{}], error) {
	return result[apiv1.UserSpec, struct{}](r.c.write(ctx, http.MethodPatch, r.at(id), patch, etag, nil))
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
// credential of the remote store id names and returns the credential's
// reference and version and its tag. Until the store's capability check is
// built, the node refuses every set 409.
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
// .../settings/{key}; no route serves List, Create or Patch. A Put or a
// Delete may start an operation, which it returns.
func (c *Client) Settings(scope Scope, id string) SettingResource {
	path := "/api/v1/settings"
	if scope != ScopeInstallation {
		path = "/api/v1/" + string(scope) + "/" + url.PathEscape(id) + "/settings"
	}
	return SettingResource{Resource[apiv1.SettingSpec, apiv1.SettingStatus]{c: c, path: path}}
}

// SettingResource is the settings at one scope, whose write may start an
// operation, as turning identity.ntlm off starts the NT hashes' deletion.
type SettingResource struct {
	Resource[apiv1.SettingSpec, apiv1.SettingStatus]
}

// Put sets the setting key names, whose tag is etag, or, with etag "*",
// creates it, returning the operation it started, or the setting.
func (r SettingResource) Put(ctx context.Context, key string, spec apiv1.SettingSpec, etag string) (Result[apiv1.SettingSpec, apiv1.SettingStatus], error) {
	return result[apiv1.SettingSpec, apiv1.SettingStatus](r.c.write(ctx, http.MethodPut, r.at(key), map[string]any{"spec": spec}, etag, nil))
}
