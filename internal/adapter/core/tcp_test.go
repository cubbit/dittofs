//go:build darwin || linux

package core_test

import (
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/testproto"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/resources"
)

// optionsServer is a loopback server the test reads its accepted sockets
// from: each accepted socket arrives through the accepted hook, its TCP
// options set (section 6.7). handshake_timeout is left to the caller, which
// disarms it where its connections must outlive the test's reads.
func optionsServer(t *testing.T, snap testproto.Settings, b *resources.Budgets) (*core.Server, chan net.Conn) {
	t.Helper()
	l := core.Listener{Services: []string{"test"}, Network: "tcp", Protocol: testproto.New(adapter.Deps{Budgets: b}).Bare()}
	srv, err := core.NewServer(adapter.Deps{Budgets: b}, snap, l)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 4)
	core.SetAccepted(srv, func(nc net.Conn) { accepted <- nc })
	stopAtEnd(t, srv)
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return srv, accepted
}

// dialAccepted dials the server's test service and hands back the server's
// end of the connection, captured at accept: the socket whose options the
// kernel holds for this server.
func dialAccepted(t *testing.T, srv *core.Server, accepted chan net.Conn) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", srv.Addrs()[key("test")][0].String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	select {
	case nc := <-accepted:
		return nc
	case <-time.After(2 * time.Second):
		t.Fatal("the dial was not accepted")
		return nil
	}
}

// sockOpt reads one integer socket option of nc, the kernel's answer for it.
func sockOpt(t *testing.T, nc net.Conn, level, opt int) int {
	t.Helper()
	raw, err := nc.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatalf("the accepted socket's raw connection: %v", err)
	}
	var v int
	cerr := raw.Control(func(fd uintptr) {
		v, err = syscall.GetsockoptInt(int(fd), level, opt)
	})
	if cerr == nil {
		cerr = err
	}
	if cerr != nil {
		t.Fatalf("reading option %d at level %d: %v", opt, level, cerr)
	}
	return v
}

// An accepted socket carries RFC 20 section 6.7's options: TCP_NODELAY on,
// which is Go's own default for every TCP connection and this RFC states
// rather than sets, and keepalive on with 15 s idle and 3 probes 5 s apart -
// the idle time also Go's default, the rest not what a listener that set
// nothing would read, Go's own keepalive waiting 15 s between probes and
// sending 9 of them. No buffer size is set: the kernel's autotuning owns the
// window.
func TestTheAcceptedSocketCarriesItsOptions(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := on("127.0.0.1")
	snap.HandshakeTimeout = 0
	srv, accepted := optionsServer(t, snap, b)
	nc := dialAccepted(t, srv, accepted)

	// darwin reads its boolean socket options as the kernel's own flag bits,
	// never normalized to 1: TCP_NODELAY reads 4 for on, SO_KEEPALIVE its own
	// 8. Linux normalizes both to 1. Zero is the only off on each.
	nodelayOn, keepaliveOn := 1, 1
	idleOpt, intvlOpt, cntOpt := 4, 5, 6 // linux's TCP_KEEPIDLE, TCP_KEEPINTVL, TCP_KEEPCNT (include/uapi/linux/tcp.h)
	if runtime.GOOS == "darwin" {
		nodelayOn, keepaliveOn = 4, 8
		// darwin's idle option is its TCP_KEEPALIVE, which Go's syscall
		// package declares there alone; its interval and count options it does
		// not, and Go's own net package writes 0x101 and 0x102 for them.
		idleOpt, intvlOpt, cntOpt = 0x10, 0x101, 0x102
	}
	if nodelay := sockOpt(t, nc, syscall.IPPROTO_TCP, syscall.TCP_NODELAY); nodelay != nodelayOn {
		t.Errorf("the accepted socket's TCP_NODELAY is %d, want Nagle's algorithm off", nodelay)
	}
	if got := sockOpt(t, nc, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE); got != keepaliveOn {
		t.Errorf("the accepted socket's SO_KEEPALIVE is %d, want keepalive on", got)
	}
	for _, o := range []struct {
		name      string
		opt, want int
	}{
		{"keepalive idle time, in seconds", idleOpt, 15},
		{"keepalive interval, in seconds", intvlOpt, 5},
		{"keepalive probe count", cntOpt, 3},
	} {
		if got := sockOpt(t, nc, syscall.IPPROTO_TCP, o.opt); got != o.want {
			t.Errorf("the accepted socket's %s is %d, want %d", o.name, got, o.want)
		}
	}
}
