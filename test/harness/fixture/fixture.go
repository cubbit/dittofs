// Package fixture sets up the server a cell's tests run against, and what
// they reach it through: the kernel's NFS server, the namespaced clients, a
// KDC, an S3 bucket, and the share's NFS and SMB mounts. dt starts it before a
// cell's first test and stops it after the last. A DittoFS server it refuses,
// until its fixture is rebuilt on the new control plane (ErrNoServer).
package fixture

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"time"

	"github.com/cubbit/dittofs/test/harness/registry"
)

// Share is the fixture's share, the default one when no shares file is given.
const Share = "/export"

// User is {user} and User2 is {user2}: the test users every fixture creates,
// with {password}, each with its UID as its GID too.
const (
	User     = "dt-user"
	UserUID  = uint32(65535)
	User2    = "dt-user2"
	User2UID = uint32(65536)
)

// stopGrace is how long a process dt started has to exit before its group is
// killed.
const stopGrace = 10 * time.Second

// Server is a DittoFS fixture's mounts and clients: the share it mounts, over
// NFS or SMB, and the namespaced clients it mounts it from. dt starts no
// DittoFS server to hold them until its fixture is rebuilt (ErrNoServer).
type Server struct {
	f       registry.Fixture
	data    string
	shares  []string  // the shares' names, the first is {share}
	nfsPort int       // 0 when the fixture serves no NFS
	smbPort int       // 0 when the fixture serves no SMB
	mount   string    // {mount}, once the fixture makes it
	clients *clients  // its namespaced clients, when it has them
	out     io.Writer // dt's lines on the setup and the teardown
}

// Command is argv as a cell's test runs it, in dir with env added: on the
// fixture's namespaced client, as the user dt runs as, or else as it is.
func (s *Server) Command(argv []string, dir string, env []string) ([]string, error) {
	if s.clients == nil {
		return argv, nil
	}
	return s.clients.asUser(argv, dir, env)
}

// secret returns 32 random hex digits.
func secret() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// iface is what choosing {server} reads of a network interface.
type iface struct {
	flags net.Flags
	addrs []net.Addr
}

// interfaces lists this machine's interfaces; the tests replace it.
var interfaces = func() ([]iface, error) {
	list, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]iface, 0, len(list))
	for _, i := range list {
		addrs, _ := i.Addrs()
		out = append(out, iface{i.Flags, addrs})
	}
	return out, nil
}

// address is {server}: the IPv4 address of the first interface that is up
// and not loopback, leaving out link-local addresses. A client connecting to
// it has an address of the same kind, which the server dials for NFSv4.0
// callbacks; it refuses a loopback one.
func address() (string, error) {
	list, err := interfaces()
	if err != nil {
		return "", err
	}
	for _, i := range list {
		if i.flags&net.FlagUp == 0 || i.flags&net.FlagLoopback != 0 {
			continue
		}
		for _, a := range i.addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip := n.IP.To4(); ip != nil && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() {
					return ip.String(), nil
				}
			}
		}
	}
	return "", errors.New("this machine has no IPv4 address on an interface that is up and not loopback")
}
