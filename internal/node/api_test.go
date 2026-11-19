package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// admin authenticates every request as an admin.
func admin(*http.Request) (auth.Caller, error) {
	return auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: "admin", Role: entity.RoleAdmin}, nil
}

// getJSON GETs addr's path, wanting 200, and decodes its body into v.
func getJSON(t *testing.T, addr, path string, v any) {
	t.Helper()
	got := get(t, addr, path)
	if got.status != http.StatusOK {
		t.Fatalf("GET %s: answered %d %s, want 200", path, got.status, got.body)
	}
	if err := json.Unmarshal([]byte(got.body), v); err != nil {
		t.Fatalf("GET %s: decode %q: %v", path, got.body, err)
	}
}

// The node serves the management API on its API listener over what it
// holds: its version to anyone; to a caller, its own record from the store's
// node view, the shares from the store's control plane, an operation's
// progress read by the runner's step for its kind, a poll parked on the
// node's clock, and a refusal audited under the node's ID. Stop closes the
// listener.
func TestNodeServesTheAPI(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(admin)))
	sh := addShare(t, h)
	op := entity.Operation{ID: entity.OperationID(uuid.New()), Kind: string(apiv1.OpUserDelete),
		Target: entity.ResourceRef{Kind: entity.ResourceUser, ID: make([]byte, len(entity.PrincipalID{}))}, State: entity.OpPending}
	if _, err := h.Store().ControlPlane().PutOperation(t.Context(), op, 0); err != nil {
		t.Fatalf("create the operation: %v", err)
	}
	started(t, h)
	addr, _ := h.Node().Addrs()
	id := h.NodeID()
	nodeID := entity.ShareID(id).String()
	opPath := "/api/v1/operations/" + uuid.UUID(op.ID).String()

	var v apiv1.Version
	getJSON(t, addr, "/api/version", &v)
	if !slices.Equal(v.Majors, []string{"v1"}) {
		t.Errorf("GET /api/version: majors %v, want [v1]", v.Majors)
	}
	var nodes apiv1.Page[apiv1.Node]
	getJSON(t, addr, "/api/v1/nodes", &nodes)
	if len(nodes.Items) != 1 || nodes.Items[0].ID != nodeID || nodes.Items[0].Status.Address != addr {
		t.Errorf("GET /api/v1/nodes: %+v, want node %s alone, at %s", nodes.Items, nodeID, addr)
	}
	var shares apiv1.Page[apiv1.Share]
	getJSON(t, addr, "/api/v1/shares", &shares)
	if len(shares.Items) != 1 || shares.Items[0].ID != sh.ID.String() {
		t.Errorf("GET /api/v1/shares: %+v, want share %s alone", shares.Items, sh.ID)
	}
	var got apiv1.Operation
	getJSON(t, addr, opPath, &got)
	if want := (apiv1.Progress{Phase: "batches", Total: -1, Unit: "batches"}); got.Progress != want {
		t.Errorf("GET the operation: progress %+v, want %+v, the deletion's step's", got.Progress, want)
	}

	// A poll parks on the node's clock: it does not answer while the clock
	// stands short of its wait, and answers once the clock passes it.
	// Neither call keeps its connection: the leak check that ends the test
	// would otherwise race the transport's goroutines winding down.
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	polled := make(chan error, 1)
	var poll *http.Response
	go func() {
		var err error
		poll, err = c.Get("http://" + addr + opPath + "?wait=2s")
		polled <- err
	}()
	parked := h.Clock().Pending()
	for deadline := time.Now().Add(10 * time.Second); h.Clock().Pending() <= parked; runtime.Gosched() {
		if time.Now().After(deadline) {
			t.Fatal("the poll did not park on the node's clock within 10 s")
		}
	}
	h.Clock().Advance(time.Second)
	select {
	case err := <-polled:
		t.Fatalf("the parked poll answered (%v) with the node's clock a second short of its wait", err)
	case <-time.After(100 * time.Millisecond): // a bounded wait is the assertion: nothing may answer here
	}
	h.Clock().Advance(time.Second)
	select {
	case err := <-polled:
		if err != nil {
			t.Fatalf("GET the operation with ?wait=2s: %v", err)
		}
		_ = poll.Body.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the parked poll: no answer within 10 s of the node's clock passing its wait")
	}
	if poll.StatusCode != http.StatusOK {
		t.Errorf("GET the operation with ?wait=2s: answered %d, want 200 once the wait passed", poll.StatusCode)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+opPath+":cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "cancel-1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST the operation's cancel: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("POST the operation's cancel: answered %d, want 409 not_cancellable", resp.StatusCode)
	}
	var audit apiv1.Page[apiv1.AuditRecord]
	getJSON(t, addr, "/api/v1/audit", &audit)
	refused := slices.IndexFunc(audit.Items, func(r apiv1.AuditRecord) bool {
		return r.Target.Kind == "operation" && r.Target.ID == uuid.UUID(op.ID).String() && strings.Contains(r.Outcome, string(apiv1.CodeNotCancellable))
	})
	if refused < 0 || audit.Items[refused].Node != nodeID {
		t.Errorf("GET /api/v1/audit: %+v, want the cancel of operation %s refused not_cancellable, on node %s", audit.Items, uuid.UUID(op.ID).String(), nodeID)
	}

	if err := h.Node().Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := get(t, addr, "/api/version"); got != closed {
		t.Errorf("GET /api/version after Stop: got %+v, want the listener closed", got)
	}
}

