package node_test

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/api"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	merrs "github.com/cubbit/dittofs/pkg/metadata/errors"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// The rows of this file run through the API of a node node.New builds, with
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
	name  string
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
// a viewer. before runs on the built node ahead of its Start.
func newWired(t *testing.T, memory bool, build func(node.Config) (*node.Node, error), before func(*nodetest.Harness)) *wired {
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
	if before != nil {
		before(h)
	}
	started(t, h)
	addr, _ := h.Node().Addrs()
	w.authNode = &authNode{h: h, addr: addr}
	w.admin = account{name: "admin", token: w.tokens(t, "admin", adminPassword).Access}
	w.admin.p = w.principalNamed(t, "admin")
	for _, c := range []struct {
		to   *account
		role entity.APIRole
	}{{&w.operator, entity.RoleOperator}, {&w.viewer, entity.RoleViewer}} {
		name := c.role.String()
		p, tk := h.SignedIn(t, addr, name, c.role)
		*c.to = account{name: name, p: p, token: tk.Access}
	}
	return w
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

// failingSet fails each set of a key whose description starts with prefix
// while it is on, as a commit the store cannot reach: vfserr.ErrUnavailable.
type failingSet struct {
	prefix string
	on     atomic.Bool
	seen   atomic.Int32
}

func (f *failingSet) At(op string, key []byte) (time.Duration, int, error) {
	if f.on.Load() && op == kvtest.OpSet && strings.HasPrefix(string(key), f.prefix) {
		f.seen.Add(1)
		return 0, 0, vfserr.ErrUnavailable
	}
	return 0, 0, nil
}

// unreachable fails every read and every transaction while it is on, as
// the KV reports a store it cannot reach: vfserr.ErrUnavailable.
type unreachable struct{ on atomic.Bool }

func (f *unreachable) At(op string, _ []byte) (time.Duration, int, error) {
	if f.on.Load() && op != kvtest.OpClose {
		return 0, 0, vfserr.ErrUnavailable
	}
	return 0, 0, nil
}

// §2.1, API writes only records, its fault half: a share create whose
// commit fails answers 503 and leaves no share and no binding, and none
// appears once the store answers again and the node has reconciled through
// every counter. Catches a handler that binds a share itself.
func TestWiredShareCreateWithItsCommitFaulted(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		configure(t, w.h, storeA)
		ns := namespaceOn(t, w.h, storeA)
		caughtUp(t, w.h)
		shares, bound, audits := w.shareIDs(t), boundShares(w.h), len(w.audits(t))

		// Every set fails, so neither the share nor the create's audit
		// record, applied or refused, can commit.
		f := &failingSet{}
		w.fault.SetFault(f)
		f.on.Store(true)
		body := apiv1.NewShare{Spec: apiv1.ShareSpec{Name: "faulted", Path: "/faulted", Namespace: uuid.UUID(ns).String()}, ExportPolicy: exportPolicy}
		r := w.send(t, http.MethodPost, "/api/v1/shares", w.operator.token, body, "Idempotency-Key", uuid.NewString())
		f.on.Store(false)
		if f.seen.Load() == 0 {
			t.Fatal("the create set no share key: the fault never reached its commit")
		}
		wantStatus(t, "POST /api/v1/shares with its commit faulted", r, http.StatusServiceUnavailable, apiv1.CodeUnavailable)
		if r.header.Get("Retry-After") == "" {
			t.Error("POST /api/v1/shares with its commit faulted: no Retry-After")
		}

		w.h.Node().Wake()
		caughtUp(t, w.h)
		if got := w.shareIDs(t); !slices.Equal(got, shares) {
			t.Errorf("shares listed once the store answers: %v, want %v, none created", got, shares)
		}
		if got := boundShares(w.h); !slices.Equal(got, bound) {
			t.Errorf("shares bound after a full reconcile: %v, want %v, no binding for the faulted create", got, bound)
		}
		if got := len(w.audits(t)); got != audits {
			t.Errorf("%d audit records after the faulted create, want %d: neither the change nor its record", got, audits)
		}
	})
}

// §2.2 order, its API clause: an operation readable before Stop reads
// through the API after reconcile stops, after the adapters stop and as the
// runner's stop is waited on, step 3. Catches an API that stops before the
// drain it reports.
func TestWiredOperationReadableUntilStopStep3(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		var (
			mu    sync.Mutex
			read  = map[string]int{}
			probe func() int
		)
		stepped := func(_ *node.Node, step string) {
			mu.Lock()
			p := probe
			mu.Unlock()
			switch step {
			case "stop reconcile", "stop adapters", "wait runner":
				if p != nil {
					status := p()
					mu.Lock()
					read[step] = status
					mu.Unlock()
				}
			}
		}
		w := newWired(t, memory, node.NewStepping(stepped), nil)
		op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
			Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: make([]byte, len(entity.PrincipalID{}))}, State: entity.OpPending}
		if _, err := w.h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
			t.Fatalf("create the operation: %v", err)
		}
		path := "/api/v1/operations/" + uuid.UUID(op.ID).String()
		if r := w.send(t, http.MethodGet, path, w.viewer.token, nil); r.status != http.StatusOK {
			t.Fatalf("GET the operation before Stop: answered %d %s, want 200", r.status, r.body)
		}
		mu.Lock()
		probe = func() int {
			r, err := w.try(t.Context(), http.MethodGet, path, w.viewer.token, nil)
			if err != nil {
				return 0
			}
			return r.status
		}
		mu.Unlock()
		if err := w.h.Node().Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, step := range []string{"stop reconcile", "stop adapters", "wait runner"} {
			if got := read[step]; got != http.StatusOK {
				t.Errorf("GET the operation at %s: answered %d, want 200 until step 3 ends", step, got)
			}
		}
	})
}

// §2.5 health, its API clause: GET /api/v1/health without a token answers
// 401, while a viewer reads it. Catches grace state or names leaked to an
// anonymous account.
func TestWiredHealthNeedsAToken(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		wantStatus(t, "GET /api/v1/health without a token", w.send(t, http.MethodGet, "/api/v1/health", "", nil), http.StatusUnauthorized, apiv1.CodeUnauthenticated)
		if r := w.send(t, http.MethodGet, "/api/v1/health", w.viewer.token, nil); r.status != http.StatusOK {
			t.Errorf("GET /api/v1/health as a viewer: answered %d %s, want 200", r.status, r.body)
		}
	})
}

// §8.1, the store unreachable, through the API: a read the store cannot
// answer is refused 503 unavailable with Retry-After, and answers once the
// store does. Catches a hung call on a dead store.
func TestWiredStoreUnreachableAnswers503(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		sh := addShare(t, w.h)
		path := "/api/v1/shares/" + sh.ID.String()
		down := &unreachable{}
		w.fault.SetFault(down)
		down.on.Store(true)
		r := w.send(t, http.MethodGet, path, w.viewer.token, nil)
		down.on.Store(false)
		wantStatus(t, "GET a share with the store unreachable", r, http.StatusServiceUnavailable, apiv1.CodeUnavailable)
		if r.header.Get("Retry-After") == "" {
			t.Error("GET a share with the store unreachable: no Retry-After")
		}
		if r := w.send(t, http.MethodGet, path, w.viewer.token, nil); r.status != http.StatusOK {
			t.Errorf("GET the share once the store answers: %d %s, want 200", r.status, r.body)
		}
	})
}

// §8.1, a record the node refuses to build, through the API: a share whose
// namespace names a remote store no setting configures is committed, and its
// status read through the API counts the node refusing it and applying it
// nowhere. Catches a 201 with the failure only in a log.
func TestWiredRefusedShareReadsRefused(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		sh := createShare(t, w.h, "nowhere", storeB)
		path := "/api/v1/shares/" + sh.ID.String()
		// The status writer writes a turn a second; ten turns are well past
		// the one that reports the refusal.
		var got apiv1.Share
		for i := 0; ; i++ {
			r := w.send(t, http.MethodGet, path, w.viewer.token, nil)
			if r.status != http.StatusOK || json.Unmarshal(r.body, &got) != nil {
				t.Fatalf("GET the refused share: answered %d %s, want 200", r.status, r.body)
			}
			if got.Status.Refused > 0 {
				break
			}
			if i == 10 {
				t.Fatalf("the refused share's status after ten turns: %+v, want it refused", got.Status)
			}
			turn(w.h)
		}
		if got.Status.Refused != 1 || got.Status.Applied == nil || got.Status.Applied.Nodes != 0 || got.Status.Applied.Of != 1 {
			t.Errorf("the refused share's status: refused %d, applied %+v; want refused by 1, applied on 0 of 1", got.Status.Refused, got.Status.Applied)
		}
	})
}

