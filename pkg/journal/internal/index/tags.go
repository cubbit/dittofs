package index

import (
	"bytes"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
)

// Conflict is one rivalled file: a file whose records, on disk, sit under two
// tags, which the running journal reaches only through a fault and a rebuild
// reaches from damaged records alone. TagConflicts returns one per such file,
// with Tags holding the owner's tag and the rival's, in that order.
type Conflict struct {
	File core.FileID
	Tags [2]core.ShareTag
}

// conflictSet is the set of rivalled files, so the conflict list costs the
// number of conflicts and never a walk of the files. Its members are told
// once, under the file's lock, at the transitions the rival mark makes: an
// update that marks a file rivalled, an update that clears the marks, and the
// settle that removes the entry with its marks still set.
//
// Its mutex is taken last and released before any other lock is acquired, so
// it sits at the end of the lock order the file map documents: the file's
// lock, then the shard's, then this one. No path holds it across a file's or
// a shard's lock, so a rebuild's workers marking files of different shards
// and a caller taking the list run beside each other without waiting on any
// file.
type conflictSet struct {
	// decision: mu is a leaf mutex outside the six lock domains: it is the
	// last lock taken, held only for a list edit, and released before any
	// other lock is acquired. Only mark and drop take it; a reader loads
	// view instead.
	mu sync.Mutex
	// view is the rivalled files in FileID byte order, an immutable slice
	// replaced whole by mark and drop under mu.
	//
	// ponytail: an edit copies the slice, O(rivalled files) per mark or drop.
	// Rivalled files come only from a fault and a rebuild over damaged
	// records, so the list stays short. Upgrade to a persistent tree when a
	// rebuild that marks thousands of files shows the copying in a profile.
	view atomic.Pointer[[]Conflict]
}

func (c *conflictSet) locate(list []Conflict, id core.FileID) (int, bool) {
	b := id.Bytes()
	return slices.BinarySearchFunc(list, b, func(x Conflict, b [16]byte) int {
		xb := x.File.Bytes()
		return bytes.Compare(xb[:], b[:])
	})
}

// mark records id as rivalled, owned by tag with rival marking its records.
func (c *conflictSet) mark(id core.FileID, owner, rival core.ShareTag) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var list []Conflict
	if v := c.view.Load(); v != nil {
		list = *v
	}
	i, found := c.locate(list, id)
	e := Conflict{File: id, Tags: [2]core.ShareTag{owner, rival}}
	var next []Conflict
	if found {
		next = slices.Clone(list)
		next[i] = e
	} else {
		next = slices.Insert(slices.Clone(list), i, e)
	}
	c.view.Store(&next)
}

// drop removes id from the set, whether or not it is a member.
func (c *conflictSet) drop(id core.FileID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.view.Load()
	if v == nil {
		return
	}
	i, found := c.locate(*v, id)
	if !found {
		return
	}
	next := slices.Delete(slices.Clone(*v), i, i+1)
	c.view.Store(&next)
}

// snapshot returns one Conflict per rivalled file, ordered by FileID bytes,
// a copy the caller owns. It takes no lock.
func (c *conflictSet) snapshot() []Conflict {
	if v := c.view.Load(); v != nil {
		return slices.Clone(*v)
	}
	return []Conflict{}
}

// CheckTag refuses a call through tag that names id, as every file-naming
// call's first step does — a read's as much as a write's — naming both tags
// when the file belongs to another share and whichever tag is asked through
// when the file is rivalled. It takes the file's lock shared, creates no
// entry and claims nothing, so a read may run it; a file with no entry is
// nobody's and checks clean.
func (ix *Index) CheckTag(tag core.ShareTag, id core.FileID) error {
	var err error
	ix.fs.view(id, func(f *file) { err = f.check(tag, id) })
	return err
}

// Owner returns the tag that owns id, and false when none does or the file has
// no entry. It takes the file's lock shared and creates no entry.
func (ix *Index) Owner(id core.FileID) (tag core.ShareTag, ok bool) {
	ix.fs.view(id, func(f *file) { tag, ok = f.owner() })
	return tag, ok
}

// TagConflicts returns the rivalled files, one Conflict per file ordered by
// FileID bytes, in time proportional to their number. It takes no file's
// lock. A rivalled file stays a member until one of its two tags is cleared
// or the last record of it leaves the disk, so the list is the read-out a
// caller consults to name the files a Stats call answers for.
func (ix *Index) TagConflicts() []Conflict { return ix.fs.conflicts.snapshot() }

// ClearTag drops id's owner and rival marks whole in memory, leaving the
// file's records on disk as they are: they still carry their tags, so a
// rebuild over them re-creates both the file's owner and, for a rivalled
// file, its conflict. In the running index the file is nobody's again, so
// the next appending call through any tag claims it afresh. A file with no
// entry is a no-op.
//
// It is the stand-in the tests use for forgetting a share's tag, which is
// narrower: a real forget of one of a rivalled file's two tags must leave
// the file owned by the other tag, not nobody's, and must be made durable —
// clear the marks in memory alone and a rebuild restores them as soon as it
// replays the records still naming both tags.
//
// decision: only tests call ClearTag today, both the index's own and the
// datapath's tag tests. Overturned when a Forget(tag) caller lands and takes
// over what the tests exercise by hand.
func (ix *Index) ClearTag(id core.FileID) error {
	_, err := ix.fs.update(id, func(f *file) error {
		f.tagged, f.tag = false, 0
		f.rivalled, f.rival = false, 0
		// Both marks drop together, so the pieces' side flags — the rival
		// mark's alone — re-own with them, keeping a later claim and rivalry
		// from resolving stale flags to a rival their pieces never had.
		f.held.ownAll()
		return nil
	})
	return err
}

