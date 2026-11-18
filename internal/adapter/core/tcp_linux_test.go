//go:build linux

package core_test

import (
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
)

// tcpUserTimeout is TCP_USER_TIMEOUT's socket-option number, 18
// (include/uapi/linux/tcp.h): the kernel takes it in milliseconds, as
// tcp.c's do_tcp_setsockopt does its unit.
const tcpUserTimeout = 18

// An accepted socket's TCP_USER_TIMEOUT is its connection's write_timeout, in
// milliseconds, read from the snapshot the connection loaded at accept: a
// Reconfigure that changes write_timeout moves the next acceptance's, and the
// first keeps its own.
func TestTCPUserTimeoutIsTheConnectionsWriteTimeout(t *testing.T) {
	b := budgets()
	adaptertest.CheckLeaks(t, b)
	snap := on("127.0.0.1")
	snap.HandshakeTimeout = 0
	snap.WriteTimeout = 7500 * time.Millisecond
	srv, accepted := optionsServer(t, snap, b)
	first := dialAccepted(t, srv, accepted)
	if got := sockOpt(t, first, syscall.IPPROTO_TCP, tcpUserTimeout); got != 7500 {
		t.Fatalf("the accepted socket's TCP_USER_TIMEOUT is %d ms, want its connection's write_timeout, 7500", got)
	}

	next := snap
	next.WriteTimeout = 4 * time.Second
	if err := srv.Reconfigure(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	second := dialAccepted(t, srv, accepted)
	if got := sockOpt(t, second, syscall.IPPROTO_TCP, tcpUserTimeout); got != 4000 {
		t.Errorf("after the Reconfigure, an accepted socket's TCP_USER_TIMEOUT is %d ms, want 4000: its connection's own write_timeout", got)
	}
	if got := sockOpt(t, first, syscall.IPPROTO_TCP, tcpUserTimeout); got != 7500 {
		t.Errorf("the first accepted socket's TCP_USER_TIMEOUT moved to %d ms, want its connection's own, 7500", got)
	}
}