// A node New builds authenticates no caller: its API answers its version and
// refuses every other call 401.
func TestTheAPIAuthenticatesNoCaller(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch())
	started(t, h)
	addr, _ := h.Node().Addrs()
	if got := get(t, addr, "/api/version"); got.status != http.StatusOK {
		t.Errorf("GET /api/version: answered %+v, want 200", got)
	}
	if got := get(t, addr, "/api/v1/nodes"); got.status != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/nodes with no caller: answered %+v, want 401", got)
	}
}

// A Start that failed leaves the API serving, its health naming the failed
// step from the node's health evaluator.
func TestTheAPIHealthNamesAFailedStart(t *testing.T) {
	var f *kvtest.FaultKV
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		f = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = f
		return node.NewAuthenticating(admin)(cfg)
	}))
	sched, err := sim.NewSchedule(1, nil, failOn(kvtest.OpGet, "CFGGEN", 1))
	if err != nil {
		t.Fatalf("fault schedule: %v", err)
	}
	f.SetFault(sched)
	if err := h.Node().Start(t.Context()); err == nil {
		t.Fatal("Start with the counters' read failing: succeeded, want it refused")
	}
	f.SetFault(nil)
	t.Cleanup(func() {
		if err := h.Node().Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	addr, _ := h.Node().Addrs()
	var got apiv1.Health
	getJSON(t, addr, "/api/v1/health", &got)
	named := slices.ContainsFunc(got.Conditions, func(c apiv1.Condition) bool {
		return c.Name == "start_failed" && strings.Contains(c.Cause, "start step apply")
	})
	if got.Ready != "not_ready" || !named {
		t.Errorf("GET /api/v1/health after the failed Start: %+v, want not_ready and start_failed naming step apply", got)
	}
	// A node never ready serves no call but the rows that answer before
	// ready, though its store is open: a write would commit a record nothing
	// applies.
	resp, err := http.Get("http://" + addr + "/api/v1/nodes")
	if err != nil {
		t.Fatalf("GET /api/v1/nodes after the failed Start: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var p apiv1.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("GET /api/v1/nodes after the failed Start: decode the answer: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || p.Code != apiv1.CodeUnavailable || resp.Header.Get("Retry-After") != "" {
		t.Errorf("GET /api/v1/nodes after the failed Start: %d %q Retry-After %q, want %d %q with none", resp.StatusCode, p.Code, resp.Header.Get("Retry-After"), http.StatusServiceUnavailable, apiv1.CodeUnavailable)
	}
}

// caller authenticates every request as an account with role.
func caller(role entity.APIRole) func(*http.Request) (auth.Caller, error) {
	return func(*http.Request) (auth.Caller, error) {
		return auth.Caller{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: role.String(), Role: role}, nil
	}
}

// connectionsPath is the path of the connections of the node id names.
func connectionsPath(id entity.NodeID) string {
	return "/api/v1/nodes/" + entity.ShareID(id).String() + "/connections"
}

// client dials port on loopback as a protocol's client would, closing the
// connection when the test ends.
func client(t *testing.T, port uint16) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial port %d: %v", port, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// clientBelow dials port on loopback as client does, from the highest local
// port below below that is free: a port another socket holds is passed over.
func clientBelow(t *testing.T, port uint16, below int) net.Conn {
	t.Helper()
	for p := below - 1; p > 1023; p-- {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p}, Timeout: 5 * time.Second}
		c, err := d.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, syscall.EADDRNOTAVAIL) {
			continue
		}
		if err != nil {
			t.Fatalf("dial port %d from local port %d: %v", port, p, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	t.Fatalf("dial port %d: no free local port below %d", port, below)
	return nil
}

// named is each connection as its protocol and the client's address.
func named(cs []apiv1.Connection) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Protocol+" "+c.Remote)
	}
	return out
}

