//go:build linux

package node_test

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/node"
	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// The connection_limit row on an OS with more loopback addresses: from
// three of them, the node's own limit refuses the third, and not the
// per-address quarter, which at a limit of 2 holds one and refuses none
// from an address holding none (RFC 24 section 4.4); above the node's bound
// the change is refused and the limit stands (RFC 13 section 6).

// limitSources is a source loopback address for each connection the tests
// make, with the address every one of them targets.
var limitSources = []struct{ local, target string }{
	{"127.0.0.1", "127.0.0.1"},
	{"127.0.0.2", "127.0.0.1"},
	{"127.0.0.3", "127.0.0.1"},
}

// limitHarness is a node serving SMB from the settings rows, its API's
// address and its connection listing's path beside it.
func limitHarness(t *testing.T) (*nodetest.Harness, uint16, string, string) {
	t.Helper()
	h := nodetest.New(t, nodetest.Watch(),
		nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleViewer))))
	port := freePort(t)
	putSettings(t, h, adapterRows("adapters.smb", true, port))
	started(t, h)
	addr, _ := h.Node().Addrs()

	return h, port, addr, connectionsPath(h.NodeID())
}

// countListed is how many connections the node lists now, read over its API
// as a viewer does.
func countListed(t *testing.T, addr, path string) int {
	t.Helper()
	return len(listed(t, addr, path))
}

// The connection_limit row lowered to 2: a connection from each of three
// loopback addresses is served before it, at the derived 4,096, and once the
// node has released all of them the first two are served while the third,
// from an address holding none of the limit, is closed at accept, the node's
// own limit refusing it alone (RFC 24 section 4.4).
func TestLimitRowLowered(t *testing.T) {
	h, port, addr, path := limitHarness(t)

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
		return countListed(t, addr, path) == 0
	})

	writeNodeRows(t, h, map[string]string{"connection_limit": "2"})

	for i, from := range limitSources {
		c := dialFrom(t, from.local, from.target, port)

		err := node.Negotiate(c, uint64(i)+10)
		if i < 2 && err != nil {
			t.Errorf("connection %d from %s at the limit 2: %v, want it answered",
				i, from.local, err)
		}

		if i == 2 && err == nil {
			t.Errorf("the third connection, from %s, answered; want it closed at accept",
				from.local)
		}
	}
}

// connection_limit changed above the node's bound is refused, the record
// naming the node's own row, and the limit stands at its last value: with two
// connections open at the limit 2, the third, from an address holding none,
// is still closed at accept (RFC 13 section 6, RFC 24 section 4.4).
func TestLimitAboveBoundChange(t *testing.T) {
	h, port, addr, path := limitHarness(t)
	writeNodeRows(t, h, map[string]string{"connection_limit": "2"})

	for i, from := range limitSources[:2] {
		connected(t, uint64(i)+1, from.local, from.target, port)
	}

	wait(t, "the node lists the two connections", func() bool {
		return countListed(t, addr, path) == 2
	})

	writeNodeRows(t, h, map[string]string{"connection_limit": "4097"})

	refused := h.Node().Refused()

	ofNode := entity.SettingRef(
		entity.SettingScope{Kind: entity.ScopeNode, ID: h.NodeID()}, "connection_limit")
	if len(refused) != 1 || refused[0].Record.Kind != ofNode.Kind ||
		!bytes.Equal(refused[0].Record.ID, ofNode.ID) {
		t.Fatalf(
			"connection_limit changed above the bound: %s, want the node's record alone refused",
			describe(refused),
		)
	}

	third := limitSources[2]

	if err := node.Negotiate(dialFrom(t, third.local, third.target, port), 30); err == nil {
		t.Errorf("the third connection, from %s, answered; want the refusal to keep the limit",
			third.local)
	}

	// A shape refused row after it is refused once, for its shape: the value
	// above the bound is no longer stored.
	writeNodeRows(t, h, map[string]string{"connection_limit": "0"})

	refused = h.Node().Refused()
	namesOnly(t, "a shape refused row after one above the bound", refused, ofNode)

	if got := describe(refused); strings.Contains(got, "4097") {
		t.Errorf("the shape refused row: %s, want no refusal of the replaced 4097", got)
	}
}

// connection_limit set before the start runs the count from the start: the
// two connections the row allows are served and the third, from an address
// holding none of it, is closed at accept (RFC 24 section 4.4).
func TestLimitRowSetBeforeStart(t *testing.T) {
	h := nodetest.New(t, nodetest.Watch(),
		nodetest.WithNew(node.NewAuthenticating(caller(entity.RoleViewer))))
	port := freePort(t)
	putSettings(t, h, adapterRows("adapters.smb", true, port))
	putNodeSettings(t, h, map[string]string{"connection_limit": "2"})
	started(t, h)

	for i, from := range limitSources {
		c := dialFrom(t, from.local, from.target, port)

		err := node.Negotiate(c, uint64(i)+1)
		if i < 2 && err != nil {
			t.Errorf("connection %d from %s at the limit the row set: %v, want it answered",
				i, from.local, err)
		}

		if i == 2 && err == nil {
			t.Errorf("the third connection, from %s, answered; want it closed at accept",
				from.local)
		}
	}
}
