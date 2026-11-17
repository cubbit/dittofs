package core_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/adapter/settings"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// newDeps is a server's dependencies, with the four budgets the leak check
// reads.
func newDeps() adapter.Deps {
	return adapter.Deps{VFS: vfstest.Null{}, Budgets: &resources.Budgets{
		Receive:    resources.NewWaitBudget("receive", 8<<20),
		Reply:      resources.NewWaitBudget("reply", 8<<20),
		Connection: resources.NewBudget("connection", 1<<20),
		Hold:       resources.NewBudget("hold", 1<<20),
	}}
}

// on is testproto's settings, binding each of addrs.
func on(addrs ...string) testproto.Settings {
	s := testproto.Settings{Adapter: settings.DefaultNFS().Adapter}
	for _, a := range addrs {
		s.Addrs = append(s.Addrs, netip.MustParseAddr(a))
	}
	return s
}

// lcListen is the real socket opener, the one the hooks below step around.
var lcListen = (&net.ListenConfig{}).Listen

// emptyStages is a protocol whose NewConn builds no stages: a wiring fault,
// the one NewServer refuses at configured, never at the first request.
type emptyStages struct{ core.Protocol }

func (emptyStages) NewConn(*core.Conn) core.Stages { return core.Stages{} }

// tcp is a listener of p at port carrying services.
func tcp(p core.Protocol, port uint16, services ...string) core.Listener {
	return core.Listener{Services: services, Network: "tcp", Port: port, Protocol: p}
}

// key is the Socket service is reported under on tcp.
func key(service string) adapter.Socket { return adapter.Socket{Service: service, Network: "tcp"} }

// stopAtEnd stops srv at the test's end, before the leak check reads what
// is still open, so a test that fails early leaves no socket behind.
func stopAtEnd(t *testing.T, srv *core.Server) {
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
}

// needIPv6Loopback skips t where ::1 cannot be bound.
func needIPv6Loopback(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("::1 cannot be bound here: %v", err)
	}
	_ = ln.Close()
}

// dialOK fails t unless a dial to a connects: a socket listens there. Nothing
// accepts, so the connection completes in the socket's backlog.
func dialOK(t *testing.T, a netip.AddrPort) {
	t.Helper()
	c, err := net.Dial("tcp", a.String())
	if err != nil {
		t.Errorf("dial %s: %v; want it to connect, a socket bound there", a, err)
		return
	}
	_ = c.Close()
}

// dialRefused fails t unless a dial to a is refused: nothing listens there.
func dialRefused(t *testing.T, a netip.AddrPort) {
	t.Helper()
	c, err := net.Dial("tcp", a.String())
	if err == nil {
		_ = c.Close()
		t.Fatalf("a dial to %s connects; want it refused", a)
	}
}

