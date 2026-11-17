package entity

import (
	"net/netip"
	"time"
)

// Session is one sign-in to the management API. It holds the hash of its
// current refresh token, never a token; every hash it replaced stays indexed
// until the session expires, so an invalidated token still finds the session
// it must revoke.
type Session struct {
	ID      [16]byte
	Account Principal
	Refresh [32]byte // hash of the current refresh token
	// Created is the store time of the write that created the session, a
	// sign-in or a password change, raised to the account's NotBefore when
	// that is later.
	Created time.Time
	Expires time.Time
	Revoked bool
}

// APIToken is one long-lived credential of an account. It holds the hash of
// its token, never the token.
type APIToken struct {
	ID      [16]byte
	Account Principal
	Hash    [32]byte
	// Created is the store time of creation, raised to the account's
	// NotBefore when that is later.
	Created time.Time
	Expires time.Time
}

// How long a sign-in count is kept after the time its expiry follows: a
// source's last settled failure, a pair's last counted attempt. The throttle
// reads a count as gone at the same distance the store's sweep deletes it.
//
// decision: each is baked into the store's expiry rows already written, so
// changing one between releases strands the rows written under the old value
// where the sweep looks for the new; a change needs a migration that moves
// them.
const (
	SourceSignInLife = 15 * time.Minute
	PairSignInLife   = 24 * time.Hour
)

// SignInFailures is one of a sign-in's three counts: an account's, a source
// address's, or the two together.
type SignInFailures struct {
	Count uint32    // attempts counted since the last success
	Last  time.Time // store time of the last counted attempt; a pair's count expires PairSignInLife after it
	// Failed, a source's only, is the store time of its last settled
	// failure, or of the attempt that created the count; the count expires
	// SourceSignInLife after it.
	Failed time.Time
	Nonce  uint64 // a source's only: drawn by the attempt that created the count
}

// SignInKey names one count: the account's alone (Source zero), the
// source's alone (Account zero), or the two together.
type SignInKey struct {
	Account Principal
	Source  netip.Prefix // as the API normalises a client address
}