// nameOf is the connection client c holds to protocol's adapter, as named
// names it.
func nameOf(protocol string, c net.Conn) string { return protocol + " " + c.LocalAddr().String() }

// listedUntil reads the node's connections at path until done holds of a
// read, for at most 10 s, and returns the last read: a client's connection
// is listed once the adapter has accepted it, and gone once it has torn it
// down, each after the client's own call returns.
func listedUntil(t *testing.T, addr, path string, done func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var p apiv1.Page[apiv1.Connection]
		getJSON(t, addr, path, &p)
		got := named(p.Items)
		if done(got) || time.Now().After(deadline) {
			return got
		}
		runtime.Gosched()
	}
}

// A viewer reads the connections each adapter lists now, from the node: a
// client of NFS and one of SMB, each named by its protocol and the client's
// address, with an empty session and its accept time; once a client closes,
// it is gone from the next read (RFC 23 §4.2, §5.2, RFC 20 §3.4). The
// adapters are enabled while the node serves, as a settings write enables
// them, so the reads run beside the reconcile loop that starts them.
func TestTheConnectionsAreEachAdaptersOpenNow(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleViewer))))
	started(t, h)
	addr, _ := h.Node().Addrs()
	path := connectionsPath(h.NodeID())
	nfsPort, smbPort := freePort(t), freePort(t)
	h.Node().SetNFS(true, nfsAt(nfsPort))
	h.Node().SetSMB(true, smbAt(smbPort))
	configure(t, h, storeA)
	h.Node().Wake()
	wait(t, "both adapters serving", func() bool { return listens(nfsPort) && listens(smbPort) })

	before := time.Now()
	nfs, smb := client(t, nfsPort), client(t, smbPort)
	want := []string{nameOf("nfs", nfs), nameOf("smb", smb)}
	listedUntil(t, addr, path, func(got []string) bool { slices.Sort(got); return slices.Equal(got, want) })
	var p apiv1.Page[apiv1.Connection]
	getJSON(t, addr, path, &p)
	read := time.Now()
	got := named(p.Items)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("connections with a client of each adapter: %q, want %q", got, want)
	}
	for _, c := range p.Items {
		if c.Session != "" || c.Since.Before(before) || c.Since.After(read) {
			t.Errorf("connection %s %s: session %q since %v, want no session, accepted between %v and %v", c.Protocol, c.Remote, c.Session, c.Since, before, read)
		}
	}

	_ = nfs.Close()
	want = []string{nameOf("smb", smb)}
	if got := listedUntil(t, addr, path, func(got []string) bool { return slices.Equal(got, want) }); !slices.Equal(got, want) {
		t.Errorf("connections once the NFS client closed: %q, want %q", got, want)
	}
}

// The connections page by a cursor of the last one's Since, Remote and
// Protocol, the order every page is in: with three clients and two a page,
// the first page holds the two accepted first and a next; with the first
// closed and a fourth accepted before the second read, that read holds the
// third and the fourth, where an offset would skip the third, and none is
// read twice (RFC 23 §4.2). Each client dials from a local port below the
// one before it, so the clients' addresses sort against their accept order
// and a key led by Remote reads them backwards.
func TestTheConnectionsPageByCursor(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleViewer))))
	nfsPort, smbPort := freePort(t), freePort(t)
	h.Node().SetNFS(true, nfsAt(nfsPort))
	h.Node().SetSMB(true, smbAt(smbPort))
	started(t, h)
	addr, _ := h.Node().Addrs()
	path := connectionsPath(h.NodeID())

	// Each client is listed before the next dials, so each is accepted after
	// the one before it.
	var open []string
	below := int(freePort(t)) + 1
	accepted := func(protocol string, port uint16) net.Conn {
		t.Helper()
		c := clientBelow(t, port, below)
		below = c.LocalAddr().(*net.TCPAddr).Port
		open = append(open, nameOf(protocol, c))
		want := slices.Sorted(slices.Values(open))
		if got := listedUntil(t, addr, path, func(got []string) bool { slices.Sort(got); return slices.Equal(got, want) }); !slices.Equal(got, want) {
			t.Fatalf("connections once %s's client was accepted: %q, want %q in any order", protocol, got, want)
		}
		return c
	}
	first := accepted("smb", smbPort)
	second := accepted("nfs", nfsPort)
	third := accepted("smb", smbPort)

	var p apiv1.Page[apiv1.Connection]
	getJSON(t, addr, path+"?limit=2", &p)
	if got, want := named(p.Items), []string{nameOf("smb", first), nameOf("nfs", second)}; !slices.Equal(got, want) || p.Next == "" {
		t.Fatalf("first page of two: %q, next %q; want %q and a next", got, p.Next, want)
	}

	_ = first.Close()
	open = open[1:]
	fourth := accepted("nfs", nfsPort)
	var rest apiv1.Page[apiv1.Connection]
	getJSON(t, addr, path+"?limit=2&after="+url.QueryEscape(p.Next), &rest)
	if got, want := named(rest.Items), []string{nameOf("smb", third), nameOf("nfs", fourth)}; !slices.Equal(got, want) || rest.Next != "" {
		t.Errorf("second page of two, the first client closed and a fourth accepted since the first page: %q, next %q; want %q and no next", got, rest.Next, want)
	}
}