// FilesClaimedBy returns every file tag claims, whether as its owner or as
// the rival of one: the enumeration the Forget of the tag drops by. Unlike
// Files, a file nothing holds — every extent settled away, its records still
// riding their segments — is named by its tag alone, and a rivalled file is
// named by its rival's tag too, whichever side the Forget has to clear. It
// takes no file's lock; the answer is in no order and a snapshot.
func (ix *Index) FilesClaimedBy(tag core.ShareTag) []core.FileID {
	var out []core.FileID
	ix.fs.walk(func(id core.FileID, f *file) {
		if !f.gone && ((f.tagged && f.tag == tag) || (f.rivalled && f.rival == tag)) {
			out = append(out, id)
		}
	})
	return out
}

// ForgetFile drops the file whole from the running index, the half a durable
// forget record of the file's tag does at its drop: every held and retained
// extent, every removal marker, and the drop ranges of the records below
// seq — those are the ones the forget record covers, so no drop query of the
// file is answered against them — while the entry's marks are left alone.
// It returns the held-state delta of the drop and the segments whose held
// bytes fell, so the caller accounts the drop and offers the emptied
// segments with the forget record as the causality record, exactly as a
// removal does. A file with no entry drops nothing and names no segment.
//
// The marks are the second half, ForgetTagMarks: the delta of this update
// names the tag as the update ended, so the drop must run first, while the
// entry still owns its tag, and the marks are cleared once the drop is
// accounted.
//
// decision: the markers and the drop ranges are cleared whole, the survivor's
// with the forgotten tag's. They are kept per file — the index cannot tell a
// marker's tag — while the pieces it can tell, so the residue a rivalled
// file's survivor carries is its marker yielding to the forget's settle-free
// clear and the drop ranges of its records below the forget record: its
// removals can settle sooner over the lost ranges, and its own removal
// records release at the liveness re-test the forget runs. Safe everywhere a
// rivalled file grows — only a fault plants one — and cheaper than a stored
// tag per piece: the entry keeps one byte of side, not eight of tag.
// Overturned by a rival whose unsettled markers must survive a forget of its
// co-tenant exact.
func (ix *Index) ForgetFile(id core.FileID, below core.Seq, tag core.ShareTag) (HeldDelta, []core.SegmentID, error) {
	var m touched
	hd, err := ix.fs.update(id, func(f *file) error {
		f.held.touched = &m
		defer func() { f.held.touched = nil }()
		// The drop takes the tag's records alone: the entry keeps a one-byte
		// side flag that resolves to the full tag against the file's own
		// marks, so the other side of a rivalled file keeps its held pieces
		// — and with them the segment its records ride, held by the same
		// bytes the drop still names — and nothing of the surviving tag's
		// content is reclaimed under it. A file the forgotten tag owns whole
		// (no rival mark) takes the cut it always did.
		all := toEnd(0)
		switch {
		case !f.rivalled:
			f.held.cut(all)
		case f.rival == tag:
			f.held.dropSide(true)
		default:
			f.held.dropSide(false)
		}
		f.retained.drop(all, func(r Retained) bool { return r.Tag == tag }, nil)
		f.markers = nil
		f.forgetRanges(below)
		return nil
	})
	return hd, m.named(), err
}

// ForgetTagMarks clears the marks of the forgotten tag on id, the second
// half of a forget the durable record settles: the file goes to the other
// tag of a rivalled one — its records of the forgotten tag are the forget's
// own, and a reopen rebuilds the marks of the records that remain — and to
// nobody otherwise, so the next appending call through any tag claims the
// file afresh, as recovery would. A file the forgotten tag never marked, and
// one with no entry, are no-ops.
func (ix *Index) ForgetTagMarks(id core.FileID, forgotten core.ShareTag) error {
	_, err := ix.fs.update(id, func(f *file) error {
		switch {
		case f.rivalled && f.rival == forgotten:
			f.rivalled, f.rival = false, 0
			// What remains is the owner's alone; the pieces the rival's own
			// forget already dropped never read a flag again, and any flag a
			// standalone marks-clear leaves would misname the next rivalry.
			f.held.ownAll()
		case f.rivalled && f.tag == forgotten:
			f.tag, f.rivalled, f.rival = f.rival, false, 0
			// The survivor's pieces are the owner's now: their side flags
			// must say so before any later rivalry resolves them to its
			// rival and a forget of it drops them with the rival's own.
			f.held.ownAll()
		case f.tag == forgotten:
			f.tagged, f.tag = false, 0
		}
		return nil
	})
	return err
}

// conflictDiff settles the conflict set's side of the update whose apply
// holds the file's lock: a file the update marked rivalled, with neither mark
// before it, joins the set naming both tags, and one the update left without
// the marks leaves it. It runs after the update's settle, still under the
// file's lock, so the set and the entry agree about the file once the update
// returns. A settle that removed the entry whole — with the marks or without
// them — has already taken the file back out, and the gone mark it left says
// so; a file a single update marked rivalled in the same breath as emptying
// its entry therefore leaves no trace, exactly as its absence says.
func (f *file) conflictDiff(fs *files, id core.FileID, was bool) {
	if f.gone {
		return
	}
	switch {
	case !was && f.rivalled:
		fs.conflicts.mark(id, f.tag, f.rival)
	case was && !f.rivalled:
		fs.conflicts.drop(id)
	}
}
