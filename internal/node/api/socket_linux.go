package api

import (
	"context"
	"net"
	"syscall"
)

// listenPrivate binds a unix socket at path with mode 0600 from the start:
// bind gives the socket file the mode of the socket before it, less the
// umask, so the socket is made 0600 before bind, leaving no moment another
// user could connect and no path a chmod could follow.
func listenPrivate(path string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		if cerr := c.Control(func(fd uintptr) { err = syscall.Fchmod(int(fd), 0o600) }); cerr != nil {
			return cerr
		}
		return err
	}}
	return lc.Listen(context.Background(), "unix", path)
}