// NewServer refuses no listener, each malformed set of listeners, naming the
// listener, and a listener on udp as unsupported; a valid set, two listeners
// at port 0 among them, is a configured server bound nowhere.
func TestNewServerRefusesMalformedListeners(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)
	set := on("127.0.0.1")

	for _, tc := range []struct {
		name        string
		snap        settings.Settings
		ls          []core.Listener
		want        string // what the refusal names
		unsupported bool   // wraps errors.ErrUnsupported
	}{
		{"no settings", nil, nil, "core: new server: no settings", false},
		{"a typed-nil settings (nfs)", (*settings.NFS)(nil), nil, "core: new server: no settings", false},
		{"a typed-nil settings (smb)", (*settings.SMB)(nil), nil, "core: new server: no settings", false},
		{"no listener", set, nil, "no listener", false},
		{"no service", set, []core.Listener{tcp(p, 0)}, "names no service", false},
		{"an empty service", set, []core.Listener{tcp(p, 0, "a", "")}, "names an empty service", false},
		{"a service twice", set, []core.Listener{tcp(p, 0, "a", "b", "a")}, "names a twice", false},
		{"no protocol", set, []core.Listener{tcp(p, 0, "b"), {Services: []string{"c"}, Network: "tcp"}}, "names no protocol", false},
		{"a typed-nil protocol", set, []core.Listener{tcp((*testproto.Protocol)(nil), 0, "a")}, "names no protocol", false},
		{"stages accept would refuse", set, []core.Listener{tcp(p, 0, "b", "c"), {Services: []string{"d"}, Network: "tcp", Protocol: emptyStages{p}}}, "stages without Header, Router or Protocol", false},
		{"two listeners on one Socket", set, []core.Listener{tcp(p, 0, "a"), tcp(p, 0, "b", "a")}, "listener 1", false},
		{"two listeners on one port", set, []core.Listener{tcp(p, 2049, "a"), tcp(p, 2049, "b")}, "listener 1", false},
		{"a udp listener", set, []core.Listener{tcp(p, 0, "a"), {Services: []string{"b"}, Network: "udp", Protocol: p}}, "listener 1", true},
	} {
		srv, err := core.NewServer(deps, tc.snap, tc.ls...)
		if err == nil || srv != nil {
			t.Errorf("%s: NewServer returned %v, %v; want it refused", tc.name, srv, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: NewServer: %v; want the refusal to name %s", tc.name, err, tc.want)
		}
		if got := errors.Is(err, errors.ErrUnsupported); got != tc.unsupported {
			t.Errorf("%s: NewServer: %v; wraps errors.ErrUnsupported %v, want %v", tc.name, err, got, tc.unsupported)
		}
	}

	srv, err := core.NewServer(deps, set, tcp(p, 0, "a", "b"), tcp(p, 0, "c"), tcp(p, 2049, "d"))
	if err != nil {
		t.Fatalf("NewServer with a valid set: %v", err)
	}
	stopAtEnd(t, srv)
	if got := srv.Health().State; got != adapter.StateConfigured {
		t.Errorf("a new server is %s, want configured", got)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("a new server's Addrs %v, want empty: nothing is bound before Start", got)
	}
}

// Start binds every listener on each address, on its family alone: two
// listeners on 127.0.0.1 and ::1 are four sockets, each service reported
// under its Socket at each address with the port bound there.
func TestStartBindsEachListenerOnEachAddress(t *testing.T) {
	needIPv6Loopback(t)
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1", "::1"), tcp(p, 0, "a", "b"), tcp(p, 0, "c"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := srv.Addrs()
	if len(got) != 3 || got[key("a")] == nil || got[key("b")] == nil || got[key("c")] == nil {
		t.Fatalf("Addrs %v, want a, b and c over tcp", got)
	}
	sockets := map[netip.AddrPort]bool{}
	for k, as := range got {
		if len(as) != 2 || as[0].Addr() != netip.MustParseAddr("127.0.0.1") || as[1].Addr() != netip.IPv6Loopback() {
			t.Errorf("Addrs[%v] %v, want 127.0.0.1 and ::1, in the settings' order", k, as)
			continue
		}
		for _, a := range as {
			if a.Port() == 0 {
				t.Errorf("Addrs[%v] has %v, want the port bound there", k, a)
			}
			sockets[a] = true
		}
	}
	if !slices.Equal(got[key("a")], got[key("b")]) {
		t.Errorf("Addrs: a at %v, b at %v; want one listener's services at its sockets", got[key("a")], got[key("b")])
	}
	if len(sockets) != 4 {
		t.Errorf("Addrs reports %d sockets %v, want 4: two listeners on two addresses", len(sockets), sockets)
	}
	for a := range sockets {
		dialOK(t, a)
	}
}

