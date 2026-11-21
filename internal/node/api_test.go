package node_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/api/auth"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/adapter"
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
	// would otherwise race the transport's goroutines winding down. The test
	// waits on the poll's own wait being set on the clock: a wait under a
	// second is one wait of exactly that length. The node's loops arm fixed
	// waits of a second or more, and the engine's computed waits arm their
	// full bound while the clock has not moved, so no other wait set meanwhile
	// can pass for the poll's. Advancing the clock before the poll parks
	// breaks that. The poll
	// runs under the test's context, which ends before the leak check reads,
	// so a test that fails before reading its answer leaves no connection
	// open.
	const wait = 900 * time.Millisecond
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	pollReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+opPath+"?wait="+wait.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	parked := h.Clock().Armed(wait)
	polled := make(chan error, 1)
	var poll *http.Response
	go func() {
		var err error
		poll, err = c.Do(pollReq)
		polled <- err
	}()
	select {
	case <-parked:
	case err := <-polled:
		t.Fatalf("the poll answered (%v) before parking on the node's clock", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the poll did not park on the node's clock within 10 s")
	}
	h.Clock().Advance(wait - time.Millisecond)
	select {
	case err := <-polled:
		t.Fatalf("the parked poll answered (%v) with the node's clock a millisecond short of its wait", err)
	case <-time.After(100 * time.Millisecond): // a bounded wait is the assertion: nothing may answer here
	}
	h.Clock().Advance(time.Millisecond)
	select {
	case err := <-polled:
		if err != nil {
			t.Fatalf("GET the operation with ?wait=%s: %v", wait, err)
		}
		_ = poll.Body.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the parked poll: no answer within 10 s of the node's clock passing its wait")
	}
	if poll.StatusCode != http.StatusOK {
		t.Errorf("GET the operation with ?wait=%s: answered %d, want 200 once the wait passed", wait, poll.StatusCode)
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

// listed is the node's connections at path, each as named names it.
func listed(t *testing.T, addr, path string) []string {
	t.Helper()
	var p apiv1.Page[apiv1.Connection]
	getJSON(t, addr, path, &p)
	return named(p.Items)
}

// exchanged has client c send protocol's adapter a request, an SMB2
// NEGOTIATE or an NFS NULL call, and read its answer, failing the test
// unless one comes within 10 s. A connection the adapter has answered on is
// open, so every read from here lists it (RFC 20 §3.4).
func exchanged(t *testing.T, protocol string, c net.Conn) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var err error
	if protocol == "smb" {
		err = node.Negotiate(c, 1)
	} else {
		_, err = node.Null(c, 1, 3)
	}
	if err != nil {
		t.Fatalf("%s's answer to the client at %s: %v", protocol, c.LocalAddr(), err)
	}
	_ = c.SetDeadline(time.Time{})
}

// hungUp ends client c's side and reads until the adapter closes its own,
// failing the test unless it does within 10 s. A connection whose socket the
// adapter has closed is not open, so no read from here lists it (RFC 20
// §3.4).
func hungUp(t *testing.T, c net.Conn) {
	t.Helper()
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("end the side of the client at %s: %v", c.LocalAddr(), err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("the adapter's close of the client at %s: %v", c.LocalAddr(), err)
	}
	_ = c.Close()
}

// A viewer reads the connections each adapter lists now, from the node: a
// client of NFS and one of SMB, each answered a request, named by its
// protocol and the client's address, with an empty session and its accept
// time; once a client hangs up and its adapter closes its end, it is gone
// from the next read (RFC 23 §4.2, §5.2, RFC 20 §3.4). The adapters are
// enabled while the node serves, as a settings write enables them, so the
// reads run beside the reconcile loop that starts them; the test waits for
// that write's apply, not on a dial, whose connection the next read would
// list.
func TestTheConnectionsAreEachAdaptersOpenNow(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleViewer))))
	started(t, h)
	addr, _ := h.Node().Addrs()
	path := connectionsPath(h.NodeID())
	nfsPort, smbPort := freePort(t), freePort(t)
	h.Node().SetNFS(true, nfsAt(nfsPort))
	h.Node().SetSMB(true, smbAt(smbPort))
	applied(t, h, storeA)

	before := time.Now()
	nfs, smb := client(t, nfsPort), client(t, smbPort)
	exchanged(t, "nfs", nfs)
	exchanged(t, "smb", smb)
	want := []string{nameOf("nfs", nfs), nameOf("smb", smb)}
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

	hungUp(t, nfs)
	want = []string{nameOf("smb", smb)}
	if got := listed(t, addr, path); !slices.Equal(got, want) {
		t.Errorf("connections once the NFS client hung up: %q, want %q", got, want)
	}
}

