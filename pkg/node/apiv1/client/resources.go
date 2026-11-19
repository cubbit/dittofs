package client

import (
	"context"
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

// Users is the users. A delete starts an operation.
func (c *Client) Users() Resource[apiv1.UserSpec, struct{}] {
	return Resource[apiv1.UserSpec, struct{}]{c: c, path: "/api/v1/users"}
}

// Groups is the groups. A delete starts an operation.
func (c *Client) Groups() Resource[apiv1.GroupSpec, struct{}] {
	return Resource[apiv1.GroupSpec, struct{}]{c: c, path: "/api/v1/groups"}
}

// Members is the members of the group whose ID is group, each named by the
// member's ID: put under "*" to add one, deleted to remove it. They are not
// listed.
func (c *Client) Members(group string) Resource[struct{}, struct{}] {
	return Resource[struct{}, struct{}]{c: c, path: "/api/v1/groups/" + url.PathEscape(group) + "/members"}
}

// Grants is the grants on the share whose ID is share, each named by its
// principal's ID: put, read and deleted one at a time, not listed.
func (c *Client) Grants(share string) Resource[apiv1.GrantSpec, struct{}] {
	return Resource[apiv1.GrantSpec, struct{}]{c: c, path: "/api/v1/shares/" + url.PathEscape(share) + "/grants"}
}

// Quotas is the quotas on the share whose ID is share, each named by its
// user's or group's ID: put and read one at a time, not listed. A put of no
// limit deletes it.
func (c *Client) Quotas(share string) Resource[apiv1.QuotaSpec, struct{}] {
	return Resource[apiv1.QuotaSpec, struct{}]{c: c, path: "/api/v1/shares/" + url.PathEscape(share) + "/quotas"}
}

// Netgroups is the netgroups, each named by its name, created by a put
// under "*".
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
