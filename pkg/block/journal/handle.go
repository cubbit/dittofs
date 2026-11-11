package journal

import (
	"context"
	"iter"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/datapath"
)

// Handle is one share's view of the journal. Every call through it that names
// a file is refused with an OtherShareError, and serves, appends, changes and
// offers nothing of that file, when the file belongs to another share's tag;
// a call that appends a record of a file with no tag claims the file for the
// handle's tag. After Close, every call fails with ErrClosed.
//
// The lock domains named below are taken in this order and no other: file
// append, capacity, file index, segment append, storage guard. A handle's
// methods are safe for concurrent use.
type Handle struct {
	j *Journal
	s *datapath.Share

	// closing is set by the first Close, before it drains the calls in flight,
	// so a Reserve that already left the share cannot add a reservation the
	// close would never release. It is read by Reserve after it records one.
	closing atomic.Bool

	// drained is set when the journal's Close drained the share and released
	// the reservations, so the first Close of the handle after it has nothing
	// left to drain and answers nil, as a first Close does.
	drained atomic.Bool

	// resMu guards res, the handle's open reservations, so Close returns what
	// each still holds. It is a leaf, taken with no lock domain held and never
	// across one.
	resMu sync.Mutex
	res   map[*Reservation]struct{}
}

// What a Change reports; see ChangeKind.
const (
	// ChangeWrite is a held extent, written or filled.
	ChangeWrite = core.ChangeWrite
	// ChangeTruncate is the marker of a Truncate.
	ChangeTruncate = core.ChangeTruncate
	// ChangeDeallocate is the marker of a Deallocate.
	ChangeDeallocate = core.ChangeDeallocate
	// ChangeDelete is the marker of a Delete.
	ChangeDelete = core.ChangeDelete
	// ChangeCloneTarget is the marker of a CloneTarget; Change.Clone is its
	// spec.
	ChangeCloneTarget = core.ChangeCloneTarget
	// ChangeLoss is a synced write the journal lost: the range reads as lost
	// rather than as the file was before the write.
	ChangeLoss = core.ChangeLoss
)

// Close ends the handle. New calls through it fail with ErrClosed, and Close
// returns once every call already in flight on it has returned. The handle's
// extents stay held, and the reservations still open through it are released,
// so none of their bytes, entries, versions or spares outlives the handle. A
// second or concurrent Close, such as another owner's of a handle Share
// returned twice, also waits for those calls and then returns ErrClosed.
func (h *Handle) Close() error {
	// The closing flag is set before the share is drained, so a Reserve that
	// left the share already cannot add a reservation after the release below
	// walks them: it records its own, sees the flag and releases it.
	h.closing.Store(true)
	if h.drained.CompareAndSwap(true, false) {
		return nil
	}
	if err := h.s.Close(); err != nil {
		return err
	}
	h.releaseAll()
	h.j.handlesMu.Lock()
	delete(h.j.handles, h.s)
	h.j.handlesMu.Unlock()
	return nil
}

// releaseAll releases every reservation the handle still holds open.
func (h *Handle) releaseAll() {
	h.resMu.Lock()
	open := make([]*Reservation, 0, len(h.res))
	for r := range h.res {
		open = append(open, r)
	}
	clear(h.res)
	h.resMu.Unlock()
	for _, r := range open {
		r.r.Release()
	}
}

// release returns whether it released r, which it registers under the handle
// for Close to find. It answers false for a reservation already released by
// its own Close, by this handle's or by the journal's.
func (h *Handle) release(r *Reservation) bool {
	h.resMu.Lock()
	if _, open := h.res[r]; !open {
		h.resMu.Unlock()
		return false
	}
	delete(h.res, r)
	h.resMu.Unlock()
	r.r.Release()
	return true
}

// track records r as one of the handle's open reservations. It answers false,
// releasing r, when the handle's close has begun: the reservation was taken
// through the share before it closed, and Close's releaseAll has either
// already run or runs before it finds this one.
func (h *Handle) track(r *Reservation) bool {
	h.resMu.Lock()
	defer h.resMu.Unlock()
	if h.closing.Load() {
		return false
	}
	if h.res == nil {
		h.res = make(map[*Reservation]struct{})
	}
	h.res[r] = struct{}{}
	return true
}