// healthBehind reports whether GET /api/v1/health, read as c, names
// config_generation_behind of the settings counter.
func (w *wired) healthBehind(t *testing.T, c account) bool {
	t.Helper()
	r := w.send(t, http.MethodGet, "/api/v1/health", c.token, nil)
	var h apiv1.Health
	if r.status != http.StatusOK || json.Unmarshal(r.body, &h) != nil {
		t.Fatalf("GET /api/v1/health: answered %d %s, want 200", r.status, r.body)
	}
	return slices.ContainsFunc(h.Conditions, func(c apiv1.Condition) bool {
		return c.Name == "config_generation_behind" && c.Subject == "settings"
	})
}

// The condition a node behind raises reaches an admin through
// /api/v1/health: config_generation_behind of the settings counter is
// named once the frozen node's lag passes 5 s, not before, and cleared once
// it catches up. Catches a condition never raised or never cleared.
func TestWiredBehindReadThroughHealth(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		var frozen atomic.Bool
		reached, release := make(chan kvstore.Counters, 1), make(chan struct{})
		w := newWired(t, memory, node.NewPausing(func(c kvstore.Counters) {
			if frozen.Load() {
				reached <- c
				<-release
			}
		}), nil)
		defer func() {
			if frozen.Swap(false) {
				close(release)
			}
		}()
		caughtUp(t, w.h)
		if w.healthBehind(t, w.admin) {
			t.Fatal("config_generation_behind named before any change")
		}

		frozen.Store(true)
		configure(t, w.h, storeA)
		want := counters(t, w.h).Settings
		w.h.Node().Wake()
		if c := awaitRead(t, reached); c.Settings != want {
			t.Fatalf("frozen pass read settings %d, want the change at %d", c.Settings, want)
		}
		w.h.Clock().Advance(5 * time.Second)
		if w.healthBehind(t, w.admin) {
			t.Error("config_generation_behind named at a lag of 5 s, want it only past 5 s")
		}
		w.h.Clock().Advance(time.Second)
		if !w.healthBehind(t, w.admin) {
			t.Error("config_generation_behind not named at a lag of 6 s")
		}

		frozen.Store(false)
		close(release)
		caughtUp(t, w.h)
		if w.healthBehind(t, w.admin) {
			t.Error("config_generation_behind still named once the node caught up")
		}
	})
}

// grantPath is share's grant of the principal id names.
func grantPath(share entity.ShareID, id entity.PrincipalID) string {
	return "/api/v1/shares/" + share.String() + "/grants/" + uuid.UUID(id).String()
}

// §3.5 grants, its API half: a grant of none is stored and read back as a
// grant; a DELETE of one principal's grant removes it alone; a grant naming
// a machine principal is refused 400 and writes nothing. Catches none
// stored as no grant, or a machine granted.
func TestWiredGrants(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		sh := addShare(t, w.h)
		alice, _ := putUser(t, w.h, "alice")
		bob, _ := putUser(t, w.h, "bob")
		put := func(p entity.PrincipalID, access string) authReply {
			return w.send(t, http.MethodPut, grantPath(sh.ID, p), w.operator.token, map[string]any{"spec": apiv1.GrantSpec{Access: access}}, "If-None-Match", "*")
		}
		for _, g := range []struct {
			p      entity.PrincipalID
			access string
		}{{alice.ID, "none"}, {bob.ID, "read"}} {
			if r := put(g.p, g.access); r.status/100 != 2 {
				t.Fatalf("PUT a grant of %s: answered %d %s, want it stored", g.access, r.status, r.body)
			}
		}
		r := w.send(t, http.MethodGet, grantPath(sh.ID, alice.ID), w.viewer.token, nil)
		var g apiv1.Grant
		if r.status != http.StatusOK || json.Unmarshal(r.body, &g) != nil || g.Spec.Access != "none" {
			t.Fatalf("GET alice's grant of none: answered %d %s, want 200 with access none", r.status, r.body)
		}

		r = w.send(t, http.MethodDelete, grantPath(sh.ID, bob.ID), w.operator.token, nil, "If-Match", r.header.Get("ETag"), "Idempotency-Key", uuid.NewString())
		if r.status == http.StatusPreconditionFailed {
			// alice's tag is not bob's: read bob's and delete under it.
			b := w.send(t, http.MethodGet, grantPath(sh.ID, bob.ID), w.viewer.token, nil)
			r = w.send(t, http.MethodDelete, grantPath(sh.ID, bob.ID), w.operator.token, nil, "If-Match", b.header.Get("ETag"), "Idempotency-Key", uuid.NewString())
		}
		if r.status != http.StatusOK {
			t.Fatalf("DELETE bob's grant: answered %d %s, want 200", r.status, r.body)
		}
		if r := w.send(t, http.MethodGet, grantPath(sh.ID, bob.ID), w.viewer.token, nil); r.status != http.StatusNotFound {
			t.Errorf("GET bob's grant after its DELETE: answered %d %s, want 404", r.status, r.body)
		}
		if r := w.send(t, http.MethodGet, grantPath(sh.ID, alice.ID), w.viewer.token, nil); r.status != http.StatusOK {
			t.Errorf("GET alice's grant after bob's DELETE: answered %d %s, want 200, hers kept", r.status, r.body)
		}

		m := entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}
		if _, err := w.h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: m, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host/files.example.com@EXAMPLE.COM"}}}, 0, entity.Alloc{}); err != nil {
			t.Fatalf("put the machine: %v", err)
		}
		r = put(m.ID, "read")
		if p := r.problem(t); r.status != http.StatusBadRequest || p.Code != "invalid_argument" {
			t.Errorf("PUT a machine's grant: answered %d %q, want 400 invalid_argument", r.status, p.Code)
		}
		if _, found, err := w.h.Store().ControlPlane().ShareGrant(t.Context(), sh.ID, m); err != nil || found {
			t.Errorf("the machine's grant after its refused PUT: found %v (%v), want none written", found, err)
		}
	})
}

var _ = errors.New

