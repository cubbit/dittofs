package journal

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
)

// Journal is one journal on one directory: the bytes of every share placed on
// the device it lives on. Each share works through its own handle.
//
// A Journal is built only by Open. Its methods are safe for concurrent use.
type Journal struct {
	core *datapath.Core

	// closed is set by the first Close, so a later one answers ErrClosed
	// without closing the core again.
	closed atomic.Bool

	// handlesMu guards handles. Only Share and Handle.Close take it, and no
	// lock domain is held when they do. Share holds it across the core's
	// attach, which takes only the core's registry mutex and short, I/O-free
	// locks of its own; Close takes it only after the core's Close returned.
	//
	// decision: handlesMu is unranked: it sits outside the six lock domains,
	// so the lock-order recorder never sees it. It is not a leaf: Share
	// holds it across the core's attach, so the core's registry mutex and
	// the locks attach takes nest under it, while nothing the core takes is
	// ever held while this mutex is acquired. Withdraw the unranking if a
	// path ever takes handlesMu while it holds a ranked lock, or if the
	// core's attach ever calls back into the journal's Share or
	// Handle.Close.
	handlesMu sync.Mutex
	// handles is the one *Handle of each open *datapath.Share, so a second
	// Share of an open tag returns the handle the first one did.
	//
	// ponytail: a map under a mutex, one entry per open handle, removed by
	// Handle.Close. Shares are few and attach and close rarely, so a lookup
	// per Share costs nothing. Upgrade to a field on the core's share if the
	// public layer ever needs its handle on a hot path.
	handles map[*datapath.Share]*Handle
}

// newJournal returns the Journal that serves c.
func newJournal(c *datapath.Core) *Journal {
	return &Journal{core: c, handles: make(map[*datapath.Share]*Handle)}
}

// Restamp rewrites the installation ID in the format file of the journal in
// dir whose identity is expect, and changes nothing else. It is the one change
// to an existing journal's format file, made after a recovery import rebuilt
// the metadata store under a new installation ID. The file is replaced whole,
// never edited in place, so a crash leaves either the old file or the new one.
//
// Restamp fails as Open does when dir holds no journal or another one. It
// reads no time and no setting, so it ignores every option it is given.
func Restamp(ctx context.Context, dir string, expect JournalID, install InstallationID, opts ...Option) error {
	return ErrNotImplemented
}

// Close shuts the journal down. New operations through it or any of its
// handles fail with ErrClosed from the moment Close begins; operations already
// in flight complete or fail cleanly, and none observes a partly closed
// journal. Background work, such as reclamation passes and sealing idle
// segments, is stopped and joined before any segment it touches is closed,
// and no storage a reader may still hold is freed.
//
// Close returns once all of that has happened. Calling it again returns
// ErrClosed.
func (j *Journal) Close() error {
	if !j.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	if j.core == nil {
		// The zero Journal, which a test helper builds and a cleanup of its own
		// closes, owns nothing to shut down.
		return ErrClosed
	}
	j.handlesMu.Lock()
	open := make([]*Handle, 0, len(j.handles))
	for _, h := range j.handles {
		open = append(open, h)
	}
	j.handlesMu.Unlock()
	for _, h := range open {
		h.closing.Store(true)
		h.closer.CompareAndSwap(0, 2)
	}
	// decision: the core fences here, after every handle's closing flag is
	// up and before any handle is drained or its reservations released, so
	// from this step on no new call starts anywhere — neither through a
	// handle, whose share the fence closes, nor on the journal itself, whose
	// Draw, Share and SetVersionLimit refuse from the flag the fence raises.
	// Every handle of the snapshot is drained below through its own share, so
	// a call in flight on it is waited for before anything it holds is
	// released; a handle that attached past the snapshot, which only the
	// fence's own gate can let through, is collected by the fence into the
	// core's drain, which the last step waits for. The reservation release
	// still runs after the drains and before the core's Close, unchanged: a
	// reservation is in memory and named by no record, so nothing of it can
	// be recovered, and its bytes, entries, versions and spares would
	// otherwise stay held by a journal that is closing. The drain comes
	// first, so a reserved write in flight lands whole against a hold still
	// open and Release never runs beside the goroutine that owns the
	// reservation. Each handle's closing flag is set before its drain, so a
	// Reserve racing this close cannot register a reservation nothing
	// releases.
	//
	// decision: a share this drained is closed, and the handle's own Close
	// afterwards answers nil once, as it did when the journal's Close left the
	// share open, and ErrClosed after that: the claim above marks the handle for
	// it, made before the drain so a Close racing this one answers by who won,
	// never by when the drain finished. A handle whose own Close claimed it first
	// is waited for here and answers as a first Close does. Overturned by a
	// contract that has Handle.Close answer ErrClosed once its journal has
	// closed.
	j.core.BeginClose()
	for _, h := range open {
		_ = h.s.Close()
	}
	for _, h := range open {
		h.releaseAll()
	}
	return j.core.Close()
}
