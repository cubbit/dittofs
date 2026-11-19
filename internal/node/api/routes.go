package api

import (
	"context"
	"net/http"

	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
)

// route is one row of the route table: a method and a pattern of
// net/http.ServeMux, the action a POST on {id}:verb names, the kind of
// resource it reaches, which a refusal before its adapter is audited
// against and only a public row leaves zero, the right its caller must hold,
// whether it answers a caller that must change its password, the kind of
// operation a write row starts on the record it wrote, empty for one that
// starts none, whether it answers before the node is ready, and its adapter.
type route struct {
	method, pattern string
	verb            string
	kind            entity.ResourceKind
	right           auth.Right
	mustChange      bool
	starts          apiv1.OpKind
	beforeReady     bool
	serve           adapter
	// query is the query parameters a write row takes; a write naming any
	// other is refused. A read row's adapter checks its own.
	query []string
}

// op names the row in its metrics and its span: its method and pattern, and
// its verb.
func (rt *route) op() string {
	op := rt.method + " " + rt.pattern
	if rt.verb != "" {
		op += ":" + rt.verb
	}
	return op
}

// routes is the API's route table, which alone drives the mux: a path the
// table does not name is 404, after authentication, so the table is not
// revealed to a caller that has none.
//
// decision: paths take IDs, never names. A path that took a name or an ID
// would make "does this exist?" ambiguous, a share named like another's ID,
// and a rename would move the path under a client holding it; so the {id} of
// every row is parsed as an ID, and a name there is 404. A name is found by a
// listing's filter. Overturned only if a resource ever lacks a stable ID,
// which no record of the metadata store does.
var routes = []route{
	{method: http.MethodGet, pattern: "/api/version", right: auth.Public, beforeReady: true, serve: read(version)},
	{method: http.MethodGet, pattern: "/api/v1/health", kind: entity.ResourceNode, right: auth.View, beforeReady: true, serve: read(healthOf)},
	{method: http.MethodGet, pattern: "/api/v1/operations", kind: entity.ResourceOperation, right: auth.View, serve: list(operations)},
	{method: http.MethodGet, pattern: "/api/v1/operations/{id}", kind: entity.ResourceOperation, right: auth.View, serve: watch(operation)},
	{method: http.MethodPost, pattern: "/api/v1/operations/{id}", verb: "cancel", kind: entity.ResourceOperation, right: auth.View, serve: cancel},
	{method: http.MethodGet, pattern: "/api/v1/nodes", kind: entity.ResourceNode, right: auth.View, serve: list(nodes)},
	{method: http.MethodGet, pattern: "/api/v1/nodes/{id}", kind: entity.ResourceNode, right: auth.View, serve: read(node)},
	{method: http.MethodGet, pattern: "/api/v1/nodes/{id}/connections", kind: entity.ResourceNode, right: auth.View, serve: nodeConnections},
	{method: http.MethodGet, pattern: "/api/v1/audit", kind: entity.ResourceInstallation, right: auth.Audit, serve: list(audit)},
	{method: http.MethodGet, pattern: "/api/v1/shares", kind: entity.ResourceShare, right: auth.View, serve: list(shares)},
	{method: http.MethodPost, pattern: "/api/v1/shares", kind: entity.ResourceShare, right: auth.Shares, serve: write(newShares, nil, createShare)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}", kind: entity.ResourceShare, right: auth.View, serve: watch(share)},
	{method: http.MethodPatch, pattern: "/api/v1/shares/{id}", kind: entity.ResourceShare, right: auth.Shares, serve: write(shareSpecs, nil, patchShare)},
	{method: http.MethodPost, pattern: "/api/v1/shares/{id}", verb: "quiesce", kind: entity.ResourceShare, right: auth.Shares, serve: write(shareTags, nil, setState(entity.ShareEnabled, entity.ShareQuiesced))},
	{method: http.MethodPost, pattern: "/api/v1/shares/{id}", verb: "resume", kind: entity.ResourceShare, right: auth.Shares, serve: write(shareTags, nil, setState(entity.ShareQuiesced, entity.ShareEnabled))},
	{method: http.MethodDelete, pattern: "/api/v1/shares/{id}", kind: entity.ResourceShare, right: auth.Shares, starts: apiv1.OpShareDelete, query: []string{"snapshots"}, serve: write(shareTags, nil, deleteShare)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}/export-policy", kind: entity.ResourceShare, right: auth.View, serve: watch(exportPolicy)},
	{method: http.MethodPut, pattern: "/api/v1/shares/{id}/export-policy", kind: entity.ResourceShare, right: auth.Shares, serve: write(policies, nil, putPolicy)},
	{method: http.MethodPatch, pattern: "/api/v1/shares/{id}/export-policy", kind: entity.ResourceShare, right: auth.Shares, serve: write(policies, nil, putPolicy)},
}

// version answers the API's major versions and nothing else.
func version(context.Context, Deps, request[none]) (apiv1.Version, error) {
	return apiv1.Version{Majors: []string{"v1"}}, nil
}