// roleTable is the least role §5.2 gives each route the router serves,
// written from RFC 23's tables, not read from the router: a route missing
// here, or one here the router does not serve, fails the sweep. A path's
// verb follows its {id}, as the API's OpenAPI document writes it.
var roleTable = map[string]string{
	"GET /api/version": "none", "POST /api/v1/auth/sign-in": "none", "POST /api/v1/auth/refresh": "none",
	// own password, own tokens, sign out: every role, on its own account
	"POST /api/v1/auth/password": "viewer", "POST /api/v1/auth/sign-out": "viewer",
	"GET /api/v1/users/{id}/api-tokens": "viewer", "POST /api/v1/users/{id}/api-tokens": "viewer",
	"DELETE /api/v1/users/{id}/api-tokens/{sub}": "viewer",
	// health detail, operations, node status, a node's open connections,
	// and every resource but accounts, tokens and the audit
	"GET /api/v1/health": "viewer", "GET /api/v1/operations": "viewer", "GET /api/v1/operations/{id}": "viewer",
	"GET /api/v1/nodes": "viewer", "GET /api/v1/nodes/{id}": "viewer", "GET /api/v1/nodes/{id}/connections": "viewer",
	"GET /api/v1/shares": "viewer", "GET /api/v1/shares/{id}": "viewer", "GET /api/v1/shares/{id}/export-policy": "viewer",
	"GET /api/v1/shares/{id}/grants": "viewer", "GET /api/v1/shares/{id}/grants/{sub}": "viewer",
	"GET /api/v1/shares/{id}/quota": "viewer", "GET /api/v1/shares/{id}/quotas": "viewer", "GET /api/v1/shares/{id}/quotas/{sub}": "viewer",
	"GET /api/v1/users": "viewer", "GET /api/v1/users/{id}": "viewer", "GET /api/v1/groups": "viewer", "GET /api/v1/groups/{id}": "viewer",
	"GET /api/v1/netgroups": "viewer", "GET /api/v1/netgroups/{sub}": "viewer", "GET /api/v1/stores/{id}": "viewer",
	"GET /api/v1/namespaces": "viewer", "GET /api/v1/namespaces/{id}": "viewer", "GET /api/v1/adapters/{sub}": "viewer",
	"GET /api/v1/installation":   "viewer",
	"GET /api/v1/settings/{sub}": "viewer", "GET /api/v1/nodes/{id}/settings/{sub}": "viewer", "GET /api/v1/namespaces/{id}/settings/{sub}": "viewer",
	"GET /api/v1/stores/{id}/settings/{sub}": "viewer", "GET /api/v1/shares/{id}/settings/{sub}": "viewer",
	// read the audit
	"GET /api/v1/audit": "admin",
	// shares, grants, export policies, quotas
	"POST /api/v1/shares": "operator", "PATCH /api/v1/shares/{id}": "operator", "DELETE /api/v1/shares/{id}": "operator",
	"POST /api/v1/shares/{id}:quiesce": "operator", "POST /api/v1/shares/{id}:resume": "operator",
	"PUT /api/v1/shares/{id}/export-policy": "operator", "PATCH /api/v1/shares/{id}/export-policy": "operator",
	"PUT /api/v1/shares/{id}/grants/{sub}": "operator", "DELETE /api/v1/shares/{id}/grants/{sub}": "operator",
	"PUT /api/v1/shares/{id}/quota": "operator", "PUT /api/v1/shares/{id}/quotas/{sub}": "operator",
	// remote stores, namespaces, adapters, settings, netgroups, and the
	// secrets they name
	"POST /api/v1/stores": "operator", "DELETE /api/v1/stores/{id}": "operator",
	"PUT /api/v1/stores/{id}/credential": "operator", "DELETE /api/v1/stores/{id}/credential": "operator",
	"POST /api/v1/namespaces": "operator", "DELETE /api/v1/namespaces/{id}": "operator", "PUT /api/v1/adapters/{sub}": "operator",
	"PUT /api/v1/netgroups/{sub}": "operator", "DELETE /api/v1/netgroups/{sub}": "operator",
	"PUT /api/v1/settings/{sub}": "operator", "DELETE /api/v1/settings/{sub}": "operator",
	"PUT /api/v1/nodes/{id}/settings/{sub}": "operator", "DELETE /api/v1/nodes/{id}/settings/{sub}": "operator",
	"PUT /api/v1/namespaces/{id}/settings/{sub}": "operator", "DELETE /api/v1/namespaces/{id}/settings/{sub}": "operator",
	"PUT /api/v1/stores/{id}/settings/{sub}": "operator", "DELETE /api/v1/stores/{id}/settings/{sub}": "operator",
	"PUT /api/v1/shares/{id}/settings/{sub}": "operator", "DELETE /api/v1/shares/{id}/settings/{sub}": "operator",
	// users and groups that hold no role
	"POST /api/v1/users": "operator", "PUT /api/v1/users/{id}": "operator", "PATCH /api/v1/users/{id}": "operator",
	"DELETE /api/v1/users/{id}": "operator", "POST /api/v1/users/{id}:set-password": "operator",
	"POST /api/v1/groups": "operator", "PUT /api/v1/groups/{id}": "operator", "PATCH /api/v1/groups/{id}": "operator",
	"DELETE /api/v1/groups/{id}": "operator", "PUT /api/v1/groups/{id}/members/{sub}": "operator",
	"DELETE /api/v1/groups/{id}/members/{sub}": "operator",
	// the installation's identity: §4.1's actions, and the keytab, which
	// holds the keys every Kerberos sign-in is verified with
	"POST /api/v1/installation:resolve-principal": "operator", "POST /api/v1/installation:test-directory": "admin",
	"POST /api/v1/installation:join-domain": "admin", "POST /api/v1/installation:rotate-machine-password": "admin",
	"PUT /api/v1/installation/keytab": "admin", "DELETE /api/v1/installation/keytab": "admin",
	// cancel takes the role that starts the operation's kind; the sweep's
	// operation is a user's deletion, an operator's
	"POST /api/v1/operations/{id}:cancel": "operator",
}

// actions is §4.1's table of actions: the resource each verb acts on and
// the least role it takes, "by kind" for cancel's.
var actionTable = map[string]struct{ resource, role string }{
	"quiesce": {"share", "operator"}, "resume": {"share", "operator"}, "rehome": {"share", "admin"},
	"accept-copy": {"namespace", "admin"}, "rebuild-index": {"namespace", "operator"},
	"rebuild-reverse-index": {"share", "operator"}, "raise-format": {"installation", "admin"},
	"rotate-signing-key": {"installation", "admin"}, "reload-provisioning": {"node", "admin"},
	"set-password": {"user", "operator"}, "drain": {"node", "operator"}, "decommission": {"node", "admin"},
	"acknowledge-unreadable": {"node", "admin"}, "declare-lost": {"backup location", "admin"},
	"resolve-principal": {"installation", "operator"}, "test-directory": {"installation", "admin"},
	"join-domain": {"installation", "admin"}, "rotate-machine-password": {"installation", "admin"},
	"cancel": {"operation", "by kind"},
}

// resourceOf is the resource a path's collection names, as §4.1's table
// names it.
func resourceOf(path string) string {
	coll, _, _ := strings.Cut(strings.TrimPrefix(path, "/api/v1/"), "/")
	coll, _, _ = strings.Cut(coll, ":")
	return map[string]string{"shares": "share", "namespaces": "namespace", "installation": "installation",
		"nodes": "node", "users": "user", "operations": "operation"}[coll]
}

// servedRoute is a route of the router, as the API's OpenAPI document lists it.
type servedRoute struct {
	method, path string // path as the document writes it, {id} and {sub} unfilled
	role         string // x-dittofs-role
	byKind       bool   // x-dittofs-role-by-kind
}

func (s servedRoute) key() string { return s.method + " " + s.path }