// A wildcard is bound on its own family: 0.0.0.0 is reported as itself, at
// the port bound, and that port answers no IPv6 dial.
func TestStartBindsAWildcardOnItsFamilyAlone(t *testing.T) {
	needIPv6Loopback(t)
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("0.0.0.0"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := srv.Addrs()[key("a")]
	if len(got) != 1 || got[0].Addr() != netip.IPv4Unspecified() || got[0].Port() == 0 {
		t.Fatalf("Addrs[a] %v, want 0.0.0.0 at the port bound", got)
	}
	port := got[0].Port()
	dialOK(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port))
	dialRefused(t, netip.AddrPortFrom(netip.IPv6Loopback(), port))
}

// An IPv6 wildcard is bound on its own family too: its port answers no IPv4
// dial, the widening direction of the tcp4/tcp6 decision: on tcp, Go binds
// :: as one dual-stack socket nobody configured for.
func TestStartBindsASixWildcardOnItsFamilyAlone(t *testing.T) {
	needIPv6Loopback(t)
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("::"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start on ::: %v", err)
	}

	got := srv.Addrs()[key("a")]
	if len(got) != 1 || got[0].Addr() != netip.MustParseAddr("::") || got[0].Port() == 0 {
		t.Fatalf("Addrs[a] %v, want :: at the port bound", got)
	}
	port := got[0].Port()
	dialOK(t, netip.AddrPortFrom(netip.IPv6Loopback(), port))
	dialRefused(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port))
}

// An IPv4-mapped address binds the IPv4 address it maps and is reported as
// the settings name it, at the port bound.
func TestStartBindsAMappedAddressAsIPv4(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("::ffff:127.0.0.1"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start on ::ffff:127.0.0.1: %v", err)
	}

	got := srv.Addrs()[key("a")]
	if len(got) != 1 || got[0].Addr() != netip.MustParseAddr("::ffff:127.0.0.1") || got[0].Port() == 0 {
		t.Fatalf("Addrs[a] %v, want ::ffff:127.0.0.1 as the settings name it, at the port bound", got)
	}
	dialOK(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), got[0].Port()))
}

// With no address, Start binds nothing and serves: the server is ready, and
// Addrs is empty.
func TestStartWithNoAddressServesBoundNowhere(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on(), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start with no address: %v, want it to serve", err)
	}
	if h := srv.Health(); h.State != adapter.StateServing || !h.Ready() {
		t.Errorf("Health %+v with no address, want serving and ready", h)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("Addrs %v with no address, want empty", got)
	}
}

// NewServer keeps its own copy of the listeners: what the caller changes in
// its slice afterwards, a listener replaced or a service renamed in place,
// reaches neither the check nor Addrs.
func TestNewServerKeepsItsOwnListeners(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	ls := []core.Listener{tcp(p, 0, "a"), tcp(p, 0, "b")}
	srv, err := core.NewServer(deps, on("127.0.0.1"), ls...)
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	ls[0].Services[0] = "b" // now two listeners name b over tcp, which NewServer refuses
	ls[1] = tcp(p, 0, "c")
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := srv.Addrs()
	if len(got) != 2 || len(got[key("a")]) != 1 || len(got[key("b")]) != 1 {
		t.Errorf("Addrs %v after the caller changed its listeners, want a and b at one address each, as NewServer checked them", got)
	}
}

