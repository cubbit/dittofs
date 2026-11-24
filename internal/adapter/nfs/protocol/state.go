package protocol

import (
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// State is one COMPOUND's state, which its members read and set in turn: the
// router builds it empty with the COMPOUND's request, as Request.State, and
// core carries it from member to member without reading it (RFC 20 section
// 4.7). No other call carries one.
//
// decision: the state holds the current filehandle and stateid, SAVEFH's
// copy of them, and the session's reply budget a SEQUENCE copies in (RFC 21
// sections 2.3 and 5.2). It holds no minor version, which is the request's
// Version, chosen per COMPOUND (RFC 21 section 2.1), and no session: the
// router builds the call's session reference, and a member that needs the
// session itself reaches it through the adapter's registry by that
// reference, one lookup per such member. Overturned if that lookup shows in a
// profile, which would put the session here.
type State struct {
	Cur, Saved Current // the current filehandle and stateid, and SAVEFH's copy
	MaxReply   int     // the session's ca_maxresponsesize, set by SEQUENCE; 0: no SEQUENCE ran
	MaxCached  int     // its ca_maxresponsesize_cached where sa_cachethis is set; 0: not cached
}

// Current is a current filehandle and the current stateid beside it (RFC 8881
// section 16.2.3.1.2). The filehandle may alias the request's record, which
// core retains for the request's life.
type Current struct {
	FH         vfs.Handle // nil: none
	Stateid    Stateid
	HasStateid bool
}

// Stateid is a stateid4 in its wire form, its sequence ID and its other
// field; an operation's own stateid4 converts to it.
type Stateid struct {
	Seqid uint32
	Other [12]byte
}

// SetFH makes fh r's current filehandle, the one way a member sets it: the
// current stateid is left unset, as an operation that changes the filehandle
// and returns no stateid leaves it (RFC 8881 section 16.2.3.1.2), and NeedsFH
// is raised in r.Level, whose other bits stay, so a row needing a current
// filehandle runs after it. r is a COMPOUND's request, whose State the router
// set.
func SetFH(r *core.Request, fh vfs.Handle) {
	r.State.(*State).Cur = Current{FH: fh}
	r.Level |= core.NeedsFH
}