// routeList is every route the router serves, read from the OpenAPI
// document the API generates from its route table.
func routeList(t *testing.T) []servedRoute {
	t.Helper()
	b, err := api.OpenAPI()
	if err != nil {
		t.Fatalf("the API's OpenAPI document: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Role   string `json:"x-dittofs-role"`
			ByKind bool   `json:"x-dittofs-role-by-kind"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode the OpenAPI document: %v", err)
	}
	var out []servedRoute
	for path, ops := range doc.Paths {
		for method, op := range ops {
			out = append(out, servedRoute{strings.ToUpper(method), path, op.Role, op.ByKind})
		}
	}
	slices.SortFunc(out, func(a, b servedRoute) int { return strings.Compare(a.key(), b.key()) })
	return out
}

// The route list read from the router matches RFC 23's tables: every route
// is one §5.2 gives a least role, at that role, and every action is a row of
// §4.1's table, on its resource and at its role. Catches a route added
// without its check, or one whose right drifted from the role table.
func TestRouteListMatchesTheRoleTables(t *testing.T) {
	routes := routeList(t)
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r.key()] = true
		want, ok := roleTable[r.key()]
		switch {
		case !ok:
			t.Errorf("%s: served, but no role table names it", r.key())
		case r.byKind && r.role != "viewer":
			t.Errorf("%s: its right is its operation kind's, its row's role %s, want viewer, the least that reads an operation", r.key(), r.role)
		case !r.byKind && r.role != want:
			t.Errorf("%s: x-dittofs-role %s, want %s", r.key(), r.role, want)
		}
		_, verb, ok := strings.Cut(r.path, ":")
		if !ok {
			continue
		}
		a, ok := actionTable[verb]
		switch {
		case !ok:
			t.Errorf("%s: action %s is not in §4.1's table", r.key(), verb)
		case a.resource != resourceOf(r.path):
			t.Errorf("%s: action %s on %s, want it on %s", r.key(), verb, resourceOf(r.path), a.resource)
		case (a.role == "by kind") != r.byKind || (!r.byKind && a.role != r.role):
			t.Errorf("%s: role %s (by kind %v), want §4.1's %s", r.key(), r.role, r.byKind, a.role)
		}
	}
	for k := range roleTable {
		if !seen[k] {
			t.Errorf("%s: the role table names it, the router does not serve it", k)
		}
	}
}

// rank orders the roles a route's least role names.
var rank = map[string]int{"none": 0, "viewer": 1, "operator": 2, "admin": 3}

// sweepSetting is the setting the sweep writes at every scope.
const sweepSetting = "gc.schedule"

// sweepFixture is the records the sweep's paths name, each existing, so a
// read by a role that holds its right answers 200.
type sweepFixture struct {
	share, namespace, group, op string
	user, member                entity.Principal
}

// fill names a route's {id} and {sub} by the records of f, the caller's own
// account for a route on the caller's own tokens.
func (f sweepFixture) fill(path string, own entity.Principal, node string) string {
	ids := map[string]string{"shares": f.share, "namespaces": f.namespace, "groups": f.group, "operations": f.op,
		"users": uuid.UUID(f.user.ID).String(), "stores": uuid.UUID(storeA).String(), "nodes": node}
	subs := map[string]string{"grants": uuid.UUID(f.member.ID).String(), "quotas": uuid.UUID(f.member.ID).String(),
		"members": uuid.UUID(f.member.ID).String(), "netgroups": "labs", "adapters": "nfs", "settings": sweepSetting,
		"api-tokens": uuid.NewString()}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		switch {
		case strings.HasPrefix(p, "{id}"):
			id := ids[parts[i-1]]
			if strings.HasPrefix(path, "/api/v1/users/{id}/api-tokens") {
				id = uuid.UUID(own.ID).String()
			}
			parts[i] = id + strings.TrimPrefix(p, "{id}")
		case p == "{sub}":
			parts[i] = subs[parts[i-1]]
		}
	}
	return strings.Join(parts, "/")
}

// sweepCall is one call of the sweep and what it answered.
type sweepCall struct {
	who          account
	role         string
	method, path string
	want         string // the route's least role
	reply        authReply
}

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

// seedSweep writes, as setup, the records the sweep's paths name.
func (w *wired) seedSweep(t *testing.T, setup string) sweepFixture {
	t.Helper()
	// The store is configured by its endpoint, and each scope holds a
	// setting, each value JSON as the API writes one, so every read of the
	// sweep names a record that exists.
	node := entity.NodeID(w.h.NodeID())
	for _, st := range []entity.Setting{
		{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: storeA}, Key: "remote.endpoint", Value: []byte(`"https://store.invalid"`)},
		{Scope: entity.SettingScope{Kind: entity.ScopeRemote, ID: storeA}, Key: sweepSetting, Value: []byte(`"daily"`)},
		{Scope: entity.SettingScope{Kind: entity.ScopeInstallation}, Key: sweepSetting, Value: []byte(`"daily"`)},
		{Scope: entity.SettingScope{Kind: entity.ScopeNode, ID: node}, Key: sweepSetting, Value: []byte(`"daily"`)},
	} {
		if _, err := w.h.Store().ControlPlane().PutSetting(t.Context(), st, 0); err != nil {
			t.Fatalf("put setting %s: %v", st.Key, err)
		}
	}
	sh := createShare(t, w.h, "a", storeA)
	for _, sc := range []entity.SettingScope{{Kind: entity.ScopeNamespace, ID: sh.Namespaces[0].Namespace}, {Kind: entity.ScopeShare, ID: sh.ID}} {
		if _, err := w.h.Store().ControlPlane().PutSetting(t.Context(), entity.Setting{Scope: sc, Key: sweepSetting, Value: []byte(`"daily"`)}, 0); err != nil {
			t.Fatalf("put setting %s: %v", sweepSetting, err)
		}
	}
	member, _ := putUser(t, w.h, "carol")
	f := sweepFixture{share: sh.ID.String(), namespace: uuid.UUID(sh.Namespaces[0].Namespace).String(), member: member}
	f.user, _ = putUser(t, w.h, "dave")
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: make([]byte, len(entity.PrincipalID{}))}, State: entity.OpPending}
	if _, err := w.h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the operation: %v", err)
	}
	f.op = uuid.UUID(op.ID).String()
	r := w.send(t, http.MethodPost, "/api/v1/groups", setup, map[string]any{"spec": apiv1.GroupSpec{Name: "staff"}}, "Idempotency-Key", uuid.NewString())
	var g apiv1.Group
	if r.status != http.StatusCreated || json.Unmarshal(r.body, &g) != nil {
		t.Fatalf("create group staff: answered %d %s, want 201", r.status, r.body)
	}
	f.group = g.ID
	m := uuid.UUID(member.ID).String()
	for _, c := range []struct{ path, body string }{
		{"/api/v1/groups/" + f.group + "/members/" + m, `{"spec":{}}`},
		{"/api/v1/netgroups/labs", `{"spec":{"members":["10.0.0.0/8"]}}`},
		{"/api/v1/shares/" + f.share + "/grants/" + m, `{"spec":{"access":"read"}}`},
		{"/api/v1/shares/" + f.share + "/quotas/" + m, `{"spec":{"hard":{"bytes":1,"files":0},"soft":{"bytes":0,"files":0},"grace_seconds":0,"advisory":{"bytes":0,"files":0}}}`},
		{"/api/v1/shares/" + f.share + "/quota", `{"spec":{"hard":{"bytes":0,"files":9},"soft":{"bytes":0,"files":0},"grace_seconds":0,"advisory":{"bytes":0,"files":0}}}`},
	} {
		if r := w.send(t, http.MethodPut, c.path, setup, json.RawMessage(c.body), "If-None-Match", "*"); r.status/100 != 2 {
			t.Fatalf("PUT %s: answered %d %s, want it written", c.path, r.status, r.body)
		}
	}
	return f
}

// §5.2 roles, its sweep, and §6 audit, its refused half: every route the
// router serves, called by a viewer, an operator and an admin with no body
// and no precondition, answers as §5.2 says: a role under the route's least
// is refused, 404 for a read, so a resource it may not read is not
// revealed, and 403 for a write; a role holding it is never refused for its
// role, and reads the existing record 200. Each call writes exactly one
// audit record, under its caller, but a read that answered: a write, and a
// read refused. Catches a route missing its check, and a refusal with no
// record.
func TestWiredRoleSweep(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		_, setup := w.h.SignedIn(t, w.addr, "setup", entity.RoleAdmin)
		f := w.seedSweep(t, setup.Access)
		node := entity.ShareID(w.h.NodeID()).String()
		routes := routeList(t)
		// Signing out ends the caller's session, so it goes last.
		slices.SortStableFunc(routes, func(a, b servedRoute) int {
			return boolInt(a.key() == "POST /api/v1/auth/sign-out") - boolInt(b.key() == "POST /api/v1/auth/sign-out")
		})
		var calls []sweepCall
		for _, who := range []struct {
			a    account
			role string
		}{{w.viewer, "viewer"}, {w.operator, "operator"}, {w.admin, "admin"}} {
			for _, r := range routes {
				want := roleTable[r.key()]
				if want == "none" {
					continue
				}
				path := f.fill(r.path, who.a.p, node)
				// cancel checks the role its operation's kind needs in its
				// handler, after the key every POST carries.
				var hdr []string
				if r.byKind {
					hdr = idemKey()
				}
				calls = append(calls, sweepCall{who.a, who.role, r.method, path, want, w.send(t, r.method, path, who.a.token, nil, hdr...)})
			}
		}
		for _, c := range calls {
			what := c.role + " " + c.method + " " + c.path
			switch {
			case rank[c.role] < rank[c.want] && c.method == http.MethodGet:
				wantStatus(t, what, c.reply, http.StatusNotFound, "not_found")
			case rank[c.role] < rank[c.want]:
				wantStatus(t, what, c.reply, http.StatusForbidden, apiv1.CodeForbidden)
			case c.reply.status == http.StatusForbidden || c.reply.status == http.StatusNotFound && c.method != http.MethodGet:
				t.Errorf("%s: answered %d %s, want the role's right held", what, c.reply.status, c.reply.body)
			case c.method == http.MethodGet && c.reply.status != http.StatusOK:
				t.Errorf("%s: answered %d %s, want 200", what, c.reply.status, c.reply.body)
			}
		}

		reader := w.tokens(t, "admin", adminPassword).Access
		for _, who := range []account{w.viewer, w.operator, w.admin} {
			got := map[string]int{}
			for _, rec := range w.auditsBy(t, reader, who.p) {
				var rq auditedRequest
				if err := json.Unmarshal(rec.Request, &rq); err != nil {
					t.Fatalf("audit record %s: request %s: %v", rec.ID, rec.Request, err)
				}
				got[rq.Method+" "+rq.Path]++
			}
			for _, c := range calls {
				if c.who.p != who.p {
					continue
				}
				want := 1
				if c.method == http.MethodGet && c.reply.status < 400 {
					want = 0
				}
				if n := got[c.method+" "+c.path]; n != want {
					t.Errorf("%s %s %s, answered %d: %d audit records, want %d", c.role, c.method, c.path, c.reply.status, n, want)
				}
			}
		}
		if n := series(t, w.h, "dittofs_api_unmapped_errors_total", ""); n != 0 {
			t.Errorf("dittofs_api_unmapped_errors_total after the sweep: %v, want 0", n)
		}
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// notApplied is each mutating route the applied sweep cannot yet apply on
// this node, and why. Each is still called, and refused, by the role
// sweep, which audits it.
//
// decision: the settings-backed writes (a setting at every scope, an
// adapter, a remote store, a namespace, a store's credential) are refused
// by the node's empty settings table until the settings table is handed to
// the API, so their applied half waits with the rows that write a setting;
// the domain's routes need a directory and a domain controller, which this
// node has none of; cancel has no success answer while no operation kind is
// cancellable, as its route's own marker says; sign-in and refresh are
// public, audited by sign-in's own rows. Withdraw each entry once its route
// can succeed here, which the applied sweep then requires.
var notApplied = map[string]string{
	"PUT /api/v1/settings/{sub}": "settings table", "DELETE /api/v1/settings/{sub}": "settings table",
	"PUT /api/v1/nodes/{id}/settings/{sub}": "settings table", "DELETE /api/v1/nodes/{id}/settings/{sub}": "settings table",
	"PUT /api/v1/namespaces/{id}/settings/{sub}": "settings table", "DELETE /api/v1/namespaces/{id}/settings/{sub}": "settings table",
	"PUT /api/v1/stores/{id}/settings/{sub}": "settings table", "DELETE /api/v1/stores/{id}/settings/{sub}": "settings table",
	"PUT /api/v1/shares/{id}/settings/{sub}": "settings table", "DELETE /api/v1/shares/{id}/settings/{sub}": "settings table",
	"PUT /api/v1/adapters/{sub}": "settings table", "POST /api/v1/stores": "settings table", "DELETE /api/v1/stores/{id}": "settings table",
	"PUT /api/v1/stores/{id}/credential": "settings table", "DELETE /api/v1/stores/{id}/credential": "settings table",
	"POST /api/v1/namespaces": "settings table", "DELETE /api/v1/namespaces/{id}": "settings table",
	"PUT /api/v1/installation/keytab": "domain", "DELETE /api/v1/installation/keytab": "domain",
	"POST /api/v1/installation:resolve-principal": "domain", "POST /api/v1/installation:test-directory": "domain",
	"POST /api/v1/installation:join-domain": "domain", "POST /api/v1/installation:rotate-machine-password": "domain",
	"POST /api/v1/operations/{id}:cancel": "no cancellable kind",
	"POST /api/v1/auth/sign-in":           "public", "POST /api/v1/auth/refresh": "public",
}

// wiredWrite is one write the applied sweep makes: its route, as the role
// table keys it, its path, body and headers, and the secret values its
// body or answer carries, which no audit record may hold.
type wiredWrite struct {
	route, path string
	body        any
	hdr         []string
	secrets     []string
	reply       authReply // what it answered, set once sent
}

// tagIn is the generation the ETag-style value v names, quoted, and false
// for none.
func tagIn(v string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.Trim(v, `"`), 10, 64)
	return n, err == nil && v != ""
}

// generationsHeld fails unless rec's generations are those d's call saw:
// before, the one its If-Match named, or 0 under If-None-Match: *; after,
// the one its answer's ETag names, 0 for a record its DELETE removed, and
// for a DELETE that started an operation, the one before while the record
// stays for the operation to remove, or 0 once it went in the DELETE's own
// write.
func generationsHeld(t *testing.T, d wiredWrite, rec apiv1.AuditRecord) {
	t.Helper()
	method, _, _ := strings.Cut(d.route, " ")
	what := method + " " + d.path
	for i := 0; i+1 < len(d.hdr); i += 2 {
		switch d.hdr[i] {
		case "If-Match":
			if n, ok := tagIn(d.hdr[i+1]); ok && rec.Before != n {
				t.Errorf("%s: audited generation before %d, want %d, the one its If-Match named", what, rec.Before, n)
			}
		case "If-None-Match":
			if rec.Before != 0 {
				t.Errorf("%s: audited generation before %d, want 0 for a record it created", what, rec.Before)
			}
		}
	}
	switch n, ok := tagIn(d.reply.header.Get("ETag")); {
	case ok && rec.After != n:
		t.Errorf("%s: audited generation after %d, want %d, its answer's ETag", what, rec.After, n)
	case !ok && method == http.MethodDelete && d.reply.status == http.StatusOK && rec.After != 0:
		t.Errorf("%s: audited generation after %d, want 0 for the record it removed", what, rec.After)
	case !ok && method == http.MethodDelete && d.reply.status == http.StatusAccepted && rec.After != rec.Before && rec.After != 0:
		t.Errorf("%s: audited generations %d before and %d after, want the record kept as it was or removed by the removal it started", what, rec.Before, rec.After)
	}
	if rec.Before == 0 && rec.After == 0 && d.route != "POST /api/v1/auth/sign-out" && d.route != "POST /api/v1/auth/password" {
		t.Errorf("%s: audited generations 0 before and 0 after, want the record's", what)
	}
}

// idemKey is a fresh Idempotency-Key header pair.
func idemKey() []string { return []string{"Idempotency-Key", uuid.NewString()} }

// §6 audit, its applied half: every mutating route that can succeed on this
// node, applied once through the API, answers 2xx and writes exactly one
// audit record under its caller, applied or naming the operation it
// started, whose generation after is past its generation before, and which
// holds no secret value the call carried. Catches a change with no record,
// and a credential copied into one.
func TestWiredEveryWriteAudited(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		configure(t, w.h, storeA)
		ns := uuid.UUID(namespaceOn(t, w.h, storeA)).String()
		const frankPassword, frankNext, erinPassword = "frank's first password", "frank's second password", "erin's password"
		w.h.Account(t, "frank", frankPassword, entity.RoleViewer)
		frank := account{name: "frank", token: w.tokens(t, "frank", frankPassword).Access}
		frank.p = w.principalNamed(t, "frank")

		// do sends a, wanting 2xx, and returns its answer; the ETag of the
		// record it wrote is the next write's If-Match.
		var done []wiredWrite
		do := func(c account, a wiredWrite) authReply {
			t.Helper()
			method, _, _ := strings.Cut(a.route, " ")
			r := w.send(t, method, a.path, c.token, a.body, a.hdr...)
			if r.status/100 != 2 {
				t.Fatalf("%s %s: answered %d %s, want it applied", method, a.path, r.status, r.body)
			}
			a.reply = r
			// The caller's own bearer token is a secret no record may hold.
			a.secrets = append(a.secrets, c.token)
			done = append(done, a)
			return r
		}
		idOf := func(r authReply) string {
			var v struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(r.body, &v); err != nil || v.ID == "" {
				t.Fatalf("answer %s: no ID (%v)", r.body, err)
			}
			return v.ID
		}
		etag := func(path string) string {
			t.Helper()
			r := w.send(t, http.MethodGet, path, w.admin.token, nil)
			if r.status != http.StatusOK {
				t.Fatalf("GET %s: answered %d %s, want 200", path, r.status, r.body)
			}
			return r.header.Get("ETag")
		}
		match := func(path string, more ...string) []string { return append([]string{"If-Match", etag(path)}, more...) }
		create := []string{"If-None-Match", "*"}
		quota := json.RawMessage(`{"spec":{"hard":{"bytes":1,"files":0},"soft":{"bytes":0,"files":0},"grace_seconds":0,"advisory":{"bytes":0,"files":0}}}`)
		a := w.admin

		r := do(a, wiredWrite{route: "POST /api/v1/shares", path: "/api/v1/shares", hdr: idemKey(), body: apiv1.NewShare{
			Spec: apiv1.ShareSpec{Name: "audited", Path: "/audited", Namespace: ns}, ExportPolicy: exportPolicy}})
		share := "/api/v1/shares/" + idOf(r)
		do(a, wiredWrite{route: "PATCH /api/v1/shares/{id}", path: share, hdr: match(share), body: json.RawMessage(`{"spec":{"case_sensitivity":"sensitive"}}`)})
		do(a, wiredWrite{route: "POST /api/v1/shares/{id}:quiesce", path: share + ":quiesce", hdr: idemKey()})
		do(a, wiredWrite{route: "POST /api/v1/shares/{id}:resume", path: share + ":resume", hdr: idemKey()})
		policy := share + "/export-policy"
		do(a, wiredWrite{route: "PUT /api/v1/shares/{id}/export-policy", path: policy, hdr: match(policy), body: map[string]any{"spec": exportPolicy}})
		do(a, wiredWrite{route: "PATCH /api/v1/shares/{id}/export-policy", path: policy, hdr: match(policy), body: json.RawMessage(`{"spec":{"read_only":true}}`)})

		r = do(a, wiredWrite{route: "POST /api/v1/users", path: "/api/v1/users", hdr: idemKey(), body: map[string]any{"spec": apiv1.UserSpec{Name: "erin"}}})
		erinID := idOf(r)
		erin := "/api/v1/users/" + erinID
		var u apiv1.User
		if err := json.Unmarshal(w.send(t, http.MethodGet, erin, a.token, nil).body, &u); err != nil {
			t.Fatalf("GET erin: %v", err)
		}
		do(a, wiredWrite{route: "PUT /api/v1/users/{id}", path: erin, hdr: match(erin), body: map[string]any{"spec": u.Spec}})
		do(a, wiredWrite{route: "PATCH /api/v1/users/{id}", path: erin, hdr: match(erin), body: json.RawMessage(`{"spec":{"email":"erin@example.com"}}`)})
		do(a, wiredWrite{route: "POST /api/v1/users/{id}:set-password", path: erin + ":set-password", body: apiv1.SetPassword{Password: erinPassword}, secrets: []string{erinPassword}})

		r = do(a, wiredWrite{route: "POST /api/v1/groups", path: "/api/v1/groups", hdr: idemKey(), body: map[string]any{"spec": apiv1.GroupSpec{Name: "ops"}}})
		group := "/api/v1/groups/" + idOf(r)
		var g apiv1.Group
		if err := json.Unmarshal(w.send(t, http.MethodGet, group, a.token, nil).body, &g); err != nil {
			t.Fatalf("GET the group: %v", err)
		}
		g.Spec.Name = "ops2"
		do(a, wiredWrite{route: "PUT /api/v1/groups/{id}", path: group, hdr: match(group), body: map[string]any{"spec": g.Spec}})
		do(a, wiredWrite{route: "PATCH /api/v1/groups/{id}", path: group, hdr: match(group), body: json.RawMessage(`{"spec":{"name":"ops3"}}`)})
		member := group + "/members/" + erinID
		r = do(a, wiredWrite{route: "PUT /api/v1/groups/{id}/members/{sub}", path: member, hdr: create, body: json.RawMessage(`{"spec":{}}`)})
		do(a, wiredWrite{route: "DELETE /api/v1/groups/{id}/members/{sub}", path: member, hdr: append([]string{"If-Match", r.header.Get("ETag")}, idemKey()...)})

		grant := share + "/grants/" + erinID
		r = do(a, wiredWrite{route: "PUT /api/v1/shares/{id}/grants/{sub}", path: grant, hdr: create, body: json.RawMessage(`{"spec":{"access":"read"}}`)})
		do(a, wiredWrite{route: "DELETE /api/v1/shares/{id}/grants/{sub}", path: grant, hdr: append([]string{"If-Match", r.header.Get("ETag")}, idemKey()...)})
		do(a, wiredWrite{route: "PUT /api/v1/shares/{id}/quotas/{sub}", path: share + "/quotas/" + erinID, hdr: create, body: quota})
		do(a, wiredWrite{route: "PUT /api/v1/shares/{id}/quota", path: share + "/quota", hdr: create, body: quota})
		r = do(a, wiredWrite{route: "PUT /api/v1/netgroups/{sub}", path: "/api/v1/netgroups/web", hdr: create, body: json.RawMessage(`{"spec":{"members":["10.0.0.1"]}}`)})
		do(a, wiredWrite{route: "DELETE /api/v1/netgroups/{sub}", path: "/api/v1/netgroups/web", hdr: append([]string{"If-Match", r.header.Get("ETag")}, idemKey()...)})

		tokens := "/api/v1/users/" + uuid.UUID(a.p.ID).String() + "/api-tokens"
		r = do(a, wiredWrite{route: "POST /api/v1/users/{id}/api-tokens", path: tokens, body: apiv1.APITokenSpec{Expires: w.h.Clock().Now().Add(time.Hour)}})
		var tok apiv1.APIToken
		if err := json.Unmarshal(r.body, &tok); err != nil || tok.Status.Token == "" {
			t.Fatalf("the API token's answer %s: %v, want its value", r.body, err)
		}
		done[len(done)-1].secrets = append(done[len(done)-1].secrets, tok.Status.Token)
		do(a, wiredWrite{route: "DELETE /api/v1/users/{id}/api-tokens/{sub}", path: tokens + "/" + tok.ID, hdr: append([]string{"If-Match", `"1"`}, idemKey()...)})

		do(a, wiredWrite{route: "DELETE /api/v1/groups/{id}", path: group, hdr: match(group, idemKey()...)})
		do(a, wiredWrite{route: "DELETE /api/v1/users/{id}", path: erin, hdr: match(erin, idemKey()...)})
		do(a, wiredWrite{route: "DELETE /api/v1/shares/{id}", path: share, hdr: match(share, idemKey()...)})
		do(frank, wiredWrite{route: "POST /api/v1/auth/password", path: "/api/v1/auth/password",
			body: apiv1.PasswordChange{Current: frankPassword, New: frankNext}, secrets: []string{frankPassword, frankNext}})
		frank.token = w.tokens(t, "frank", frankNext).Access
		do(frank, wiredWrite{route: "POST /api/v1/auth/sign-out", path: "/api/v1/auth/sign-out"})

		covered := map[string]bool{}
		for _, d := range done {
			covered[d.route] = true
		}
		for k, least := range roleTable {
			method, _, _ := strings.Cut(k, " ")
			if method == http.MethodGet || least == "none" && notApplied[k] == "" {
				continue
			}
			if _, waits := notApplied[k]; covered[k] == waits {
				t.Errorf("%s: applied %v, listed as waiting %v; want exactly one", k, covered[k], waits)
			}
		}

		reader := w.tokens(t, "admin", adminPassword).Access
		records := append(w.auditsBy(t, reader, a.p), w.auditsBy(t, reader, frank.p)...)
		for _, d := range done {
			method, _, _ := strings.Cut(d.route, " ")
			var mine []apiv1.AuditRecord
			for _, rec := range records {
				var rq auditedRequest
				if json.Unmarshal(rec.Request, &rq) == nil && rq.Method == method && rq.Path == d.path {
					mine = append(mine, rec)
				}
			}
			// A PUT and a PATCH of one path are told apart by their method;
			// two writes of one route and path are each listed once.
			want := 0
			for _, o := range done {
				if o.route == d.route && o.path == d.path {
					want++
				}
			}
			if len(mine) != want {
				t.Errorf("%s %s: %d audit records, want %d", method, d.path, len(mine), want)
				continue
			}
			for _, rec := range mine {
				if rec.Outcome != "applied" && !strings.HasPrefix(rec.Outcome, "operation") && !isUUID(rec.Outcome) {
					t.Errorf("%s %s: outcome %q, want applied or the operation it started", method, d.path, rec.Outcome)
				}
				generationsHeld(t, d, rec)
				raw, _ := json.Marshal(rec)
				for _, s := range d.secrets {
					if strings.Contains(string(raw), s) {
						t.Errorf("%s %s: its audit record holds a secret value the call carried: %s", method, d.path, raw)
					}
				}
			}
		}
	})
}