// A bind that fails fails Start, naming the listener and the address, and
// closes what Start opened: the server is configured and bound nowhere. The
// configured port is free on the wire - the refusal is injected on its first
// bind alone, so a repick, which the configured-port gate owes to refuse,
// would instead bind the port for real and the server would serve, which the
// test sees as its own failure. The "a" socket the good listener opened
// stands shut at the end: its address is dialed and refused. It runs in
// parallel, with no leak watch - the watch is process-wide and runs beside no
// parallel test - so the hook it sets being this server's alone is shown, and
// its sockets' closure is pinned from outside, by the dials.
func TestStartThatFailsClosesWhatItOpened(t *testing.T) {
	t.Parallel()
	deps := newDeps()
	p := testproto.New(deps)

	// A free port, found by binding one and letting go, for "b" to name.
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()
	configured := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port)

	var refused bool          // the injected refusal happened
	var opened netip.AddrPort // the last socket bound on 127.0.0.1: "a"'s, that Start must close again
	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, 0, "a"), tcp(p, port, "b"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	core.SetListen(srv, func(ctx context.Context, network, addr string) (net.Listener, error) {
		if a, perr := netip.ParseAddrPort(addr); perr == nil && a == configured && !refused {
			// The runtime's in-use refusal, on demand: nothing holds the
			// port, and a second bind meets the free port and binds it,
			// where the configured-port gate owes the repick refused.
			refused = true
			return nil, fmt.Errorf("%w: %s is spoken for", syscall.EADDRINUSE, configured)
		}
		ln, lerr := lcListen(ctx, network, addr)
		if lerr == nil && network == "tcp4" {
			opened = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ln.Addr().(*net.TCPAddr).AddrPort().Port())
		}
		return ln, lerr
	})

	err = srv.Start(t.Context())
	if err == nil {
		t.Fatal("Start bound a port whose refusal was injected")
	}
	if !refused {
		t.Fatal("Start failed without meeting the injected refusal: the test no longer reaches it")
	}
	if !strings.Contains(err.Error(), `["b"]`) || !strings.Contains(err.Error(), configured.String()) {
		t.Errorf("Start: %v; want it to name the listener [b] and the address %s", err, configured)
	}
	if got := srv.Health().State; got != adapter.StateConfigured {
		t.Errorf("after a failed Start the server is %s, want configured", got)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("after a failed Start Addrs is %v, want empty: every socket it opened was closed by the failure", got)
	}
	dialRefused(t, opened)
}

// A port-0 listener whose pick a later family holds repicks: IPv4 and IPv6
// ports are allocated apart, so the port 127.0.0.1's socket picked can be
// held on ::1 by another socket, and the whole group binds again with a
// fresh pick, Start reporting one port. The hook holds the pick out before
// delegating, so the refusal it meets is the runtime's own. It runs in
// parallel, with no leak watch - the watch is process-wide and runs beside no
// parallel test - so the hook it sets being this server's alone is shown, and
// its sockets' closure is pinned from outside, by the dials.
func TestAPortZeroStartRepicksWhenALaterFamilyHoldsItsPick(t *testing.T) {
	t.Parallel()
	needIPv6Loopback(t)
	deps := newDeps()
	p := testproto.New(deps)

	var held net.Listener
	srv, err := core.NewServer(deps, on("127.0.0.1", "::1"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	var once bool
	core.SetListen(srv, func(ctx context.Context, network, addr string) (net.Listener, error) {
		// The first ::1 bind at the picked port finds it held: by this
		// listener, so the real call below answers with the runtime's in-use
		// refusal. The hold stays put to the test's end, where the old
		// pick's socket is shown closed; a repick asks a fresh port, which no
		// listener of this test holds.
		if ap, perr := netip.ParseAddrPort(addr); perr == nil && !once && network == "tcp6" && ap.Port() != 0 {
			if ln, err := net.Listen("tcp6", addr); err == nil {
				once = true
				held = ln
				t.Cleanup(func() { _ = ln.Close() })
			}
		}
		return lcListen(ctx, network, addr)
	})
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v; want it to repick when ::1 holds the port 127.0.0.1 picked", err)
	}
	if held == nil {
		t.Fatal("Start bound ::1 without the hook holding its pick first: the test no longer reaches the repick")
	}
	addrs := srv.Addrs()[key("a")]
	if len(addrs) != 2 || addrs[0].Port() != addrs[1].Port() {
		t.Fatalf("Addrs[a] %v, want both addresses at the one repicked port", addrs)
	}
	if addrs[1].Port() == held.Addr().(*net.TCPAddr).AddrPort().Port() {
		t.Errorf("Addrs[a] reports the port ::1 held, %d", addrs[1].Port())
	}
	// The abandoned pick's socket is closed behind the repick: the group it
	// opened, 127.0.0.1 at the held port, refuses a dial.
	dialRefused(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), held.Addr().(*net.TCPAddr).AddrPort().Port()))
}

