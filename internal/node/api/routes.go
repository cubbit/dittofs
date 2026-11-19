package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

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
// starts none, whether a write row carries a credential and so keeps no
// idempotency record, whether it answers before the node is ready, and its
// adapter. A row with a verb and no {id} is an action on the one record its
// path names, served at its pattern followed by :verb.
type route struct {
	method, pattern string
	verb            string
	kind            entity.ResourceKind
	right           auth.Right
	mustChange      bool
	starts          apiv1.OpKind
	keyless         bool
	beforeReady     bool
	serve           adapter
	// query is the query parameters a write row takes; a write naming any
	// other is refused. A read row's adapter checks its own.
	query []string
	// target parses the path's {sub} below the {id} it names, refusing one
	// that names nothing, and returns the ID the row's audit records name
	// its record by; nil, the path has no {sub} and the ID is {id}'s.
	target func(id [16]byte, sub string) ([]byte, error)
	// sub describes what the path's {sub} names, and params the query
	// parameters whose values are narrower than any string, both for the
	// API's OpenAPI document.
	sub    *param
	params []param
}

// ref is the ID the audit records of the row's record id and sub name it
// by, nil when sub does not parse.
func (rt *route) ref(id [16]byte, sub string) []byte {
	if rt.target == nil {
		return id[:]
	}
	b, _ := rt.target(id, sub)
	return b
}

// param describes a query parameter in the API's OpenAPI document: its
// name, its values when they are a closed set, its format, an example of
// it, and what it means.
type param struct {
	name, format, example, desc string
	enum                        []string
}

// What a row's {sub} names, as the API's OpenAPI document says it.
var (
	subPrincipal = &param{format: "uuid", desc: "The principal's ID."}
	subMember    = &param{format: "uuid", desc: "The member's principal ID."}
	subNetgroup  = &param{example: "labs", desc: "The netgroup's name."}
	subProtocol  = &param{enum: protocols, desc: "The adapter's protocol."}
	subSetting   = &param{example: "identity.ntlm", desc: "The setting's key, of 1 to " + strconv.Itoa(maxSettingKey) + " bytes."}
)

// names is the names of vs, the values of a closed set a query parameter
// takes, which its parse and the API's OpenAPI document both read.
func names[E fmt.Stringer](vs []E) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.String()
	}
	return out
}

// valueNamed is the value of vs named s, false when none is.
func valueNamed[E fmt.Stringer](vs []E, s string) (E, bool) {
	for _, v := range vs {
		if v.String() == s {
			return v, true
		}
	}
	var zero E
	return zero, false
}

// hasID reports whether the row's path names a record by {id}.
func (rt *route) hasID() bool { return strings.Contains(rt.pattern, "{id}") }

// creates reports whether the row creates its record: a POST on a
// collection, answered 201 with the record's Location.
func (rt *route) creates() bool { return rt.method == http.MethodPost && !rt.hasID() }

// precondition is what a write row's request must carry: an
// Idempotency-Key; If-Match; or, a PUT that may create its record, If-Match
// or If-None-Match: *.
type precondition struct{ key, match, create bool }

// precondition is the row's: a POST and a DELETE carry an Idempotency-Key,
// but on a keyless row, which refuses one;
// a PATCH, a DELETE and a POST action that changes the spec carry If-Match;
// a PUT carries If-Match or If-None-Match: *, which a record that always
// exists answers 412.
func (rt *route) precondition() precondition {
	m := rt.method
	return precondition{
		key:    (m == http.MethodPost || m == http.MethodDelete) && !rt.keyless,
		match:  m == http.MethodPatch || m == http.MethodDelete || m == http.MethodPost && rt.serve.changesSpec,
		create: m == http.MethodPut,
	}
}

// compares reports whether the write compares its record's tag, and so may
// be refused 412.
func (p precondition) compares() bool { return p.match || p.create }

// op names the row in its metrics and its span: its method and pattern, and
// its verb.
func (rt *route) op() string {
	op := rt.method + " " + rt.pattern
	if rt.verb != "" {
		op += ":" + rt.verb
	}
	return op
}