func isUUID(s string) bool { _, err := uuid.Parse(s); return err == nil }

// §5.2 roles, its secret clause: an operator's write through every
// secret-bearing route it can name the admin by, and through those that
// write a secret of their own, is refused or reaches only its own secret:
// the admin's password and the signing key keep their versions, and the
// admin signs in with its password. Catches a privileged credential
// replaced through a secret write.
func TestWiredOperatorReachesNoAdminCredential(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		configure(t, w.h, storeA)
		in, err := w.h.Store().Installation(t.Context())
		if err != nil {
			t.Fatalf("read the installation: %v", err)
		}
		root := w.stored(t, w.admin.p)
		password, signing := versionOf(t, w.authNode, root.Password), versionOf(t, w.authNode, in.SigningKey)
		admin := "/api/v1/users/" + uuid.UUID(w.admin.p.ID).String()
		op := w.operator.token
		for _, c := range []struct {
			method, path string
			body         any
			hdr          []string
		}{
			{http.MethodPost, admin + ":set-password", apiv1.SetPassword{Password: "taken over"}, nil},
			{http.MethodPatch, admin, json.RawMessage(`{"spec":{"disabled":true}}`), []string{"If-Match", `"1"`}},
			{http.MethodPost, admin + "/api-tokens", apiv1.APITokenSpec{Expires: w.h.Clock().Now().Add(time.Hour)}, nil},
			{http.MethodPut, "/api/v1/installation/keytab", json.RawMessage(`{"spec":{}}`), []string{"If-None-Match", "*"}},
			{http.MethodPut, "/api/v1/stores/" + uuid.UUID(storeA).String() + "/credential", json.RawMessage(`{"spec":{"access_key":"a","secret_key":"taken over"}}`), []string{"If-None-Match", "*"}},
			{http.MethodPost, "/api/v1/auth/password", apiv1.PasswordChange{Current: adminPassword, New: "taken over"}, nil},
		} {
			r := w.send(t, c.method, c.path, op, c.body, c.hdr...)
			if r.status/100 == 2 && c.path != "/api/v1/auth/password" {
				t.Errorf("the operator's %s %s: answered %d %s, want it refused", c.method, c.path, r.status, r.body)
			}
		}
		if got := versionOf(t, w.authNode, w.stored(t, w.admin.p).Password); got != password {
			t.Errorf("the admin's password version after the operator's writes: %d, want %d", got, password)
		}
		if got := versionOf(t, w.authNode, in.SigningKey); got != signing {
			t.Errorf("the signing key's version after the operator's writes: %d, want %d", got, signing)
		}
		w.tokens(t, "admin", adminPassword)
	})
}

