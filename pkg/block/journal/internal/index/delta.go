package index

import (
	"cmp"
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
	// content whose write time is unknown. Times are kept in buckets of one
	// second, so it can overstate the oldest write's age by under a second and
	// never understates it.
	//
	// decision: content a rebuild restores carries no write time, so a file
	// rebuilt dirty reports the zero time until recovery supplies the times.
	// Overturned if the dirty-file list is read for ageing before recovery
	// fills them in.
	OldestDirty time.Time
}

// dirtySpan is the width of one dirty-time bucket: a write opens a new bucket
// only when its time is at least this long after the newest bucket's.
const dirtySpan = time.Second

// dirtyTimes keeps, for one file, the dirty bytes still held or retained
// (offloaded bit unset) in buckets of write time, in ascending version order.
// A write's time is monotone in its version, so a version belongs to the last
// bucket whose first counter is at or below it, and the head bucket holds the
// oldest dirty write. A bucket's at is the earliest write time in it.
//
// The head is pruned only by oldest, once its last dirty byte has left, and
// never at the moment a count reaches zero, because an edit takes a piece out
// and puts its parts back within one call. An emptied bucket in the middle is
// left, as it costs nothing but its slot.
//
// decision: a version's time is that of its bucket, so oldest can overstate the
// age of the oldest dirty write by under dirtySpan and never understates it:
// the bucket's time is at or before every write in it. Overturned by a reader
// that needs the exact time of one write.
//
// ponytail: the table holds one bucket per dirtySpan of dirty backlog, so its
// memory is bounded by the seconds a file stays dirty, not by its versions,
// and an update costs a binary search over those buckets. Upgrade to an exact
// per-piece time stored in the held entry if the 40-byte entry bound ever
// widens, or finer buckets if a reader needs sub-second ages.
type dirtyTimes struct{ vers []dirtyBucket }

type dirtyBucket struct {
	first uint64 // the lowest counter that opened this bucket
	at    time.Time
	bytes int64
}

// decision: buckets key on the counter alone, which names a version because
// the epoch is always zero in this format: a record with another epoch is
// refused on encode and decode. Key on the whole version if an epoch is ever
// raised.
//
// floor returns the bucket counter belongs to: the last one whose first is at
// or below it, or the first when counter is older than all, so an older
// version re-held by a failed window folds into the oldest bucket. The table
// must not be empty.
func (d *dirtyTimes) floor(counter uint64) int {
	i, _ := slices.BinarySearchFunc(d.vers, counter, func(b dirtyBucket, c uint64) int { return cmp.Compare(b.first, c) })
	// i is the first bucket not below counter; an exact match is its floor.
	if i < len(d.vers) && d.vers[i].first == counter {
		return i
	}
	return max(i-1, 0)
}

// note records at as version counter's write time: it opens a bucket when at
// lies a span or more past the newest bucket's time, and otherwise leaves the
// version in the bucket it falls in, filling in a time that bucket lacks.
func (d *dirtyTimes) note(counter uint64, at time.Time) {
	if d == nil {
		return
	}
	n := len(d.vers)
	if n == 0 || (counter > d.vers[n-1].first && at.Sub(d.vers[n-1].at) >= dirtySpan) {
		d.vers = append(d.vers, dirtyBucket{first: counter, at: at})
		return
	}
	if i := d.floor(counter); d.vers[i].at.IsZero() {
		d.vers[i].at = at
	}
}

// add counts n dirty bytes of version counter, negative when they leave.
func (d *dirtyTimes) add(counter uint64, n int64) {
	if d == nil {
		return
	}
	if len(d.vers) == 0 {
		d.vers = append(d.vers, dirtyBucket{first: counter})
	}
	d.vers[d.floor(counter)].bytes += n
}

// oldest prunes the head buckets with no dirty byte and returns the new head's
// time, zero when none is left. It is amortised O(1) per bucket opened.
func (d *dirtyTimes) oldest() time.Time {
	k := 0
	for k < len(d.vers) && d.vers[k].bytes <= 0 {
		k++
	}
	if k == len(d.vers) {
		d.vers = d.vers[:0]
		return time.Time{}
	}
	d.vers = d.vers[k:]
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