// Reserve takes, at once, everything needed by the given number of writes
// carrying n record bytes in total, so an operation that must not fail
// part-way finds every refusal before it changes anything: the bytes against
// the share's limit and the journal's maximum, index entries against the
// extent limit, one version per write below the version limit, and the spare
// segments its bytes roll into. The reservation
// holds them until its Close.
//
// n is record bytes: what the writes append — each record's header, body,
// padding and payload — not the payload bytes alone. A caller that sizes n by
// the payload it copies is short of each write's record overhead, and its last
// write is refused ErrReservationExhausted rather than the ask covering it.
//
// A refusal is a NoSpaceError naming LimitShare, LimitJournal, LimitIndex,
// LimitVersion or LimitSpare, returned at once, without waiting and without
// I/O. Reserve takes the capacity domain. A reservation is kept in memory: a
// restart drops it.
//
// An ask of no bytes or no writes names no operation that could exist, so it
// is refused with ErrInvalidExtent, holding nothing. A handle whose close began
// refuses Reserve with ErrClosed, and one in flight holds that close back; a
// reservation open through the handle when it closes is released, and its later
// calls fail with ErrClosed.
func (h *Handle) Reserve(n int64, writes int) (*Reservation, error) {
	// decision: a zero Handle, built by a test over a zero Journal, refuses
	// Reserve with ErrNotImplemented instead of panicking on the core it does
	// not have. That is Share's guard one method over, and holds for the same
	// reason: no caller holds a zero value past building it, and the engine's
	// tests build one to see a refusal answer a nil reservation. Overturned by
	// any caller that calls Reserve on a zero value for another reason, or by
	// Open making zero values unreachable, when the guard can go.
	if h.j == nil || h.j.core == nil || h.s == nil {
		return nil, ErrNotImplemented
	}
	r, err := h.j.core.Reserve(context.Background(), h.s, n, writes)
	if err != nil {
		return nil, err
	}
	res := &Reservation{h: h, r: r}
	if !h.track(res) {
		r.Release()
		return nil, ErrClosed
	}
	return res, nil
}

// WriteAt stages p at offset off of file id, with mtime, the modification time
// the write sets in nanoseconds since the Unix epoch, or 0 when it is
// suspended. On success the extent is held and dirty, and every later read
// serves it wherever its version is newer than what was held. It is written
// before WriteAt returns but durable only once a Sync names the file, or
// within SyncBound, whichever comes first.
//
// WriteAt draws a new content version and returns it. It reserves its record
// bytes before accepting them; a refusal is a NoSpaceError naming LimitShare,
// LimitJournal, LimitIndex, LimitVersion or LimitSpare, returned at once with
// no byte written. It does not draw on the headroom, but once less than half
// the headroom remains it is refused with a NoSpaceError naming
// LimitHeadroom, so what remains serves the records that return space. A
// write through a Reservation is exempt.
//
// It takes the file append domain, in which its version is assigned, then
// capacity, the file index and the segment's append domain.
func (h *Handle) WriteAt(ctx context.Context, id FileID, off int64, p []byte, mtime int64) (Version, error) {
	return h.j.core.WriteAt(ctx, h.s, id, off, p, mtime)
}

// Sync returns once every operation on the named files that returned before
// the call is durable. A failed sync window returns a SyncError naming the
// files whose writes it lost; a later successful sync never stands in for
// them. Sync holds no lock across the sync itself.
func (h *Handle) Sync(ctx context.Context, ids ...FileID) error {
	return ErrNotImplemented
}