// throttled is a node whose API listens where its bootstrap's listen, TLS
// and socket say, holding five viewer accounts, user0 to user4, each with
// its own password.
func throttled(t *testing.T, memory bool, set func(cfg *node.Config)) (*nodetest.Harness, []entity.Principal) {
	t.Helper()
	opts := []nodetest.Option{nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		set(&cfg)
		return node.New(cfg)
	})}
	if memory {
		opts = append(opts, nodetest.InMemory())
	}
	h := nodetest.New(t, opts...)
	var ps []entity.Principal
	for i := range 5 {
		ps = append(ps, h.Account(t, accountName(i), accountName(i)+"'s password", entity.RoleViewer))
	}
	started(t, h)
	return h, ps
}

func accountName(i int) string { return "user" + string(rune('0'+i)) }

// signInVia signs name in with password through c at base, and returns the
// status it answered.
func signInVia(t *testing.T, c *http.Client, base, name, password string) int {
	t.Helper()
	b, _ := json.Marshal(apiv1.SignIn{Name: name, Password: password})
	resp, err := c.Post(base+"/api/v1/auth/sign-in", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("sign %s in at %s: %v", name, base, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// fromAddr is a client whose connections leave from local.
func fromAddr(local net.IP) *http.Client {
	d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: local}}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, DialContext: d.DialContext}}
}

