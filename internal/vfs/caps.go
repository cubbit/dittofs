package vfs

import "github.com/cubbit/dittofs/pkg/vfs"

// Each capability the service's own rules read is read here, once, so a
// search for its name finds every rule that depends on it. Open state reads
// the rest through its Caller.

// privilegeOnEveryCall reports whether c's privileged caller is privileged on
// every call, rather than only through a backup-intent open.
func privilegeOnEveryCall(c vfs.Call) bool { return c.Caps.Has(vfs.PrivilegeOnEveryCall) }

// hostCall reports whether c comes from a host's lock or mount service,
// which admission's lowered minimums read.
func hostCall(c vfs.Call) bool { return c.Caps.Has(vfs.HostCall) }

// seesPseudoFS reports whether c's client sees a pseudo-filesystem above
// the shares, which a lookup of ".." at a share's root goes up to.
func seesPseudoFS(c vfs.Call) bool { return c.Caps.Has(vfs.SeesPseudoFS) }

// clientIsMachine reports whether the client c registers is a whole
// machine, whose record names no owner.
func clientIsMachine(c vfs.Call) bool { return c.Caps.Has(vfs.ClientIsMachine) }
