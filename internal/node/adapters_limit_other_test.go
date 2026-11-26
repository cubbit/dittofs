//go:build !linux

package node_test

import (
	"net"
	"testing"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The connection_limit row written for the OSes that carry no second
// loopback IPv4 alias: on the two source addresses every such OS has,
// 127.0.0.1 and ::1, the node's own limit alone refuses the second
// connection: the per-address quarter, one at a limit of 1, holds none from
// an address holding none (RFC 24 section 4.4). The adapter listens on both
// families' loopback. Where ::1 binds nothing the test fails, never skips:
// a machine without the second address cannot run the rule's second
// connection (RFC 13 section 6's never-replaced).

// limitSources is a source address for each connection the test makes, with
// the address it targets of its own family.
var limitSources = []struct{ local, target string }{
	{"127.0.0.1", "127.0.0.1"},
	{"::1", "::1"},
}

// The connection_limit row lowered to 1: a connection from each of the two
// addresses is served before it, at the derived 4,096, and once the node has
// released both the first is served while the second, from an address
// holding none of the limit, is closed at accept (RFC 24 section 4.4).
func TestLimitRowRefusesASecondAddress(t *testing.T) {
	listening := []string{"127.0.0.1", "::1"}
	newOnBoth := func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.Adapters.Listen = listening
		return node.NewAuthenticating(caller(entity.RoleViewer))(cfg)
	}
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(newOnBoth))
	port := freePort(t)
	putSettings(t, h, adapterRows("adapters.smb", true, port))
	started(t, h)
	addr, _ := h.Node().Addrs()
	path := connectionsPath(h.NodeID())

	var open []net.Conn
	for i, from := range limitSources {
		open = append(open, connected(t, uint64(i)+1, from.local, from.target, port))
	}

	for _, c := range open {
		if err := c.Close(); err != nil {
			t.Fatalf("closing a connection the node counted: %v", err)
		}
	}

	wait(t, "the node lists no connection", func() bool {
		return len(listed(t, addr, path)) == 0
	})

	writeNodeRows(t, h, map[string]string{"connection_limit": "1"})

	for i, from := range limitSources {
		c := dialFrom(t, from.local, from.target, port)

		err := node.Negotiate(c, uint64(i)+10)
		if i == 0 && err != nil {
			t.Errorf("the connection from %s at the limit 1: %v, want it answered", from.local, err)
		}

		if i == 1 && err == nil {
			t.Errorf("the second connection, from %s, was answered; want it closed",
				from.local)
		}
	}
}

// connection_limit set before the start runs the count from the start: the
// one connection the row allows is served and the second, from an address
// holding none of it, is closed at accept (RFC 24 section 4.4).
func TestLimitRowSetBeforeStart(t *testing.T) {
	listening := []string{"127.0.0.1", "::1"}
	newOnBoth := func(cfg node.Config) (*node.Node, error) {
		cfg.Bootstrap.Adapters.Listen = listening
		return node.NewAuthenticating(caller(entity.RoleViewer))(cfg)
	}
	h := nodetest.New(t, nodetest.Watch(), nodetest.WithNew(newOnBoth))
	port := freePort(t)
	putSettings(t, h, adapterRows("adapters.smb", true, port))
	putNodeSettings(t, h, map[string]string{"connection_limit": "1"})
	started(t, h)

	for i, from := range limitSources {
		c := dialFrom(t, from.local, from.target, port)

		err := node.Negotiate(c, uint64(i)+1)
		if i == 0 && err != nil {
			t.Errorf("the connection from %s at the limit the row set: %v, want it answered",
				from.local, err)
		}

		if i == 1 && err == nil {
			t.Errorf("the second connection, from %s, was answered; want it closed",
				from.local)
		}
	}
}