// With no adapter enabled, the connections read is an empty page.
func TestTheConnectionsOfANodeServingNoAdapter(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleViewer))))
	started(t, h)
	addr, _ := h.Node().Addrs()
	if got, want := get(t, addr, connectionsPath(h.NodeID())), (answer{http.StatusOK, `{"items":[]}`}); got != want {
		t.Errorf("connections with no adapter enabled: answered %+v, want %+v", got, want)
	}
}

// notFound reports whether a is 404 not_found.
func notFound(a answer) bool {
	var p apiv1.Problem
	return a.status == http.StatusNotFound && json.Unmarshal([]byte(a.body), &p) == nil && p.Code == "not_found"
}

// The node lists its own connections only: an ID other than its own, a
// node registered in the store as much as one that never registered, is
// answered 404 not_found, as GET /nodes/{id} answers a node that never
// registered (RFC 23 §4.2, §4.7).
func TestTheConnectionsOfAnotherNodeAreNotFound(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleViewer))))
	started(t, h)
	addr, _ := h.Node().Addrs()
	other := entity.Node{ID: entity.NodeID(uuid.New()), Address: "192.0.2.7:12049", Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage}
	if _, err := h.Store().Nodes().RegisterNode(t.Context(), other); err != nil {
		t.Fatalf("register another node: %v", err)
	}
	for _, id := range []entity.NodeID{other.ID, entity.NodeID(uuid.New())} {
		if got := get(t, addr, connectionsPath(id)); !notFound(got) {
			t.Errorf("connections of node %s: answered %+v, want 404 not_found", entity.ShareID(id), got)
		}
	}
}

// An account without the view right, one with no role, is refused the
// connections 404 not_found, as anything it may not read (RFC 23 §5.2).
func TestTheConnectionsNeedTheViewRight(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleNone))))
	started(t, h)
	addr, _ := h.Node().Addrs()
	if got := get(t, addr, connectionsPath(h.NodeID())); !notFound(got) {
		t.Errorf("connections for an account with no role: answered %+v, want 404 not_found", got)
	}
}

// The connections are read from the node, not from the store (RFC 23 §4.2):
// with a client connected and every call of the store refused unavailable, a
// viewer still reads its connection.
func TestTheConnectionsAreReadWhileTheStoreRefuses(t *testing.T) {
	var f *kvtest.FaultKV
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(func(cfg node.Config) (*node.Node, error) {
		f = kvtest.NewFaultKV(cfg.KV)
		cfg.KV = f
		return node.NewAuthenticating(caller(entity.RoleViewer))(cfg)
	}))
	smbPort := freePort(t)
	h.Node().SetSMB(true, smbAt(smbPort))
	started(t, h)
	addr, _ := h.Node().Addrs()
	path := connectionsPath(h.NodeID())
	want := []string{nameOf("smb", client(t, smbPort))}
	if got := listedUntil(t, addr, path, func(got []string) bool { return slices.Equal(got, want) }); !slices.Equal(got, want) {
		t.Fatalf("connections with the store answering: %q, want %q", got, want)
	}

	unreachable := fmt.Errorf("the store is unreachable: %w", vfserr.ErrUnavailable)
	sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: "*", Key: "*", Action: sim.FaultAction{Err: unreachable}})
	if err != nil {
		t.Fatalf("fault schedule: %v", err)
	}
	f.SetFault(sched)
	got := get(t, addr, path)
	f.SetFault(nil)
	var p apiv1.Page[apiv1.Connection]
	if got.status != http.StatusOK || json.Unmarshal([]byte(got.body), &p) != nil || !slices.Equal(named(p.Items), want) {
		t.Errorf("connections with every store call refused: answered %+v, want 200 listing %q", got, want)
	}
}
