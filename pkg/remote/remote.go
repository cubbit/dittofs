// Package remote is the remote block store's contract (RFC 4 §4.1): the
// types, constants, interface and errors every consumer of the remote tier
// shares. It holds no state and no behaviour beyond methods over its own
// values, and imports the standard library only (RFC 4 §1.2). The codec and
// every store live in packages beneath it and count as implementations.
package remote

import (
	"strconv"
	"time"
)

// Name is a block's name (RFC 2 §4.2): derived from its chunk hashes, key
// scope, the put attempt's nonce and chain ID.
type Name [32]byte

// Role names a control object (RFC 4 §4.13). The set is fixed in code: a new
// role is a new constant and a new location, never a caller-chosen key.
type Role uint8

// decision: the repair proof's role (RFC 4 §4.11) is not declared, since its one
// caller is a Recheck pass that accepts the versioning repair proof, which has
// not landed. A role's number is never persisted or sent (a control object is
// keyed by its String), so declaring it then changes no stored value. Add
// it with the first Recheck pass that accepts a repair proof.
const (
	RoleClaim  Role = iota + 1 // the namespace claim (RFC 27)
	RoleHealth                 // the health probe's object (RFC 4 §4.7)
	RoleKeys                   // the namespace's key records, wrapped; a non-encrypting namespace's chunk-ID key in the clear (RFC 5 Appendix B.2)
)

// String is the role's name as RFC 4 §4.13 writes it. A store keys a control
// object by this name, never by the role's number.
func (r Role) String() string {
	switch r {
	case RoleClaim:
		return "claim"
	case RoleHealth:
		return "health"
	case RoleKeys:
		return "keys"
	}

	return "Role(" + strconv.Itoa(int(r)) + ")"
}

// Mode is how a store treats versions and deletes (RFC 4 §4.14). Every
// namespace's store is Mutable; a store refuses to open Immutable until the
// calls a backup location makes are declared.
type Mode uint8

const (
	Mutable   Mode = iota + 1 // no versioning, no lock, deletes allowed
	Immutable                 // versioning and compliance lock required, no delete
)

// Version names one stored version of a name in an immutable store (RFC 4
// §4.14); GetControl reports it, empty on a mutable store.
type Version string

// Checksum names the checksum a put carries up front, as the capability check
// chose it (RFC 4 §4.11), or None when the store verifies the service's digest
// instead. None is the zero value, so the zero StoredSum reports no checksum.
type Checksum uint8

const (
	ChecksumNone   Checksum = iota // no checksum sent; a get's service reported none
	ChecksumCRC32C                 // CRC32C, sent with the put
	ChecksumMD5                    // MD5, sent with the put or compared with the service's digest
)

// StoredSum is the whole-object checksum the service holds for what a get
// reads, with its kind; Kind is ChecksumNone when the service reported none
// (RFC 4 §4.4).
type StoredSum struct {
	Kind Checksum
	Sum  []byte
}

// Direction is put or get; health is probed per direction (RFC 4 §4.7).
type Direction uint8

const (
	DirPut Direction = iota + 1
	DirGet
)

// Range is a byte range in an encoded block. The zero Range means the whole
// block.
type Range struct{ Off, Len int64 }

// Chunk is what a transfer carries: a chunk's plaintext hash and its bytes,
// verified against that hash on a get (RFC 4 §3.4). It has no file offset, and
// is not the carver's chunk (RFC 2 §1.2).
type Chunk struct {
	Hash  [32]byte
	Bytes []byte
}

// ChunkRange asks for one chunk of a block: the hash it is verified against and
// its range in the encoded block, as block metadata records it (RFC 6 §2.2).
type ChunkRange struct {
	Hash  [32]byte
	Range Range
}

// Meter receives a transfer's wire bytes (RFC 3 §2.4): Moved with each count of
// encoded body bytes sent or read, and Sent once a put's last body byte has
// been sent. A nil func is skipped, so the zero Meter meters nothing. It is two
// funcs and carries no behaviour of its own.
type Meter struct {
	Moved func(n int64)
	Sent  func()
}

// Cursor is where a walk of List resumes: the last service key the previous
// page listed, block or not, since a Name cannot stand for a key that is not
// a block (RFC 4 §4.6). It is opaque to callers, who keep one and pass it back
// to the store that returned it; the zero Cursor starts at the beginning.
type Cursor string

// Info describes one stored block, as a listing reports it.
type Info struct {
	Name    Name
	Size    int64     // GC reports the space an unrecorded block holds
	ModTime time.Time // when the service stored it; reported, never used to decide a delete
}