// A configured port held on one of its two addresses fails Start at the
// other, and the sockets the group opened before the refusal - the same
// port's on 127.0.0.1 - are closed with it, so nothing stays bound at the
// half that worked. Nothing is injected: the hold is a real socket, and the
// refusal it meets the runtime's own.
func TestAConfiguredPortHeldOnOneAddressClosesWhatItsGroupOpened(t *testing.T) {
	needIPv6Loopback(t)
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	held, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	at := held.Addr().(*net.TCPAddr).AddrPort()
	srv, err := core.NewServer(deps, on("127.0.0.1", "::1"), tcp(p, at.Port(), "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err == nil {
		t.Fatal("Start bound a port ::1 holds")
	} else if !strings.Contains(err.Error(), `["a"]`) || !strings.Contains(err.Error(), at.String()) {
		t.Errorf("Start: %v; want it to name the listener [a] and the address %s", err, at)
	}
	if got := srv.Health().State; got != adapter.StateConfigured {
		t.Errorf("after a failed Start the server is %s, want configured", got)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("after a failed Start Addrs is %v, want empty: the group's opened sockets were closed by the failure", got)
	}
	// The group's first socket, the port held on ::1 bound on 127.0.0.1: it
	// listened there until its own group's failure closed it.
	dialRefused(t, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), at.Port()))
}

// lieAboutAddr wraps a listener that reports a non-TCP address: an opener
// answering with one is a bug in the opener's seat, and the bind owes the
// socket closed and the group refused, not a port read off an address that
// lies about what listens.
type lieAboutAddr struct{ net.Listener }

func (lieAboutAddr) Addr() net.Addr { return &net.UnixAddr{Name: "under test", Net: "unix"} }

// A bind whose listener reports no TCP address is refused, and the listener
// it opened is closed on the refusal: nothing stays bound at a port an
// address like that can never name.
func TestABindWhoseListenerReportsNoTCPAddressIsRefused(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	var at netip.AddrPort // the wrapped listener's real address, dialable until the refusal closes it
	core.SetListen(srv, func(ctx context.Context, network, addr string) (net.Listener, error) {
		ln, lerr := lcListen(ctx, network, addr)
		if lerr != nil {
			return nil, lerr
		}
		if network == "tcp4" {
			at = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ln.Addr().(*net.TCPAddr).AddrPort().Port())
		}
		return lieAboutAddr{Listener: ln}, nil
	})

	if err := srv.Start(t.Context()); err == nil {
		t.Fatal("Start accepted a listener that reports no TCP address")
	} else if !strings.Contains(err.Error(), "not a TCP listener") {
		t.Errorf("Start: %v; want it to refuse the listener that answers no TCP address", err)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("Addrs after the refused Start %v, want empty", got)
	}
	dialRefused(t, at)
}

// The settings' addresses are the server's own copy: changing the caller's
// settings after NewServer, the slice included, leaves what Start binds and
// Addrs reports as they were given.
func TestTheServerKeepsItsOwnAddresses(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)
	one := settings.DefaultNFS()
	one.Addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	srv, err := core.NewServer(deps, one, tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)

	// Two changes to the caller's settings, one in place and one by append:
	// neither reaches what Start binds.
	one.Addrs[0] = netip.IPv6Loopback()
	one.Addrs = append(one.Addrs, netip.MustParseAddr("127.0.0.2"))

	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	k := key("a")
	addrs := srv.Addrs()
	if len(addrs[k]) != 1 || addrs[k][0].Addr() != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("Start binds %v, want the one address it was given", addrs[k])
	}
}

