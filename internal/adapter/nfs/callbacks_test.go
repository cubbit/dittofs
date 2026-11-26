package nfs

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// TestHandlersCarryACallbacksConstructorForTheClientAsked checks the handlers
// nfs.New builds carry a callbacks constructor, and that each call hands out
// one value holding the client and protocol asked for: Connect registers that
// value (RFC 20 section 3.3), and a nil, swapped or shared one would send a
// recall or a revocation to the wrong client or to none.
func TestHandlersCarryACallbacksConstructorForTheClientAsked(t *testing.T) {
	h := handlers(adapter.Deps{})
	if h.Callbacks == nil {
		t.Fatal("h.Callbacks = nil, want a constructor")
	}

	a := h.Callbacks(7, entity.ProtocolNFSv4)

	ca, ok := a.(*callbacks)
	if !ok {
		t.Fatalf("h.Callbacks returned %T, want *callbacks", a)
	}

	if ca.client != 7 {
		t.Errorf("ca.client = %d, want 7", ca.client)
	}

	if ca.protocol != entity.ProtocolNFSv4 {
		t.Errorf("ca.protocol = %d, want ProtocolNFSv4 (%d)", ca.protocol, entity.ProtocolNFSv4)
	}

	b := h.Callbacks(9, entity.ProtocolNLM)

	cb, ok := b.(*callbacks)
	if !ok {
		t.Fatalf("h.Callbacks returned %T, want *callbacks", b)
	}

	if cb.client != 9 {
		t.Errorf("cb.client = %d, want 9", cb.client)
	}

	if cb.protocol != entity.ProtocolNLM {
		t.Errorf("cb.protocol = %d, want ProtocolNLM (%d)", cb.protocol, entity.ProtocolNLM)
	}

	if a == b {
		t.Error("h.Callbacks returned the same value for client 7 and client 9")
	}
}
