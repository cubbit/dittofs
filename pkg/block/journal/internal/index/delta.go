package index

import (
	"slices"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// Transition is how one update moved its file across the line between
// holding a byte whose offloaded bit is unset, held or retained, and not:
// the dirty-file list's membership.
type Transition int8

const (
	Unchanged   Transition = iota // still dirty, or still clean
	BecameDirty                   // first unset bit, held or retained
	BecameClean                   // no held or retained byte with an unset bit left
)

// HeldDelta is the change one index update made to one file's held state:
// the account function's input. It is distinct from Change, Since's yield,
// which has no tag and no byte deltas. The zero HeldDelta means the update
// changed nothing.
//
// decision: the delta is measured from per-file counters the leaf functions
// keep (heldSet.put and cut, retainedSet.add and edit), inside the update's
// lock, rather than built by each operation or computed by a caller from a
// before and an after, because a caller or an operation that adjusts a figure
// itself can forget one. The leaf functions are the only places bytes enter
// or leave a set, so no operation can bypass them. Overturned if a set gains
// a write path that bypasses put, cut, add and edit.
type HeldDelta struct {
	// Tag is the file's owner when the update ended.
	//
	// decision: a share's tag is never 0, which marks the journal's own records. fill claims the file in
	// its own update, so no delta of a fill carries tag 0. Every other update
	// follows a claim of the journal call that made it, so a file that holds
	// bytes has an owner by then. Two updates skip it and return the zero tag
	// for a file nothing owns: a test's direct mutation, and an update on a
	// file a rebuild rebuilt from a retained piece alone, which has an entry
	// and no owner. Harmless while a rebuild's deltas are discarded; the
	// account function must not be handed one. Overturned by a journal call that changes a file's
	// bytes before claiming it: its delta would put those bytes under tag 0
	// and a later release through the share would drive its sum below zero.
	Tag           core.ShareTag
	File          core.FileID
	HeldBytes     int64 // change in the sum of held extent lengths
	DirtyBytes    int64 // change in held bytes whose offloaded bit is unset
	RetainedBytes int64 // change in the sum of retained extent lengths
	Dirty         Transition
	// OldestDirty is the write time of the file's oldest content whose
	// offloaded bit is unset, held or retained, as the update left it: a
	// state, not a change. It is zero when the file holds none, and zero for
	// content whose write time is unknown.
	//
	// decision: content a rebuild restores carries no write time, so a file
	// rebuilt dirty reports the zero time until recovery supplies the times.
	// Overturned if the dirty-file list is read for ageing before recovery
	// fills them in.
	OldestDirty time.Time
}

// dirtyTimes keeps, for one file, the write time of each version that still
// has a byte with an unset offloaded bit, held or retained, in ascending
// version order. A write's time is monotone in its version, so the head is
// the oldest dirty write. A version's entry is dropped by oldest once its last
// dirty byte has left; it is not dropped at the moment the count reaches zero,
// because an edit takes a piece out and puts its parts back within one call.
//
// ponytail: a sorted slice, O(versions with dirty bytes) to find, insert and
// prune per update. Upgrade to a time stored in the held entry only if the
// 40-byte entry bound moves, or to a heap if a file with many dirty versions
// shows this in a profile.
type dirtyTimes struct{ vers []verDirty }

type verDirty struct {
	counter uint64
	at      time.Time
	bytes   int64
}

// decision: find keys on the counter alone, which names a version because the
// epoch is always zero in this format: a record with another epoch is refused
// on encode and decode. Key on the whole version if an epoch is ever raised.
func (d *dirtyTimes) find(counter uint64) (int, bool) {
	return slices.BinarySearchFunc(d.vers, counter, func(e verDirty, c uint64) int {
		switch {
		case e.counter < c:
			return -1
		case e.counter > c:
			return 1
		}
		return 0
	})
}

// note records at as version counter's write time, keeping an earlier known one.
func (d *dirtyTimes) note(counter uint64, at time.Time) {
	if d == nil {
		return
	}
	i, ok := d.find(counter)
	if !ok {
		d.vers = slices.Insert(d.vers, i, verDirty{counter: counter, at: at})
	} else if d.vers[i].at.IsZero() {
		d.vers[i].at = at
	}
}

// add counts n dirty bytes of version counter, negative when they leave.
func (d *dirtyTimes) add(counter uint64, n int64) {
	if d == nil {
		return
	}
	i, ok := d.find(counter)
	if !ok {
		d.vers = slices.Insert(d.vers, i, verDirty{counter: counter})
	}
	d.vers[i].bytes += n
}

// oldest prunes the versions with no dirty byte and returns the head's time.
func (d *dirtyTimes) oldest() time.Time {
	d.vers = slices.DeleteFunc(d.vers, func(e verDirty) bool { return e.bytes <= 0 })
	if len(d.vers) == 0 {
		return time.Time{}
	}
	return d.vers[0].at
}

// heldTotals is one file's four per-set counters, read under its lock.
// retainedDirty counts toward the file's dirty membership only: DirtyBytes is
// held bytes alone.
type heldTotals struct{ held, dirty, retained, retainedDirty int64 }

func (f *file) totals() heldTotals {
	return heldTotals{f.held.bytes, f.held.dirty, f.retained.bytes, f.retained.dirty}
}

func (t heldTotals) isDirty() bool { return t.dirty > 0 || t.retainedDirty > 0 }

// delta returns the change from was to f's totals now, for id. It returns the
// zero HeldDelta, with no tag and no file, when no figure moved and the
// transition is Unchanged. The caller holds f's lock.
func (f *file) delta(id core.FileID, was heldTotals) HeldDelta {
	now := f.totals()
	d := HeldDelta{
		File:          id,
		HeldBytes:     now.held - was.held,
		DirtyBytes:    now.dirty - was.dirty,
		RetainedBytes: now.retained - was.retained,
		OldestDirty:   f.dirtyAt.oldest(),
	}
	switch {
	case !was.isDirty() && now.isDirty():
		d.Dirty = BecameDirty
	case was.isDirty() && !now.isDirty():
		d.Dirty = BecameClean
	}
	if d.HeldBytes == 0 && d.DirtyBytes == 0 && d.RetainedBytes == 0 && d.Dirty == Unchanged {
		return HeldDelta{}
	}
	d.Tag = f.tag
	return d
}
