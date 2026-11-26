package gss_test

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/gss"
	"github.com/cubbit/dittofs/pkg/identity/identitytest"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// clientAddr is the client address a protocol hands Identity.
var clientAddr = netip.MustParseAddr("198.51.100.7")

// The Identity of a Kerberos context set up through memory's SignIn carries
// a ticket's proof, the context's principal and expiry, no claims and the
// address given, with Protection left at its zero for the protocol to set.
// Catches a principal or an expiry dropped, a proof weaker than a ticket,
// claims carried in, and an address other than the one given.
func TestIdentityFromKerberosContext(t *testing.T) {
	f := newFixture(t)
	end := now.Add(8 * time.Hour)
	init, _ := f.initiator(t, identitytest.TicketOpts{Client: "admin", End: end})
	mech := gss.Kerberos(f.svc, nfsStep)
	if _, done, err := mech.Step(t.Context(), firstToken(t, init)); err != nil || !done {
		t.Fatalf("the exchange = done %v, %v; want a context", done, err)
	}
	c := mech.Context()
	id, err := gss.Identity(c, clientAddr)
	if err != nil {
		t.Fatalf("Identity = %v, want an Identity", err)
	}
	if id.Proof != vfs.ProofTicket {
		t.Errorf("Identity: Proof = %v, want ProofTicket", id.Proof)
	}
	if id.Evidence.Principal != f.svc.Store.Admin.Principal {
		t.Errorf("Identity: Evidence.Principal = %v, want the ticket's principal %v", id.Evidence.Principal, f.svc.Store.Admin.Principal)
	}
	if !id.Evidence.Expires.Equal(end) {
		t.Errorf("Identity: Evidence.Expires = %v, want the ticket's end %v", id.Evidence.Expires, end)
	}
	if !reflect.DeepEqual(id.Evidence, c.Evidence) {
		t.Errorf("Identity: Evidence = %+v, want the context's %+v", id.Evidence, c.Evidence)
	}
	if !reflect.DeepEqual(id.Claims, vfs.Claims{}) {
		t.Errorf("Identity: Claims = %+v, want none", id.Claims)
	}
	if id.Client != clientAddr {
		t.Errorf("Identity: Client = %v, want the address given %v", id.Client, clientAddr)
	}
	if id.Protection != vfs.ProtectionNone {
		t.Errorf("Identity: Protection = %v, want it left at ProtectionNone for the protocol", id.Protection)
	}
}

// A nil context is the logon failure, never an anonymous Identity. Catches
// a missing context turned into a caller.
func TestIdentityNilContext(t *testing.T) {
	id, err := gss.Identity(nil, clientAddr)
	if !errors.Is(err, vfs.ErrLogonFailure) {
		t.Errorf("Identity of no context = %v, want ErrLogonFailure", err)
	}
	if !reflect.DeepEqual(id, vfs.Identity{}) {
		t.Errorf("Identity of no context = %+v, want the zero Identity", id)
	}
}
