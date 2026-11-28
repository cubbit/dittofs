package nfs

import (
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/pkg/adapter"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// TestHandlersCarryACallbacksConstructorForTheClientAsked checks the handlers
// nfs.New builds carry a callbacks constructor, and that each call hands out
// one value holding the client and protocol asked for: Connect registers that
// value (RFC 20 section 3.3), and a nil, swapped or shared one would send a
// recall or a revocation to the wrong client or to none.
func TestHandlersCarryACallbacksConstructorForTheClientAsked(t *testing.T) {
	h := handlers(adapter.Deps{Budgets: adaptertest.Budgets()})
	if h.Callbacks == nil {
		t.Fatal("h.Callbacks = nil, want a constructor")
	}

	a := h.Callbacks(7, vfs.ProtocolNFSv4)

	ca, ok := a.(*callbacks)
	if !ok {
		t.Fatalf("h.Callbacks returned %T, want *callbacks", a)
	}

	if ca.client != 7 {
		t.Errorf("ca.client = %d, want 7", ca.client)
	}

	if ca.protocol != vfs.ProtocolNFSv4 {
		t.Errorf("ca.protocol = %d, want ProtocolNFSv4 (%d)", ca.protocol, vfs.ProtocolNFSv4)
	}

	b := h.Callbacks(9, vfs.ProtocolNLM)

	cb, ok := b.(*callbacks)
	if !ok {
		t.Fatalf("h.Callbacks returned %T, want *callbacks", b)
	}

	if cb.client != 9 {
		t.Errorf("cb.client = %d, want 9", cb.client)
	}

	if cb.protocol != vfs.ProtocolNLM {
		t.Errorf("cb.protocol = %d, want ProtocolNLM (%d)", cb.protocol, vfs.ProtocolNLM)
	}

	if a == b {
		t.Error("h.Callbacks returned the same value for client 7 and client 9")
	}
}

// A client whose lease expired has its sessions ended and their pool charge
// returned, and only its: another client's session stays, and so does a
// session of the client when the revocation is of another kind, since a
// session lives under the lease alone (RFC 21 section 3.3). A session left
// would answer a client the service has forgotten and hold the pool for it.
func TestRevokedLeaseExpiryEndsTheClientsSessions(t *testing.T) {
	spec := protocol.Spec{
		Fore: protocol.Channel{
			Slots:       1,
			MaxRequest:  4096,
			MaxResponse: 4096,
			MaxOps:      1,
		},
	}

	for _, tc := range []struct {
		name   string
		reason vfs.RevokeReason
		ended  bool
	}{
		{"lease expiry", vfs.RevokedLeaseExpired, true},
		{"an administrator's revoke", vfs.RevokedByAdmin, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := adapter.Deps{Budgets: adaptertest.Budgets()}
			h := handlers(d)

			mine, err := h.Sessions.Create(7, spec)
			if err != nil {
				t.Fatal(err)
			}

			other, err := h.Sessions.Create(9, spec)
			if err != nil {
				t.Fatal(err)
			}

			before := d.Budgets.Connection.Reserved()

			h.Callbacks(7, vfs.ProtocolNFSv4).Revoked(
				t.Context(),
				vfs.Revocation{Client: 7, Reason: tc.reason},
			)

			if _, held := h.Sessions.Get(mine.ID); held == tc.ended {
				t.Errorf("client 7's session is held: %v, want %v", held, !tc.ended)
			}

			if _, held := h.Sessions.Get(other.ID); !held {
				t.Error("client 9's session is gone, want it kept")
			}

			if got := d.Budgets.Connection.Reserved(); tc.ended && got >= before {
				t.Errorf(
					"the pool holds %d bytes, want fewer than %d: the charge returned",
					got,
					before,
				)
			}

			if got := d.Budgets.Connection.Reserved(); !tc.ended && got != before {
				t.Errorf("the pool holds %d bytes, want %d: nothing returned", got, before)
			}

			h.Sessions.EndClient(7)
			h.Sessions.EndClient(9)
		})
	}
}