// sprayed fails a sign-in for each of the five accounts, the i-th through
// clients[i%len], each answered 401, and then signs user0 in with its own
// password through clients[0], returning what that sixth attempt answered.
func sprayed(t *testing.T, clients []*http.Client, bases []string) int {
	t.Helper()
	for i := range 5 {
		if got := signInVia(t, clients[i%len(clients)], bases[i%len(bases)], accountName(i), "wrong"); got != http.StatusUnauthorized {
			t.Fatalf("failed sign-in %d, for %s: answered %d, want 401", i+1, accountName(i), got)
		}
	}
	return signInVia(t, clients[0], bases[0], accountName(0), accountName(0)+"'s password")
}

// §5.3 throttling, its loopback and local-socket clauses, through the
// node's own listeners: five failed sign-ins for five accounts from
// 127.0.0.6, from ::1, and from 127.0.0.1 and ::ffff:127.0.0.1 together
// each make the sixth from that source answer 429, a loopback address being
// a source like any other; five for one account through the local socket
// leave the sixth checked, and write no source or pair count. Catches a
// listener that loses the caller's address.
func TestWiredLoopbackSignInsThrottled(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		t.Run("127.0.0.6", func(t *testing.T) {
			if runtime.GOOS != "linux" {
				t.Skip("a connection from 127.0.0.6 needs a host that routes all of 127.0.0.0/8 to loopback, as Linux does; this host is " + runtime.GOOS)
			}
			h, _ := throttled(t, memory, func(*node.Config) {})
			addr, _ := h.Node().Addrs()
			if got := sprayed(t, []*http.Client{fromAddr(net.ParseIP("127.0.0.6"))}, []string{"http://" + addr}); got != http.StatusTooManyRequests {
				t.Errorf("the sixth sign-in from 127.0.0.6: answered %d, want 429", got)
			}
		})
		t.Run("::1", func(t *testing.T) {
			l, err := net.Listen("tcp", "[::1]:0")
			if err != nil {
				t.Skipf("this host has no IPv6 loopback to listen on: %v", err)
			}
			_ = l.Close()
			h, _ := throttled(t, memory, func(cfg *node.Config) { cfg.Bootstrap.API.Listen = []string{"[::1]:0"} })
			addr, _ := h.Node().Addrs()
			if got := sprayed(t, []*http.Client{fromAddr(nil)}, []string{"http://" + addr}); got != http.StatusTooManyRequests {
				t.Errorf("the sixth sign-in from ::1: answered %d, want 429", got)
			}
		})
		t.Run("127.0.0.1 and ::ffff:127.0.0.1", func(t *testing.T) {
			// Both spellings reach one listener only on every interface's
			// address, which the node serves over TLS alone.
			certFile, keyFile, pool := selfSigned(t)
			h, _ := throttled(t, memory, func(cfg *node.Config) {
				cfg.Bootstrap.API.Listen = []string{"[::]:0"}
				cfg.Bootstrap.API.TLS.CertFile, cfg.Bootstrap.API.TLS.KeyFile = certFile, keyFile
			})
			addr, _ := h.Node().Addrs()
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("the API's address %q: %v", addr, err)
			}
			c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{RootCAs: pool}}}
			bases := []string{"https://" + net.JoinHostPort("127.0.0.1", port), "https://" + net.JoinHostPort("::ffff:127.0.0.1", port)}
			if got := sprayed(t, []*http.Client{c}, bases); got != http.StatusTooManyRequests {
				t.Errorf("the sixth sign-in after failures from 127.0.0.1 and ::ffff:127.0.0.1: answered %d, want 429", got)
			}
		})
		t.Run("local socket", func(t *testing.T) {
			// A socket's path is capped near 104 bytes on some hosts, which a
			// subtest's t.TempDir() can pass.
			dir, err := os.MkdirTemp("", "dfs")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			path := filepath.Join(dir, "api.sock")
			h, ps := throttled(t, memory, func(cfg *node.Config) { cfg.Bootstrap.API.Socket = path })
			c := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", path)
				}}}
			for i := range 5 {
				if got := signInVia(t, c, "http://node", accountName(0), "wrong"); got != http.StatusUnauthorized {
					t.Fatalf("failed sign-in %d on the socket: answered %d, want 401", i+1, got)
				}
			}
			if got := signInVia(t, c, "http://node", accountName(0), accountName(0)+"'s password"); got != http.StatusOK {
				t.Errorf("the sixth sign-in on the socket, with the password: answered %d, want 200, checked", got)
			}
			p := ps[0]
			for _, k := range []entity.SignInKey{
				{Source: entity.SourceKey(netip.IPv4Unspecified())},
				{Source: entity.SourceKey(netip.MustParseAddr("127.0.0.1"))},
				{Account: p, Source: entity.SourceKey(netip.IPv4Unspecified())},
				{Account: p, Source: entity.SourceKey(netip.MustParseAddr("127.0.0.1"))},
			} {
				if f, found, err := h.Store().ControlPlane().SignInFailures(t.Context(), k); err != nil || found {
					t.Errorf("sign-in count %+v after failures on the socket: %+v, found %v (%v), want none", k, f, found, err)
				}
			}
		})
	})
}

// storeCodes answers each of the metadata store's error codes, as §4.7's
// table answers them: its status and its code.
var storeCodes = map[merrs.ErrorCode]struct {
	status int
	code   string
}{
	merrs.ErrNotFound: {404, "not_found"}, merrs.ErrAccessDenied: {403, "access_denied"}, merrs.ErrAuthRequired: {403, "auth_required"},
	merrs.ErrPermissionDenied: {403, "permission_denied"}, merrs.ErrAlreadyExists: {409, "already_exists"}, merrs.ErrNotEmpty: {409, "not_empty"},
	merrs.ErrIsDirectory: {400, "is_directory"}, merrs.ErrNotDirectory: {400, "not_directory"}, merrs.ErrInvalidArgument: {400, "invalid_argument"},
	merrs.ErrIOError: {500, "io_error"}, merrs.ErrNoSpace: {409, "no_space"}, merrs.ErrQuotaExceeded: {409, "quota_exceeded"},
	merrs.ErrReadOnly: {409, "read_only"}, merrs.ErrNotSupported: {400, "not_supported"}, merrs.ErrInvalidHandle: {400, "invalid_handle"},
	merrs.ErrStaleHandle: {409, "stale_handle"}, merrs.ErrLocked: {409, "locked"}, merrs.ErrLockNotFound: {409, "lock_not_found"},
	merrs.ErrPrivilegeRequired: {403, "privilege_required"}, merrs.ErrNameTooLong: {400, "name_too_long"}, merrs.ErrDeadlock: {409, "deadlock"},
	merrs.ErrGracePeriod: {503, "grace_period"}, merrs.ErrLockLimitExceeded: {409, "lock_limit_exceeded"}, merrs.ErrLockConflict: {409, "lock_conflict"},
	merrs.ErrConnectionLimitReached: {503, "connection_limit_reached"}, merrs.ErrConflict: {409, "conflict"}, merrs.ErrCrossShare: {400, "cross_share"},
}