// A listener at port 0 is one port: the system picks it on the first address
// it binds and every other address uses the same one, so a service reports no
// port of its own per address.
func TestAPortZeroListenerIsOnePort(t *testing.T) {
	needIPv6Loopback(t)
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)
	srv, err := core.NewServer(deps, on("127.0.0.1", "::1"), tcp(p, 0, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	addrs := srv.Addrs()
	seen := map[uint16]bool{}
	for _, k := range []adapter.Socket{key("a"), key("b")} {
		for _, a := range addrs[k] {
			seen[a.Port()] = true
		}
	}
	if len(seen) != 1 {
		t.Errorf("a port-0 listener reports the ports %v, want one", seen)
	}
}

// A Server is what NewServer builds: a zero value refuses to start by error,
// not by panic.
func TestAZeroServerRefusesToStart(t *testing.T) {
	var srv core.Server
	if err := srv.Start(t.Context()); err == nil {
		t.Fatal("a zero Server started")
	} else if !strings.Contains(err.Error(), "not built by NewServer") {
		t.Errorf("a zero Server: %v", err)
	}
}

// Addrs hands out a fresh map each call, slices included: what a caller does
// to one leaves the next as it was.
func TestAddrsIsTheCallersOwn(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, 0, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first := srv.Addrs()
	want := make(map[adapter.Socket][]netip.AddrPort)
	for k, as := range first {
		want[k] = slices.Clone(as)
	}
	first[key("a")][0] = netip.AddrPort{}
	first[key("a")] = append(first[key("a")], netip.MustParseAddrPort("192.0.2.1:1"))
	delete(first, key("b"))
	first[key("x")] = nil

	if got := srv.Addrs(); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("Addrs after the caller changed the last one's map %v, want %v as before", got, want)
	}
}

// Stop closes every socket and moves to stopped; a second Stop returns nil,
// and a stopped server is never started again. Start on a serving server is
// refused too, naming the state, and the server is ready only while serving.
func TestStopTwiceAndStartAfterStop(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if h := srv.Health(); h.Ready() {
		t.Errorf("a configured server's Health %+v is ready, want not", h)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h := srv.Health(); h.State != adapter.StateServing || !h.Ready() {
		t.Errorf("a started server's Health %+v, want serving and ready", h)
	}
	if err := srv.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "serving") {
		t.Errorf("Start on a serving server: %v; want it refused naming serving", err)
	}
	at := srv.Addrs()[key("a")]
	if len(at) != 1 {
		t.Fatalf("Addrs[a] %v, want one address", at)
	}

	if err := srv.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if h := srv.Health(); h.State != adapter.StateStopped || h.Ready() {
		t.Errorf("a stopped server's Health %+v, want stopped and not ready", h)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("a stopped server's Addrs %v, want empty", got)
	}
	dialRefused(t, at[0])

	if err := srv.Stop(t.Context()); err != nil {
		t.Errorf("a second Stop: %v, want nil", err)
	}
	if err := srv.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Start after Stop: %v; want it refused naming stopped", err)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("Addrs after a refused Start %v, want empty", got)
	}
}

// Stop on a configured server moves it to stopped, with nothing to drain, and
// it is never started.
func TestStopWhileConfigured(t *testing.T) {
	deps := newDeps()
	adaptertest.CheckLeaks(t, deps.Budgets)
	p := testproto.New(deps)

	srv, err := core.NewServer(deps, on("127.0.0.1"), tcp(p, 0, "a"))
	if err != nil {
		t.Fatal(err)
	}
	stopAtEnd(t, srv)
	if err := srv.Stop(t.Context()); err != nil {
		t.Fatalf("Stop on a configured server: %v", err)
	}
	if got := srv.Health().State; got != adapter.StateStopped {
		t.Errorf("after Stop the server is %s, want stopped", got)
	}
	if err := srv.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Start after Stop on a configured server: %v; want it refused naming stopped", err)
	}
	if got := srv.Addrs(); len(got) != 0 {
		t.Errorf("Addrs after a refused Start %v, want empty", got)
	}
}
