package gss

import (
	"errors"
	"net/netip"

	"github.com/cubbit/dittofs/pkg/vfs"
)

// Identity is a request's Identity from the context c its client set up, the
// one place either protocol builds one from a context: a ticket's proof, what
// SignIn established, no claims, and the client's address addr. Protection
// is the protocol's to set per request, from how that request came. A
// Kerberos context answers the same Identity for NFS and SMB, so there is no
// protocol argument.
// The context's Keys stay with it: an Identity has no field for a key. A nil
// context is the logon failure, never an anonymous Identity.
func Identity(c *Context, addr netip.Addr) (vfs.Identity, error) {
	if c == nil {
		return vfs.Identity{}, logonFailure(errors.New("no context"))
	}
	return vfs.Identity{Proof: vfs.ProofTicket, Evidence: c.Evidence, Client: addr}, nil
}
