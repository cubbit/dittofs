package protocol

import (
	"net/netip"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// MOUNT's two procedures that reach the service as entry calls, Root and
// ListShares (RFC 1813 Appendix I, RFC 21 Appendix D): MNT and EXPORT.
const (
	procMountMnt    uint32 = 1
	procMountExport uint32 = 5
)

// identity is the neutral Identity a credential gives, a pure function of the
// credential and the client's address (RFC 21 section 11.1): AUTH_NONE proves
// nothing, AUTH_SYS claims the uid, gid and groups it carries, and every
// other flavour, or an AUTH_SYS with no credential, is the refusal Route
// answers AUTH_BADCRED, never a default and never the anonymous Identity. It
// squashes nothing and decides no privilege: a claimed uid 0 stays the claim
// 0. It takes no certificate, so a client certificate authenticates the
// connection and never the caller. The claims' group list is gids's, so the
// Identity holds no allocation of its own.
//
// decision: the Identity of AUTH_NONE and AUTH_SYS is built here, in the NFS
// protocol, and not in gss.Identity as RFC 20 section 4.4 has it, since the
// RFC's package is not built and only NFS has an AUTH_SYS arm. Overturned if
// the gss package lands first with an AUTH_SYS arm: this function then moves
// there.
func identity(
	flavour uint32, sys *wire.AuthSysParms, addr netip.Addr, gids *[16]uint32,
) (vfs.Identity, error) {
	switch {
	case flavour == wire.AuthNone:
		return vfs.Identity{Proof: vfs.ProofNone, Protection: vfs.ProtectionNone, Client: addr}, nil
	case flavour == wire.AuthSys && sys != nil:
		n := copy(gids[:], sys.Groups())

		return vfs.Identity{
			Proof:      vfs.ProofClaimed,
			Protection: vfs.ProtectionNone,
			Claims:     vfs.Claims{UID: sys.UID, GID: sys.GID, GIDs: gids[:n]},
			Client:     addr,
		}, nil
	}

	return vfs.Identity{}, errAuthCred
}

// callCaps is the one capability profile a call states, chosen by its program,
// version and procedure alone and never built from a field of the request (RFC
// 17 section 3.0): MOUNT's MNT and EXPORT reach the service as entry calls,
// MountCaps, MOUNT's other procedures and NFSv3's NFSv3Caps, NFSv4's
// NFSv4Caps, NLM's and NSM's NLMCaps. A program no table serves states the
// narrowest profile, which fails closed.
//
// decision: the profile is a whole one chosen here, never a bit alone, since
// admission fails open for a call that states HostCall by mistake (RFC 17
// section 4.9): so only the procedures RFC 21 Appendix D lists for the host
// services state it, and the table test holds every program and procedure to
// its profile. Overturned if a profile must depend on the request's own
// fields.
func callCaps(prog, vers, proc uint32) vfs.Caps {
	switch prog {
	case wire.ProgNFS:
		if vers == wire.VersNFS4 {
			return vfs.NFSv4Caps
		}

		return vfs.NFSv3Caps
	case wire.ProgMount:
		if proc == procMountMnt || proc == procMountExport {
			return vfs.MountCaps
		}

		return vfs.NFSv3Caps
	case wire.ProgNLM, wire.ProgNSM:
		return vfs.NLMCaps
	}

	return 0
}

// callProtocol is the label a call's context carries (RFC 17 section 3.0):
// NFSv3 and MOUNT NFSv3, NFSv4 NFSv4, NLM and NSM NLM, which counts under the
// same metric label as NFSv3. A program no table serves has none.
func callProtocol(prog, vers uint32) vfs.Protocol {
	switch prog {
	case wire.ProgNFS:
		if vers == wire.VersNFS4 {
			return vfs.ProtocolNFSv4
		}

		return vfs.ProtocolNFSv3
	case wire.ProgMount:
		return vfs.ProtocolNFSv3
	case wire.ProgNLM, wire.ProgNSM:
		return vfs.ProtocolNLM
	}

	return 0
}
