//go:build !linux

package api

import (
	"errors"
	"net"
	"os"
)

// listenPrivate binds a unix socket at path and then makes it 0600, since
// these hosts refuse a mode on a socket before bind.
//
// decision: a connection made between the bind and the chmod, by another
// user able to reach the path's directory, is accepted, which only a umask
// granting group or others write allows, since connecting needs write
// permission on the socket. Such a caller still authenticates as everyone
// does; what it gains is the local mark, which spares it the sign-in
// throttle's per-source count. And chmod follows a symlink, so another user
// who swaps the socket for a link between the two has the server chmod the
// link's target. Holding the socket in a directory only the server's user
// may enter closes both. The mode is not set through the umask, since the
// umask is the process's and would change every file another goroutine
// creates meanwhile. Overturned if the local mark ever admits a call by
// itself, or the directory cannot be the server's user's alone.
func listenPrivate(path string) (net.Listener, error) {
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, errors.Join(err, l.Close())
	}
	return l, nil
}
