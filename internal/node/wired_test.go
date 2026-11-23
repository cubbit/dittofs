package node_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
)

// The wired rows run through the API of a node node.New builds, with
// sign-in, the runner, reconcile and the status writer all its own, each
// once over Badger on disk and once over Badger in memory.

// wired is a started node built by node.New over a KV the test can fault,
// and a signed-in account of each role.
type wired struct {
	*authNode
	fault                   *kvtest.FaultKV
	admin, operator, viewer account
}

// account is a signed-in account: its principal and its access token.
type account struct {
	p     entity.Principal
	token string
}

// onBackends runs row once over Badger on disk and once in memory, each in
// a subtest of its own, under one leak check registered at the top.
func onBackends(t *testing.T, row func(t *testing.T, memory bool)) {
	prof.Watch(t, prof.Options{})
	for _, memory := range []bool{false, true} {
		name := "badger"
		if memory {
			name = "memory"
		}
		t.Run(name, func(t *testing.T) { row(t, memory) })
	}
}

// newWired builds the node through build, node.New when nil, over a FaultKV
// around the harness's KV, starts it and signs in an admin, an operator and
// a viewer.
func newWired(t *testing.T, memory bool, build func(node.Config) (*node.Node, error)) *wired {
	t.Helper()
	if build == nil {
		build = node.New
	}
	w := &wired{}
	opts := []nodetest.Option{nodetest.AdminPassword(adminPassword), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		w.fault = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = w.fault
		return build(cfg)
	})}
	if memory {
		opts = append(opts, nodetest.InMemory())
	}
	h := nodetest.New(t, opts...)
	started(t, h)
	addr, _ := h.Node().Addrs()
	w.authNode = &authNode{h: h, addr: addr}
	w.admin = account{token: w.tokens(t, "admin", adminPassword).Access}
	w.admin.p = w.principalNamed(t, "admin")
	for _, c := range []struct {
		to   *account
		role entity.APIRole
	}{{&w.operator, entity.RoleOperator}, {&w.viewer, entity.RoleViewer}} {
		p, tk := h.SignedIn(t, addr, c.role.String(), c.role)
		*c.to = account{p: p, token: tk.Access}
	}
	return w
}

// faulting has the node's KV answer each call rules name as they say, until
// the returned function puts the KV back.
func (w *wired) faulting(t *testing.T, rules ...sim.Rule) (restore func()) {
	t.Helper()
	sched, err := sim.NewSchedule(1, nil, rules...)
	if err != nil {
		t.Fatalf("fault schedule: %v", err)
	}
	w.fault.SetFault(sched)
	return func() { w.fault.SetFault(nil) }
}

// boundShares is every share the engine holds a binding of.
func boundShares(h *nodetest.Harness) []entity.ShareID {
	var out []entity.ShareID
	for _, j := range h.Node().Engine().Stats().Journals {
		for _, id := range j.TagShares {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b entity.ShareID) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(out)
}

// shareIDs is the IDs of the shares GET /api/v1/shares lists.
func (w *wired) shareIDs(t *testing.T) []string {
	t.Helper()
	r := w.send(t, http.MethodGet, "/api/v1/shares", w.viewer.token, nil)
	var page apiv1.Page[apiv1.Share]
	if r.status != http.StatusOK || json.Unmarshal(r.body, &page) != nil {
		t.Fatalf("GET /api/v1/shares: answered %d %s, want the listing", r.status, r.body)
	}
	var ids []string
	for _, s := range page.Items {
		ids = append(ids, s.ID)
	}
	slices.Sort(ids)
	return ids
}

// namespaceOn writes a namespace on store through the store, as the
// installation's namespaces are seeded until the API writes them.
func namespaceOn(t *testing.T, h *nodetest.Harness, store [16]byte) entity.NamespaceID {
	t.Helper()
	ns := entity.Namespace{ID: entity.NewNamespaceID(), Store: store}
	if _, err := h.Store().ControlPlane().PutNamespace(t.Context(), ns, 0); err != nil {
		t.Fatalf("put a namespace: %v", err)
	}
	return ns.ID
}

// exportPolicy is a share's export policy the API accepts.
var exportPolicy = apiv1.ExportPolicySpec{Flavors: []string{"sys"}, Clients: []apiv1.ClientRule{{CIDR: "10.0.0.0/8", Access: "read-write"}}}

// idemKey is a fresh Idempotency-Key header pair.
func idemKey() []string { return []string{"Idempotency-Key", uuid.NewString()} }

// auditsBy is the audit records of p read through GET /api/v1/audit with
// token, every page.
func (w *wired) auditsBy(t *testing.T, token string, p entity.Principal) []apiv1.AuditRecord {
	t.Helper()
	var out []apiv1.AuditRecord
	for after := ""; ; {
		path := "/api/v1/audit?limit=500&actor=" + p.String()
		if after != "" {
			path += "&after=" + after
		}
		r := w.send(t, http.MethodGet, path, token, nil)
		var page apiv1.Page[apiv1.AuditRecord]
		if r.status != http.StatusOK || json.Unmarshal(r.body, &page) != nil {
			t.Fatalf("GET %s: answered %d %s, want the page", path, r.status, r.body)
		}
		out = append(out, page.Items...)
		if page.Next == "" {
			return out
		}
		after = page.Next
	}
}

// auditedRequest is an audit record's request, as the API writes it.
type auditedRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// requestOf is rec's request.
func requestOf(t *testing.T, rec apiv1.AuditRecord) auditedRequest {
	t.Helper()
	var rq auditedRequest
	if err := json.Unmarshal(rec.Request, &rq); err != nil {
		t.Fatalf("audit record %s: request %s: %v", rec.ID, rec.Request, err)
	}
	return rq
}
