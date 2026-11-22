// Package fixture sets up the server a cell's tests run against, and what
// they reach it through: DittoFS, initialised with dfs init and started with
// dfs start, or the kernel's NFS server, the namespaced clients, a KDC, an S3
// bucket, and the share's NFS and SMB mounts. dt starts it before a cell's
// first test and stops it after the last.
package fixture

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/cubbit/dittofs/test/harness/registry"
)

// Share is the fixture's share, the default one when no shares file is given.
const Share = "/export"

// User is {user} and User2 is {user2}: the test users every DittoFS fixture
// creates, each with its UID as its GID too, and no password yet, since no
// route sets one.
const (
	User     = "dt-user"
	UserUID  = uint32(65535)
	User2    = "dt-user2"
	User2UID = uint32(65536)
)

// stopGrace is how long a process dt started has to exit before its group is
// killed.
const stopGrace = 10 * time.Second

// Server is a DittoFS fixture: the dfs it started, the API client signed in
// as its administrator, the share it mounts, over NFS or SMB, and the
// namespaced clients it mounts it from.
//
// decision: the NFS and SMB mounts, the namespaced clients, the KDC and the
// S3 bucket stay on Server, though no fixture reaches them yet (Unserved),
// because a fixture serving a protocol through the node reuses them. Delete
// them if that fixture lands without them.
type Server struct {
	f       registry.Fixture
	data    string
	server  string    // {server}
	shares  []string  // the shares' names, without a leading /; the first is {share}
	nfsPort int       // 0 when the fixture serves no NFS
	smbPort int       // 0 when the fixture serves no SMB
	mount   string    // {mount}, once the fixture makes it
	clients *clients  // its namespaced clients, when it has them
	out     io.Writer // dt's lines on the setup and the teardown

	bin        string            // the directory holding dfs
	config     string            // the bootstrap's path
	home       string            // the children's HOME, under which their XDG directories are
	passphrase string            // the storage key's
	password   string            // the administrator's
	node       [16]byte          // the node's identity, which the bootstrap names
	namespace  string            // the ID of the namespace the shares are in
	users      map[string]string // each fixture user's ID, by name
	shareIDs   map[string]string // each share's ID, by name
	log        *os.File          // dfs.log, dfs init's and dfs start's output

	cmd     *exec.Cmd // dfs start, while it runs
	exited  chan struct{}
	err     error // what cmd's Wait returned, once exited is closed
	killed  bool  // Kill ended cmd, which Restart may replace
	api     string
	client  *client.Client
	session *session // the client's token source

	grace    time.Duration // how long Stop waits after SIGTERM; zero is stopGrace
	stop     sync.Once
	stopDone bool // Stop has run: nothing starts again
	stopErr  error
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

// interfaces lists this machine's interfaces.
func interfaces() ([]iface, error) {
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

// address is {server}, picked from this machine's interfaces.
func address() (string, error) {
	list, err := interfaces()
	if err != nil {
		return "", err
	}
	return pickAddress(list)
}

// pickAddress is {server} among the interfaces in list: the IPv4 address of
// the first that is up and not loopback, leaving out link-local addresses. A
// client connecting to it has an address of the same kind, which the server
// dials for NFSv4.0 callbacks; it refuses a loopback one.
func pickAddress(list []iface) (string, error) {
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