// ReadAt fills p from the extents of file id the journal holds, starting at
// off. Every part of (off, len(p)) the journal does not hold is reported in
// missing, exactly, and the matching bytes of p are left untouched: they are
// never zeroed, and a missing extent never shows as a short read. The journal
// does not say why an extent is missing. held names the content version of
// each extent served, and asOf is the file's change version at the read,
// which a Fill of a missing extent passes back.
//
// ReadAt draws no version and appends nothing. It reads asOf under the file
// append domain, then resolves the extents under the file index domain held
// shared, and reads from storage with neither held. A record that does not
// verify goes to the corruption rule: it is served from a copy that verifies,
// or dropped and its extents reported missing, whether its payload, its
// header, or the record its header names is what fails. An I/O error is
// returned wrapped. On an error n and held still describe what was copied
// into p.
func (h *Handle) ReadAt(ctx context.Context, id FileID, off int64, p []byte) (n int, missing []Extent, held []Held, asOf Version, err error) {
	return h.j.core.ReadAt(ctx, h.s, id, off, p)
}

// Fill places bytes fetched from the remote tier into the parts of
// (off, len(p)) of file id the journal does not hold, and never overwrites a
// held extent. The filled extents are marked offloaded, since the bytes came
// from the remote tier, so the caller passes only bytes it fetched for exactly
// that extent of that file. v is the content version of the ref the bytes were
// fetched from; Fill draws no version.
//
// Fill is not a write path, and it is a separate method from WriteAt so that
// neither can be taken for the other: a write is newer than what is held and
// owes an offload, a fill is older and is offloaded already.
//
// Fill is refused with ErrStaleFill, writing nothing, when the file changed
// after asOf, the change version ReadAt returned. It reserves its record bytes
// as WriteAt does, refused by a NoSpaceError naming LimitShare, LimitJournal,
// LimitIndex or LimitSpare, and, like WriteAt, by one naming LimitHeadroom
// once less than half the headroom remains. Its absence check and its write are one step in
// the file append domain, atomic with respect to WriteAt on the same file.
func (h *Handle) Fill(ctx context.Context, id FileID, off int64, p []byte, asOf Version, v Version) error {
	return h.j.core.Fill(ctx, h.s, id, off, p, asOf, v)
}

// Truncate stops holding every extent of file id at or beyond size, and
// narrows an extent straddling it. It draws a new version, leaves a removal
// marker until the caller settles it, and is durable on return. A removal
// whose durability wait fails returns no version, as a failed write does: a
// read-only journal undoes the unsynced record, so the removal did not happen.
//
// Its record draws on the reserved headroom and is never refused by the
// share's limit or the journal's maximum. It is refused by a NoSpaceError
// naming LimitHeadroom once the headroom is exhausted — the ledger is never
// credited back before repack, so the refusal can reach a removal of held
// content too — or naming LimitVersion. A negative size is refused with
// ErrInvalidExtent before anything is drawn or appended. It takes the file
// append domain.
func (h *Handle) Truncate(ctx context.Context, id FileID, size int64) (Version, error) {
	return h.j.core.Truncate(ctx, h.s, id, size)
}

// Deallocate stops holding extent e of file id, whatever its offloaded bit. It
// draws a new version, leaves a removal marker until the caller settles it,
// and is durable on return; a failed durability wait returns no version. A
// zero-length e is refused with ErrZeroLength and appends nothing; a negative
// offset or length, or an end that overflows, with ErrInvalidExtent, likewise
// before anything is drawn or appended.
//
// It draws on the headroom as Truncate does, and is refused as Truncate is or
// by a NoSpaceError naming LimitIndex when splitting a held extent would pass
// the index's cap on what removals add. It takes the file append domain.
func (h *Handle) Deallocate(ctx context.Context, id FileID, e Extent) (Version, error) {
	return h.j.core.Deallocate(ctx, h.s, id, e)
}

// Delete stops holding every extent of file id. It draws a new version, leaves
// a removal marker until the caller settles it, and is durable on return; a
// failed durability wait returns no version. It draws on the headroom and is
// refused as Truncate is. It takes the file append domain.
func (h *Handle) Delete(ctx context.Context, id FileID) (Version, error) {
	return h.j.core.Delete(ctx, h.s, id)
}

