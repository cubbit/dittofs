//go:build linux

package core

import (
	"net"
	"syscall"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT's socket-option number, 18: Go 1.26's
// syscall declares the constant on linux/arm64 but not on linux/amd64
// (zerrors_linux_*.go), and its value is the kernel's own, spelled in
// include/uapi/linux/tcp.h; golang.org/x/sys, which has it everywhere, is
// outside RFC 20 section 2.3's import list.
const tcpUserTimeout = 18

// userTimeout sets TCP_USER_TIMEOUT on an accepted socket, timeout in
// milliseconds, through its raw connection: unacknowledged data the kernel
// would otherwise keep retransmitting to its retransmission limit, about 15
// minutes, ends the connection at timeout instead, closing one a vanished
// peer left a lease break on, which keepalive alone cannot reach
// (RFC 20 section 6.7). A timeout of zero sets zero, the kernel's own
// behaviour. Nothing is set on a socket that is not the kernel's to query, a
// test's say, and a failed set is left alone: without the option the socket
// falls back to the kernel's retransmission limit, and one accepted socket
// setting nothing closes no connection.
func userTimeout(nc net.Conn, timeout time.Duration) {
	sc, ok := nc.(syscall.Conn)
	if !ok {
		return
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return
	}
	ms := int(timeout / time.Millisecond)
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, ms)
	})
}
