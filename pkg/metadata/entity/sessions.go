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

// SignInFailures is one of a sign-in's three counts: an account's, a source
// address's, or the two together.
type SignInFailures struct {
	Count uint32    // attempts counted since the last success
	Last  time.Time // store time of the last counted attempt; a pair's count expires 24 h after it
	// Failed, a source's only, is the store time of its last settled
	// failure, or of the attempt that created the count; the count expires
	// 15 min after it.
	Failed time.Time
	Nonce  uint64 // a source's only: drawn by the attempt that created the count
}

// SignInKey names one count: the account's alone (Source zero), the
// source's alone (Account zero), or the two together.
type SignInKey struct {
	Account Principal
	Source  netip.Prefix // as the API normalises a client address
}
