package apiclient

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
