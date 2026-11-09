package journal

import (
	"context"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
)

// Journal is one journal on one directory: the bytes of every share placed on
// the device it lives on. Each share works through its own handle.
//
// A Journal is built only by Open. Its methods are safe for concurrent use.
type Journal struct {
	core *datapath.Core

	// handlesMu guards handles. Only Share and Handle.Close take it, and no
	// lock domain is held when they do. Share holds it across the core's
	// attach, which takes only the core's registry mutex and short, I/O-free
	// locks of its own; Close takes it only after the core's Close returned.
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

// Init creates a new journal in dir, writing into its format file a fresh
// journal identity and install, the installation ID the same initialisation
// step records in the metadata store. It returns the identity, for the caller
// to record as the journal it expects in dir.
//
// Init reads no time and needs no clock. It is the only call that creates a
// journal, and only in an empty directory: it refuses, with ErrNotEmpty and
// writing nothing, a directory that holds any entry but a format.tmp, which
// an Init that crashed before its rename leaves. A negative setting is
// refused with ErrInvalidSetting.
func Init(ctx context.Context, dir string, install InstallationID, opts ...Option) (JournalID, error) {
	return JournalID{}, ErrNotImplemented
}

// Open opens the journal in dir, rebuilding its state from the segments on
// disk. It never creates one.
//
// expect is the identity the caller recorded for dir. With expect non-zero,
// Open fails with ErrNoJournal when dir holds no journal, empty, missing, or
// holding neither a format file nor a segment, and with an IdentityError
// when it holds another journal. A directory holding a segment and no format
// file is a journal Open cannot identify: it fails with ErrUnidentified and
// is left as it is. With expect zero,
// Open always fails: with an IdentityError naming the identity and
// installation ID it found, and whether any segment is present, or with
// ErrNoJournal. A format file of an unknown format version, or a record of an
// unknown kind or with a non-zero epoch, fails with ErrUnknownFormat; a
// format file whose checksum does not verify fails with ErrCorrupt.
//
// floor is the larger of the highest content version the caller has recorded
// for any file of the journal's shares and the smallest durable version limit
// among them. Every version the journal issues after Open exceeds both floor
// and every version on disk, so none issued before a crash whose record was
// lost is issued again.
//
// Open requires WithClock. Without it, Open fails with ErrNoClock and opens
// nothing.
func Open(ctx context.Context, dir string, floor Version, expect JournalID, opts ...Option) (*Journal, error) {
	return nil, ErrNotImplemented
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
	return ErrNotImplemented
}
