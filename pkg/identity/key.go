package identity

import (
	"crypto/sha256"
	"encoding/binary"
)

// ProofKey is a collision-resistant digest of what a request proved and
// claimed. Two identities share one only if they proved exactly the same thing.
type ProofKey [32]byte

// Key returns the proof key: SHA-256 over an encoding of every field of the
// Identity but Client, the evidence's included. Every fixed-width field is
// written as itself and every string and slice with its length first, so no
// two identities differing in a keyed field encode alike. It is computed on
// each call and never fails, the zero principal of a None identity included.
// Adding a field to Identity or Evidence is an edit to this function.
//
// ponytail: it hashes the whole evidence on every call, which costs
// microseconds. Compute the evidence's digest once at sign-in, keeping it
// across node channels, when a profile shows Key in admission's cost.
//
// decision: a Kerberos row's linked SID has no field of its own here. Sign-in
// binds the ticket's PAC to the SID the row is linked for, so that SID is the
// evidence's first and is keyed with it, and relinking the row is a write that
// replaces the identity State a cached caller was built under. Give it a field
// if resolution ever reads the row itself.
func (id Identity) Key() ProofKey {
	b := make([]byte, 0, 128)
	b = append(b, byte(id.Proof), byte(id.Protection))

	b = binary.BigEndian.AppendUint32(b, id.Claims.UID)
	b = binary.BigEndian.AppendUint32(b, id.Claims.GID)
	b = binary.BigEndian.AppendUint64(b, uint64(len(id.Claims.GIDs)))
	for _, g := range id.Claims.GIDs {
		b = binary.BigEndian.AppendUint32(b, g)
	}

	e := id.Evidence
	b = binary.BigEndian.AppendUint64(b, uint64(len(e.Name)))
	b = append(b, e.Name...)
	b = append(b, byte(e.Principal.Kind))
	b = append(b, e.Principal.ID[:]...)
	b = appendSIDs(b, e.SIDs)
	b = appendSIDs(b, e.DenyOnly)
	// The instant alone: two Times of one instant in different locations, or
	// one carrying a monotonic reading, prove the same thing.
	b = binary.BigEndian.AppendUint64(b, uint64(e.Expires.Unix()))
	b = binary.BigEndian.AppendUint32(b, uint32(e.Expires.Nanosecond()))
	return sha256.Sum256(b)
}

// appendSIDs writes the list's length, then each SID field by field: its
// count byte is written as well as the length of its sub-authorities, since
// nothing holds the two equal.
func appendSIDs(b []byte, sids []SID) []byte {
	b = binary.BigEndian.AppendUint64(b, uint64(len(sids)))
	for _, s := range sids {
		b = append(b, s.Revision, s.SubAuthorityCount)
		b = append(b, s.IdentifierAuthority[:]...)
		b = binary.BigEndian.AppendUint64(b, uint64(len(s.SubAuthorities)))
		for _, a := range s.SubAuthorities {
			b = binary.BigEndian.AppendUint32(b, a)
		}
	}
	return b
}
