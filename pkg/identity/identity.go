// Package identity is the contract of identity and authentication: the values
// an adapter's proof becomes, the interfaces the identity server implements
// and the filesystem and management API call, and the errors they answer. It
// is a leaf: it imports only the standard library, the metadata store's value
// types, the SID codec and the filesystem's error values. pkg/vfs aliases the
// value types an adapter needs, so an adapter imports pkg/vfs alone.
package identity

import (
	"net/netip"
	"time"

	"github.com/cubbit/dittofs/pkg/auth/sid"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// SID is a Windows security identifier.
type SID = sid.SID

// Identity is what the adapter proved for one request, not yet mapped through
// any share's policy, in no protocol's terms. It names no flavour, mechanism
// or protocol: each adapter maps its own to a ProofKind and a Protection. It
// never carries a key.
type Identity struct {
	Proof      ProofKind
	Protection Protection
	Claims     Claims     // what the client asserted; read only at ProofClaimed, never as proof
	Evidence   Evidence   // what sign-in established; zero below ProofSecret
	Client     netip.Addr // the client's address, which client rules read
}

// ProofKind is how the sender was proved, in increasing order. ProofSecret
// and ProofTicket prove a principal.
type ProofKind uint8

const (
	ProofNone    ProofKind = iota // nothing: an anonymous or guest caller
	ProofClaimed                  // POSIX ids asserted, nothing verified
	ProofSecret                   // NTLMv2's challenge and response, and nothing else
	ProofTicket                   // a third party's mutual proof, such as Kerberos
)

// Protection is how the request's messages are protected, in increasing order.
type Protection uint8

const (
	ProtectionNone      Protection = iota
	ProtectionIntegrity            // every message signed
	ProtectionPrivacy              // every message sealed
)

// Claims are POSIX ids a client asserted. They name no principal until
// resolution maps them, and they never make a caller authenticated.
type Claims struct {
	UID, GID uint32
	GIDs     []uint32
}

// Evidence is what one sign-in established, and nothing more. It holds no
// key, so it is safe to copy into every request's Identity.
type Evidence struct {
	Name      string           // canonical: "alice@CORP.EXAMPLE", "CORP\\alice", a local user name; empty: none
	Principal entity.Principal // what sign-in bound Name to; every call acts as it; zero: none
	SIDs      []SID            // from a verified PAC or a pass-through reply: the user's first; else empty
	DenyOnly  []SID            // marked deny-only by the signer
	Expires   time.Time        // the ticket's end time; zero for NTLM
}

// EvidenceDigest is the evidence's part of the proof key: a collision-resistant
// hash of the fields sign-in established, computed once at sign-in. It is
// derived from identifiers, not from a secret, so it is no key.
type EvidenceDigest [32]byte
