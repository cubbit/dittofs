// Package caps is the capabilities an adapter states on every call: what its
// protocol does, as bits of one value, so the service's rules read a
// behaviour and never a protocol. It imports nothing, so open state and the
// service both read the one type; pkg/vfs re-exports it.
package caps

import (
	"fmt"
	"strings"
)

// Caps is a set of capabilities. Its zero value is the narrowest caller: each
// bit only widens or changes the one rule it names.
type Caps uint16

const (
	PrivilegeOnEveryCall Caps = 1 << iota // a privileged caller's privilege counts on every call, not only through a backup-intent open
	OpenFilesKeepNames                    // an open name keeps it: a rename over an open file, or of a directory with an open made through it, is refused
	WantOpenID                            // each open is issued an OpenID that names it from then on
	GrantBelongsToOpen                    // a grant asked with no lease key belongs to its open alone, an oplock
	ClientIsMachine                       // the client is a whole machine its users share: its record names no owner
	HostCall                              // the call comes from a host's lock or mount service, which proves no user
	SeesPseudoFS                          // the client sees a pseudo-filesystem above the shares; LOOKUPP at a share's root goes up to it
	SpellsReservedNames                   // on a windows-rule share, a reserved character or trailing space or dot in a name is stored in its private-use spelling, not refused
	ListsEveryEntry                       // a listing is never filtered by access-based enumeration; without it, a share asking that filter hides what the caller cannot read
)

// The profile each adapter states on every call; nothing else builds a Caps.
//
//	profile    privilege  keep-names  open-id  oplock  machine  host  pseudo-fs  spell-names  list-all
//	NFSv3      x                                                                  x            x
//	NFSv4      x                                                       x          x            x
//	Mount      x                                                 x
//	NLM        x                                         x       x
//	SMB                   x           x        x         x
//
// MOUNT states MountCaps on MNT and EXPORT alone, NFSv3Caps otherwise; NSM
// states NLMCaps.
const (
	NFSv3Caps = PrivilegeOnEveryCall | SpellsReservedNames | ListsEveryEntry
	NFSv4Caps = PrivilegeOnEveryCall | SeesPseudoFS | SpellsReservedNames | ListsEveryEntry
	MountCaps = PrivilegeOnEveryCall | HostCall
	NLMCaps   = PrivilegeOnEveryCall | ClientIsMachine | HostCall
	SMBCaps   = OpenFilesKeepNames | WantOpenID | GrantBelongsToOpen | ClientIsMachine
)

// Has reports whether every bit of x is set in c.
func (c Caps) Has(x Caps) bool { return c&x == x }

var names = []string{"privilege", "keep-names", "open-id", "oplock", "machine", "host", "pseudo-fs", "spell-names", "list-all"}

// String is the set bits' short names joined by "|", empty for none, and any
// unnamed bits in hex, as golden files and logs render a profile.
func (c Caps) String() string {
	var out []string
	for i, n := range names {
		if c&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	if rest := c &^ (1<<len(names) - 1); rest != 0 {
		out = append(out, fmt.Sprintf("%#x", uint16(rest)))
	}
	return strings.Join(out, "|")
}