// failingReads fails each read of a key naming the ID whose first four
// bytes hex prints, as keys.Describe names an ID, with err while it is set.
type failingReads struct {
	id  string
	mu  sync.Mutex
	err error
}

func (f *failingReads) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *failingReads) At(op string, key []byte) (time.Duration, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	read := op == kvtest.OpGet || op == kvtest.OpScan || op == kvtest.OpBefore
	if f.err != nil && read && strings.Contains(string(key), f.id) {
		return 0, 0, f.err
	}
	return 0, 0, nil
}

// §4.7 error table, through the API of a wired node: each value of the API's
// own set and of the components' sets that a call can provoke here answers
// its status and code; each error of the metadata store's own seam, the
// KV's errors and the store's codes, injected on the store's reads of a
// share, answers its own; and after them all api_unmapped_errors_total is 0.
// Catches an error with no row.
//
// decision: some values are neither provoked nor injected here. The
// filesystem's and the engine's errors (vfserr's file-operation values, the
// entity errors of the file tree) are raised by components this node's API
// reaches through no route; the settings table's and the secret provider's
// refusals wait on the settings table being handed to the API; joined needs
// a joined installation; managed and not_a_copy are raised by no route yet;
// a KV conflict reaches no caller bare, and only contention makes one. Each
// is answered by the problem table, which the API's own test holds total;
// node.Config's per-component fault seam, a row of the plan's deferral
// ledger, is what would reach them here. Withdraw an entry once a route or a
// seam on this node raises it.
func TestWiredErrorTable(t *testing.T) {
	onBackends(t, func(t *testing.T, memory bool) {
		w := newWired(t, memory, nil, nil)
		sh := addShare(t, w.h)
		share := "/api/v1/shares/" + sh.ID.String()
		op, admin, viewer := w.operator.token, w.admin.token, w.viewer.token

		// The provoked rows run in order: each may set up the next.
		r := w.send(t, http.MethodPost, "/api/v1/users", admin, map[string]any{"spec": apiv1.UserSpec{Name: "gina"}}, "Idempotency-Key", "gina")
		if r.status != http.StatusCreated {
			t.Fatalf("create gina: answered %d %s, want 201", r.status, r.body)
		}
		gina, _ := entity.ParsePrincipalID(strings.TrimPrefix(r.header.Get("Location"), "/api/v1/users/"))
		pending := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
			Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: gina[:]}, State: entity.OpRunning}
		if _, err := w.h.Store().ControlPlane().PutOperation(t.Context(), pending, 0); err != nil {
			t.Fatalf("put a running deletion of gina: %v", err)
		}
		machine := entity.Principal{Kind: entity.PrincipalMachine, ID: entity.NewPrincipalID()}
		if _, err := w.h.Store().ControlPlane().PutUser(t.Context(), entity.User{Principal: machine, IDs: entity.ProtocolIDs{{Scheme: entity.SchemeKerberos, Value: "host/m.example.com@EXAMPLE.COM"}}}, 0, entity.Alloc{}); err != nil {
			t.Fatalf("put the machine: %v", err)
		}
		changer := w.h.Account(t, "hank", "hank's password", entity.RoleViewer)
		w.change(t, changer, func(tx entity.ControlPlaneTx, u *entity.User) error {
			u.MustChange = true
			_, err := tx.PutUser(*u, u.Generation, entity.Alloc{})
			return err
		})
		hank := w.tokens(t, "hank", "hank's password").Access
		etag := func(path string) string { return w.send(t, http.MethodGet, path, admin, nil).header.Get("ETag") }
		adminPath := "/api/v1/users/" + uuid.UUID(w.admin.p.ID).String()
		opPath := "/api/v1/operations/" + uuid.UUID(pending.ID).String()

		for _, c := range []struct {
			what   string
			send   func() authReply
			status int
			code   apiv1.Code
		}{
			{"no token", func() authReply { return w.send(t, http.MethodGet, "/api/v1/shares", "", nil) }, 401, apiv1.CodeUnauthenticated},
			{"a viewer's share create", func() authReply {
				return w.send(t, http.MethodPost, "/api/v1/shares", viewer, nil, "Idempotency-Key", uuid.NewString())
			}, 403, apiv1.CodeForbidden},
			{"a share that does not exist", func() authReply { return w.send(t, http.MethodGet, "/api/v1/shares/"+uuid.NewString(), viewer, nil) }, 404, "not_found"},
			{"an unknown field", func() authReply {
				return w.send(t, http.MethodPost, "/api/v1/groups", op, json.RawMessage(`{"bogus":1}`), "Idempotency-Key", uuid.NewString())
			}, 400, "invalid"},
			{"a PATCH with no If-Match", func() authReply {
				return w.send(t, http.MethodPatch, share, op, json.RawMessage(`{"spec":{}}`))
			}, 428, apiv1.CodePreconditionRequired},
			{"a PATCH under a stale If-Match", func() authReply {
				return w.send(t, http.MethodPatch, share, op, json.RawMessage(`{"spec":{}}`), "If-Match", `"999999"`)
			}, 412, apiv1.CodeGenerationMismatch},
			{"a key reused with another body", func() authReply {
				return w.send(t, http.MethodPost, "/api/v1/users", admin, map[string]any{"spec": apiv1.UserSpec{Name: "ivan"}}, "Idempotency-Key", "gina")
			}, 422, apiv1.CodeIdempotencyMismatch},
			{"a cancel", func() authReply {
				return w.send(t, http.MethodPost, opPath+":cancel", admin, nil, "Idempotency-Key", uuid.NewString())
			}, 409, apiv1.CodeNotCancellable},
			{"a delete of a user whose deletion runs", func() authReply {
				p := "/api/v1/users/" + uuid.UUID(gina).String()
				return w.send(t, http.MethodDelete, p, op, nil, "If-Match", etag(p), "Idempotency-Key", uuid.NewString())
			}, 409, apiv1.CodeOperationRunning},
			{"the last admin deleted", func() authReply {
				return w.send(t, http.MethodDelete, adminPath, admin, nil, "If-Match", etag(adminPath), "Idempotency-Key", uuid.NewString())
			}, 409, apiv1.CodeLastAdmin},
			{"a machine granted", func() authReply {
				return w.send(t, http.MethodPut, grantPath(sh.ID, machine.ID), op, json.RawMessage(`{"spec":{"access":"read"}}`), "If-None-Match", "*")
			}, 400, "invalid_argument"},
			{"a call before the password changes", func() authReply { return w.send(t, http.MethodGet, "/api/v1/shares", hank, nil) }, 403, apiv1.CodePasswordChangeRequired},
		} {
			wantStatus(t, c.what, c.send(), c.status, c.code)
		}

		reads := &failingReads{id: hex.EncodeToString(sh.ID[:4])}
		w.fault.SetFault(reads)
		inject := func(what string, err error, status int, code apiv1.Code) {
			t.Helper()
			reads.set(err)
			r := w.send(t, http.MethodGet, share, viewer, nil)
			reads.set(nil)
			wantStatus(t, "GET a share with its read failing "+what, r, status, code)
		}
		for c := merrs.ErrorCode(1); !strings.HasPrefix(c.String(), "Unknown("); c++ {
			want, ok := storeCodes[c]
			if !ok {
				t.Errorf("store code %v: no row in the test's table", c)
				continue
			}
			inject(c.String(), &merrs.StoreError{Code: c, Message: "injected"}, want.status, apiv1.Code(want.code))
		}
		for _, c := range []struct {
			err    error
			status int
			code   apiv1.Code
		}{
			{kv.ErrNotFound, 404, "not_found"}, {kv.ErrTooLarge, 400, "too_large"}, {kv.ErrUncommittedVersion, 500, "uncommitted_version"},
			{kv.ErrClosed, 503, "closed"}, {vfserr.ErrUnavailable, 503, apiv1.CodeUnavailable},
			{context.DeadlineExceeded, 503, apiv1.CodeUnavailable}, {context.Canceled, 503, apiv1.CodeUnavailable},
		} {
			inject(c.err.Error(), c.err, c.status, c.code)
		}
		w.fault.SetFault(nil)

		// Six failed sign-ins from this address for one account throttle the
		// next, so this row runs last.
		w.h.Account(t, "jo", "jo's password", entity.RoleViewer)
		var last authReply
		for range 6 {
			last = w.signIn(t, "jo", "wrong")
		}
		wantStatus(t, "a sign-in past its wait", last, http.StatusTooManyRequests, apiv1.CodeThrottled)

		if n := series(t, w.h, "dittofs_api_unmapped_errors_total", ""); n != 0 {
			t.Errorf("dittofs_api_unmapped_errors_total after the error table: %v, want 0", n)
		}
	})
}