// muxPattern is the pattern the row is served at: its own, or, for an
// action on a path with no {id}, which the mux cannot split a verb from,
// its pattern followed by :verb.
func (rt *route) muxPattern() string {
	if rt.verb != "" && !rt.hasID() {
		return rt.pattern + ":" + rt.verb
	}
	return rt.pattern
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
var baseRoutes = []route{
	{method: http.MethodGet, pattern: "/api/version", right: auth.Public, beforeReady: true, serve: read(version)},
	{method: http.MethodGet, pattern: "/api/v1/health", kind: entity.ResourceNode, right: auth.View, beforeReady: true, serve: read(healthOf)},
	{method: http.MethodGet, pattern: "/api/v1/operations", kind: entity.ResourceOperation, right: auth.View, serve: list(operations), params: operationFilters},
	{method: http.MethodGet, pattern: "/api/v1/operations/{id}", kind: entity.ResourceOperation, right: auth.View, serve: watch(operation)},
	// decision: :cancel has no success answer while no operation kind
	// defines a cancel: it answers 409 not_cancellable. The right it needs is
	// the one that starts the operation's kind, which its handler checks, so
	// its row asks only View. Overturned once a kind is cancellable.
	{method: http.MethodPost, pattern: "/api/v1/operations/{id}", verb: "cancel", kind: entity.ResourceOperation, right: auth.View, serve: adapter{form: formWrite, roleByKind: true, run: cancel}},
	{method: http.MethodGet, pattern: "/api/v1/nodes", kind: entity.ResourceNode, right: auth.View, serve: list(nodes)},
	{method: http.MethodGet, pattern: "/api/v1/nodes/{id}", kind: entity.ResourceNode, right: auth.View, serve: read(node)},
	{method: http.MethodGet, pattern: "/api/v1/nodes/{id}/connections", kind: entity.ResourceNode, right: auth.View, serve: nodeConnections},
	{method: http.MethodGet, pattern: "/api/v1/audit", kind: entity.ResourceInstallation, right: auth.Audit, serve: list(audit), params: auditFilters},
	{method: http.MethodGet, pattern: "/api/v1/shares", kind: entity.ResourceShare, right: auth.View, serve: list(shares), params: shareFilters},
	{method: http.MethodPost, pattern: "/api/v1/shares", kind: entity.ResourceShare, right: auth.Shares, serve: write(newShares, nil, createShare)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}", kind: entity.ResourceShare, right: auth.View, serve: watch(share)},
	{method: http.MethodPatch, pattern: "/api/v1/shares/{id}", kind: entity.ResourceShare, right: auth.Shares, serve: write(shareSpecs, nil, patchShare)},
	{method: http.MethodPost, pattern: "/api/v1/shares/{id}", verb: "quiesce", kind: entity.ResourceShare, right: auth.Shares, serve: write(shareTags, nil, setState(entity.ShareEnabled, entity.ShareQuiesced))},
	{method: http.MethodPost, pattern: "/api/v1/shares/{id}", verb: "resume", kind: entity.ResourceShare, right: auth.Shares, serve: write(shareTags, nil, setState(entity.ShareQuiesced, entity.ShareEnabled))},
	{method: http.MethodDelete, pattern: "/api/v1/shares/{id}", kind: entity.ResourceShare, right: auth.Shares, starts: apiv1.OpShareDelete, query: []string{"snapshots"}, params: []param{snapshotsParam}, serve: write(shareTags, nil, deleteShare)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}/export-policy", kind: entity.ResourceShare, right: auth.View, serve: watch(exportPolicy)},
	{method: http.MethodPut, pattern: "/api/v1/shares/{id}/export-policy", kind: entity.ResourceShare, right: auth.Shares, serve: write(policies, nil, putPolicy)},
	{method: http.MethodPatch, pattern: "/api/v1/shares/{id}/export-policy", kind: entity.ResourceShare, right: auth.Shares, serve: write(policies, nil, putPolicy)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}/grants", kind: entity.ResourceGrant, right: auth.View, serve: list(grantsList)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}/grants/{sub}", kind: entity.ResourceGrant, right: auth.View, target: pairRef, sub: subPrincipal, serve: watch(grant)},
	{method: http.MethodPut, pattern: "/api/v1/shares/{id}/grants/{sub}", kind: entity.ResourceGrant, right: auth.Shares, target: pairRef, sub: subPrincipal, serve: write(grants, nil, putGrant)},
	{method: http.MethodDelete, pattern: "/api/v1/shares/{id}/grants/{sub}", kind: entity.ResourceGrant, right: auth.Shares, target: pairRef, sub: subPrincipal, serve: write(tagsOf(grants), nil, deleteGrant)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}/quota", kind: entity.ResourceQuota, right: auth.View, serve: watch(quota)},
	{method: http.MethodPut, pattern: "/api/v1/shares/{id}/quota", kind: entity.ResourceQuota, right: auth.Shares, serve: write(quotas, nil, putQuota)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}/quotas", kind: entity.ResourceQuota, right: auth.View, serve: list(quotasList)},
	{method: http.MethodGet, pattern: "/api/v1/shares/{id}/quotas/{sub}", kind: entity.ResourceQuota, right: auth.View, target: pairRef, sub: subPrincipal, serve: watch(quota)},
	{method: http.MethodPut, pattern: "/api/v1/shares/{id}/quotas/{sub}", kind: entity.ResourceQuota, right: auth.Shares, target: pairRef, sub: subPrincipal, serve: write(quotas, nil, putQuota)},
	{method: http.MethodGet, pattern: "/api/v1/users", kind: entity.ResourceUser, right: auth.View, serve: list(usersList), params: []param{{name: "name", desc: "The user of this name: a list of zero or one."}}},
	{method: http.MethodPost, pattern: "/api/v1/users", kind: entity.ResourceUser, right: auth.Principals, serve: write(users, kerberosLink, createUser)},
	{method: http.MethodGet, pattern: "/api/v1/users/{id}", kind: entity.ResourceUser, right: auth.View, serve: watch(user)},
	{method: http.MethodPut, pattern: "/api/v1/users/{id}", kind: entity.ResourceUser, right: auth.Principals, serve: write(users, kerberosLink, putUser)},
	{method: http.MethodPatch, pattern: "/api/v1/users/{id}", kind: entity.ResourceUser, right: auth.Principals, serve: write(users, kerberosLink, putUser)},
	{method: http.MethodDelete, pattern: "/api/v1/users/{id}", kind: entity.ResourceUser, right: auth.Principals, starts: apiv1.OpUserDelete, serve: write(tagsOf(users), nil, deleteUser)},
	{method: http.MethodGet, pattern: "/api/v1/groups", kind: entity.ResourceGroup, right: auth.View, serve: list(groupsList), params: []param{{name: "name", desc: "The group of this name: a list of zero or one."}}},
	{method: http.MethodPost, pattern: "/api/v1/groups", kind: entity.ResourceGroup, right: auth.Principals, serve: write(groups, nil, createGroup)},
	{method: http.MethodGet, pattern: "/api/v1/groups/{id}", kind: entity.ResourceGroup, right: auth.View, serve: watch(group)},
	{method: http.MethodPut, pattern: "/api/v1/groups/{id}", kind: entity.ResourceGroup, right: auth.Principals, serve: write(groups, nil, putGroup)},
	{method: http.MethodPatch, pattern: "/api/v1/groups/{id}", kind: entity.ResourceGroup, right: auth.Principals, serve: write(groups, nil, putGroup)},
	{method: http.MethodDelete, pattern: "/api/v1/groups/{id}", kind: entity.ResourceGroup, right: auth.Principals, starts: apiv1.OpGroupDelete, serve: write(tagsOf(groups), nil, deleteGroup)},
	{method: http.MethodPut, pattern: "/api/v1/groups/{id}/members/{sub}", kind: entity.ResourceMembership, right: auth.Principals, target: pairRef, sub: subMember, serve: write(memberships, nil, addMember)},
	{method: http.MethodDelete, pattern: "/api/v1/groups/{id}/members/{sub}", kind: entity.ResourceMembership, right: auth.Principals, target: pairRef, sub: subMember, serve: write(tagsOf(memberships), nil, removeMember)},
	// decision: a netgroup's path names it by name, the one exception to
	// paths taking IDs: a netgroup has no ID, the store keys it by name and
	// a client rule names it by name. Overturned once a netgroup has an ID.
	{method: http.MethodGet, pattern: "/api/v1/netgroups", kind: entity.ResourceNetgroup, right: auth.View, serve: list(netgroupsList)},
	{method: http.MethodGet, pattern: "/api/v1/netgroups/{sub}", kind: entity.ResourceNetgroup, right: auth.View, target: netgroupRef, sub: subNetgroup, serve: watch(netgroup)},
	{method: http.MethodPut, pattern: "/api/v1/netgroups/{sub}", kind: entity.ResourceNetgroup, right: auth.Stores, target: netgroupRef, sub: subNetgroup, serve: write(netgroups, nil, putNetgroup)},
	{method: http.MethodDelete, pattern: "/api/v1/netgroups/{sub}", kind: entity.ResourceNetgroup, right: auth.Stores, target: netgroupRef, sub: subNetgroup, serve: write(tagsOf(netgroups), nil, deleteNetgroup)},
	{method: http.MethodPost, pattern: "/api/v1/stores", kind: entity.ResourceRemoteStore, right: auth.Stores, serve: write(stores, nil, createStore)},
	{method: http.MethodGet, pattern: "/api/v1/stores/{id}", kind: entity.ResourceRemoteStore, right: auth.View, serve: watch(store)},
	{method: http.MethodDelete, pattern: "/api/v1/stores/{id}", kind: entity.ResourceRemoteStore, right: auth.Stores, serve: write(tagsOf(stores), settingKeys(storeScope), deleteStore)},
	{method: http.MethodPut, pattern: "/api/v1/stores/{id}/credential", kind: entity.ResourceRemoteStore, right: auth.Stores, serve: write(credentials, sealCredential, putCredential)},
	{method: http.MethodDelete, pattern: "/api/v1/stores/{id}/credential", kind: entity.ResourceRemoteStore, right: auth.Stores, serve: write(tagsOf(credentials), nil, deleteCredential)},
	{method: http.MethodGet, pattern: "/api/v1/namespaces", kind: entity.ResourceNamespace, right: auth.View, serve: list(namespaceList)},
	{method: http.MethodPost, pattern: "/api/v1/namespaces", kind: entity.ResourceNamespace, right: auth.Stores, serve: write(namespaces, nil, createNamespace)},
	{method: http.MethodGet, pattern: "/api/v1/namespaces/{id}", kind: entity.ResourceNamespace, right: auth.View, serve: watch(namespace)},
	{method: http.MethodDelete, pattern: "/api/v1/namespaces/{id}", kind: entity.ResourceNamespace, right: auth.Stores, serve: write(tagsOf(namespaces), settingKeys(namespaceScope), deleteNamespace)},
	{method: http.MethodGet, pattern: "/api/v1/adapters/{sub}", kind: entity.ResourceAdapter, right: auth.View, target: adapterRef, sub: subProtocol, serve: watch(adapterRead)},
	{method: http.MethodPut, pattern: "/api/v1/adapters/{sub}", kind: entity.ResourceAdapter, right: auth.Stores, target: adapterRef, sub: subProtocol, serve: write(adapters, adapterFieldsStep, putAdapter)},
	{method: http.MethodGet, pattern: "/api/v1/installation", kind: entity.ResourceInstallation, right: auth.View, serve: read(installationRead)},
	{method: http.MethodPost, pattern: "/api/v1/installation", verb: "resolve-principal", kind: entity.ResourceInstallation, right: auth.Principals, starts: apiv1.OpResolvePrincipal, serve: write(onInstallation[apiv1.ResolvePrincipalParams](), prepareResolve, startOn)},
	{method: http.MethodPost, pattern: "/api/v1/installation", verb: "test-directory", kind: entity.ResourceInstallation, right: auth.Installation, starts: apiv1.OpTestDirectory, serve: write(onInstallation[none](), prepareTestDirectory, startOn)},
	{method: http.MethodPost, pattern: "/api/v1/installation", verb: "join-domain", kind: entity.ResourceInstallation, right: auth.Installation, starts: apiv1.OpJoinDomain, keyless: true, serve: write(onInstallation[apiv1.JoinDomain](), prepareJoin, startOn)},
	{method: http.MethodPost, pattern: "/api/v1/installation", verb: "rotate-machine-password", kind: entity.ResourceInstallation, right: auth.Installation, starts: apiv1.OpRotateMachinePassword, serve: write(onInstallation[none](), prepareRotate, startOn)},
}

// routes is the whole table: the rows above, and the generic setting
// resource's rows at each scope.
var routes = slices.Concat(baseRoutes, settingRoutes())

// version answers the API's major versions and nothing else.
func version(context.Context, Deps, request[none]) (apiv1.Version, error) {
	return apiv1.Version{Majors: []string{"v1"}}, nil
}