// CloneTarget stops holding (spec.DstOff, spec.Len) of file id as Deallocate
// does, and its record and marker carry spec, so a restart that finds the
// marker resumes the clone rather than leaving the range reading as a hole. A
// zero-length spec is refused with ErrZeroLength and appends nothing, and one
// whose source or destination cannot be named with ErrInvalidExtent: a
// negative destination offset or length, or an end that overflows; a negative
// SrcOff or one whose end overflows Len; or an AsOf carrying an epoch. Nothing
// is drawn or appended for either.
//
// It draws a new version and is durable on return; a failed durability wait
// returns no version. Its record frees nothing, so besides Deallocate's
// refusals it is refused with a NoSpaceError naming LimitHeadroom once less
// than half the headroom remains. It takes the file append domain.
func (h *Handle) CloneTarget(ctx context.Context, id FileID, spec CloneSpec) (Version, error) {
	return h.j.core.CloneTarget(ctx, h.s, id, spec)
}

// Since yields every held extent of file id, every unsettled removal marker
// and every synced loss whose version is strictly above from, in version
// order; of one version, markers come before extents, then extents in offset
// order. It reads a consistent view of the file and blocks writes to it no
// longer than a read does: the view is copied under the file's lock and the
// records are read after it is released. An unsynced loss is not yielded, and
// neither is a Forget or a loss that reports a superseded write.
//
// A write carries the modification time its record holds, read from the
// record and verified as a read verifies it, and the runs over which it
// superseded another write above from. A record that does not verify goes to
// the corruption rule: its extent is yielded when a copy serves it, and when
// the record is dropped a synced one is yielded as a loss in its place. A
// segment retired meanwhile, or a record found moved after it was dropped from
// its old place, sends the read on to the record the index now names for each
// piece of the extent, a bounded number of times, and leaves out a piece the
// index no longer holds; a record that keeps moving ends the
// iteration with an error rather than being skipped. A cancelled ctx ends the
// iteration with its error; an I/O error ends it with that error.
//
// decision: Since has no unread-file state to refuse on: the journal records
// none yet, so ErrUnreadable cannot be yielded, and a file whose records do
// not verify is handled by the corruption rule above. Withdraw this once an
// open can leave a file unread, which Since must then answer with
// ErrUnreadable and nothing else.
func (h *Handle) Since(ctx context.Context, id FileID, from Version) iter.Seq2[Change, error] {
	return h.j.core.Since(ctx, h.s, id, from)
}

// Settle states that metadata has recorded every removal and every synced loss
// of file id at or below v, and drops those markers and the superseded runs
// kept for them, so a write's Over at or below v is dropped too. It never drops
// a held extent, appends nothing and is not persisted: recovery rebuilds the
// markers, and the caller settles again. Settling before the change is
// recorded is the caller's error.
func (h *Handle) Settle(id FileID, v Version) { h.j.core.Settle(h.s, id, v) }

// Extents returns the extents of file id the journal holds, in offset order.
// It says what the journal holds, not what exists: a caller answering where
// data or holes lie combines it with metadata, or it reports evicted content
// as a hole. Pieces of one record that differ only in their offloaded bit
// come back as one extent.
func (h *Handle) Extents(id FileID) ([]Extent, error) {
	return h.j.core.Extents(h.s, id)
}

// Files yields the share's files that hold an extent, an unsettled removal
// marker or an unsettled synced loss.
func (h *Handle) Files() iter.Seq[FileID] {
	return slices.Values(h.j.core.Files(h.s))
}

// DirtyFiles yields the share's files holding an extent whose offloaded bit is
// unset, each with the write time of its oldest dirty byte. It is served from
// a list the journal maintains, costs O(dirty files), and neither walks the
// placement index nor reads a record. The order is unspecified.
//
// The iteration runs over a copy taken when DirtyFiles is called, so a caller
// may write or release while ranging and sees none of it. A file whose write
// time is not known, as after a rebuild, carries the zero time.
func (h *Handle) DirtyFiles() iter.Seq2[FileID, time.Time] {
	files := h.j.core.DirtyFiles(h.s)
	return func(yield func(FileID, time.Time) bool) {
		for _, f := range files {
			if !yield(f.File, f.Oldest) {
				return
			}
		}
	}
}
