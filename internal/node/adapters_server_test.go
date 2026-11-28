package node_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/node/nodetest"
	"github.com/cubbit/dittofs/pkg/adapter"
)

// The node hands its adapters the installation's ID, the same on every node,
// and its own ID, so an NFSv4.1 client is told a server scope and owner that
// are the installation's and a minor ID that is the node's (RFC 21 section
// 3.3): neither is zero.
func TestNodeFillsTheAdaptersServerIdentity(t *testing.T) {
	h := nodetest.New(t)
	in, err := h.Store().Installation(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	got := h.Node().AdapterServer()
	if got == (adapter.Server{}) {
		t.Fatal("the adapters' Deps.Server is zero")
	}

	want := adapter.Server{Installation: in.ID, Node: h.NodeID()}
	if got != want {
		t.Errorf("Deps.Server = %x, want the installation %x and the node %x", got, in.ID, h.NodeID())
	}
}