// The connections page by a cursor of the last one's Since, Remote and
// Protocol, the order every page is in: with three clients and two a page,
// the first page holds the two accepted first and a next; with the first
// hung up and a fourth accepted before the second read, that read holds the
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

	// Each client is answered, and so listed, before the next dials, so each
	// is accepted after the one before it.
	var open []string
	below := int(freePort(t)) + 1
	accepted := func(protocol string, port uint16) net.Conn {
		t.Helper()
		c := clientBelow(t, port, below)
		below = c.LocalAddr().(*net.TCPAddr).Port
		exchanged(t, protocol, c)
		open = append(open, nameOf(protocol, c))
		got, want := listed(t, addr, path), slices.Sorted(slices.Values(open))
		if slices.Sort(got); !slices.Equal(got, want) {
			t.Fatalf("connections once %s's client was answered: %q, want %q in any order", protocol, got, want)
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

	hungUp(t, first)
	open = open[1:]
	fourth := accepted("nfs", nfsPort)
	var rest apiv1.Page[apiv1.Connection]
	getJSON(t, addr, path+"?limit=2&after="+url.QueryEscape(p.Next), &rest)
	if got, want := named(rest.Items), []string{nameOf("smb", third), nameOf("nfs", fourth)}; !slices.Equal(got, want) || rest.Next != "" {
		t.Errorf("second page of two, the first client hung up and a fourth accepted since the first page: %q, next %q; want %q and no next", got, rest.Next, want)
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

// heldConnections is a real adapter with a fault on cue: its Connections
// waits until release closes, as a read waiting on the adapter's lock does.
type heldConnections struct {
	adapter.Adapter
	release <-chan struct{}
}

func (h heldConnections) Connections() iter.Seq[adapter.ConnInfo] {
	<-h.release
	return h.Adapter.Connections()
}

// The node lists its own connections only: an ID other than its own, a
// node registered in the store as much as one that never registered, is
// answered 404 not_found, as GET /nodes/{id} answers a node that never
// registered, before any adapter is read: with SMB serving and its
// Connections held, each is still answered (RFC 23 §4.2, §4.7).
func TestTheConnectionsOfAnotherNodeAreNotFound(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleViewer))))
	release := make(chan struct{})
	h.Node().OnAdapter(func(_ string, a adapter.Adapter) adapter.Adapter { return heldConnections{a, release} })
	h.Node().SetSMB(true, smbAt(freePort(t)))
	started(t, h)
	t.Cleanup(func() { close(release) }) // before Stop, which a read still held would hold
	addr, _ := h.Node().Addrs()
	other := entity.Node{ID: entity.NodeID(uuid.New()), Address: "192.0.2.7:12049", Roles: entity.NodeRoleProtocol | entity.NodeRoleStorage}
	if _, err := h.Store().Nodes().RegisterNode(t.Context(), other); err != nil {
		t.Fatalf("register another node: %v", err)
	}
	// Each read runs under a deadline of its own: a read that waits on the
	// held adapters ends there and is named, rather than ending the test on
	// the client's timeout.
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for _, id := range []entity.NodeID{other.ID, entity.NodeID(uuid.New())} {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+connectionsPath(id), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			cancel()
			t.Errorf("connections of node %s: the API did not answer while the adapters were held (%v), want 404 not_found before any adapter is read", entity.ShareID(id), err)
			continue
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if err != nil {
			t.Fatalf("connections of node %s: read the answer: %v", entity.ShareID(id), err)
		}
		if got := (answer{resp.StatusCode, string(b)}); !notFound(got) {
			t.Errorf("connections of node %s: answered %+v while the adapters were held, want 404 not_found", entity.ShareID(id), got)
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
	c := client(t, smbPort)
	exchanged(t, "smb", c)
	want := []string{nameOf("smb", c)}
	if got := listed(t, addr, path); !slices.Equal(got, want) {
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
