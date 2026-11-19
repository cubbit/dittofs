package datapath

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
)

// DirtyFile is one file of the dirty-file list: a file holding an extent,
// held or retained, whose offloaded bit is unset, with the write time of its
// oldest such content. The time is zero when the content's write time is not
// known, as for content a rebuild restored.
type DirtyFile struct {
	File   core.FileID
	Oldest time.Time
}

// dirtyList is the core's list of dirty files. Account keeps it current from
// the index's deltas, so a read of it touches neither the index nor a record.
//
// decision: mu is a leaf: one mutex guards the map and nothing else is taken
// while it is held: not a file's append domain, the index, a segment or any
// I/O. Account runs under a file's domain and from the loss path, so a lock
// that waited on either would invert the order; a leaf cannot. Overturned if a caller ever
// needs the list and a file's state read as one step.
//
// ponytail: DirtyFiles copies the share's entries under the mutex on every
// call, O(dirty files across all shares) time and O(the share's dirty files)
// memory. Upgrade to a per-share map, or a snapshot swapped in on change,
// when a periodic reader's copy shows in a profile or the shares' dirty sets
// differ enough in size that one share's scan pays for another's.
type dirtyList struct {
	mu sync.Mutex
	m  map[core.FileID]dirtyEntry
	// byTime counts the listed files by the whole second of their oldest dirty
	// time (Unix seconds, rounded down). Files with an unknown (zero) time are
	// not counted.
	byTime map[int64]int
	// keys is a min-heap of the seconds byTime holds, with lazy deletion: a
	// second is pushed when byTime first holds it and popped from the top
	// only once byTime no longer does, so the oldest is the top after the
	// stale ones are discarded. inHeap names the seconds keys holds, so a
	// second that byTime dropped and holds again while its stale entry is
	// still in the heap is not pushed twice. The heap is rebuilt from byTime
	// when stale entries outnumber live ones, which bounds it to a small
	// multiple of byTime.
	//
	// ponytail: the rebuild is O(live seconds) under mu, which every publish
	// takes, and it runs once per as many drops as there are live seconds, so
	// it amortises to O(1) per drop; one publish pays the whole pass. Upgrade
	// to a per-second ordered structure with no rebuild when a backlog of
	// hundreds of thousands of seconds shows that one slow publish.
	keys   secHeap
	inHeap map[int64]struct{}
	// oldest is the second of the oldest counted file with its sign bit
	// flipped, so the zero value of a list reads as noOldest: none counted. It
	// is replaced under mu whenever the heap's top changes, and read without
	// it.
	//
	// ponytail: the value keeps the whole-second resolution byTime has, so it
	// can overstate the oldest write's age by under a second and never
	// understates it. Upgrade to the exact time when a consumer needs
	// sub-second age, which byTime would then have to key by nanosecond.
	oldest atomic.Int64
}

// noOldest is the oldest value of a list that counts no file with a known
// time. No Unix second a clock reads equals it.
const noOldest = math.MinInt64

// oldestBits flips the sign bit between a second and the form oldest stores.
const oldestBits = math.MinInt64

// secHeap is a min-heap of Unix seconds.
type secHeap []int64

func (h *secHeap) push(k int64) {
	*h = append(*h, k)
	s := *h
	for i := len(s) - 1; i > 0; {
		p := (i - 1) / 2
		if s[p] <= s[i] {
			break
		}
		s[p], s[i] = s[i], s[p]
		i = p
	}
}

// down sifts the entry at i toward the leaves.
func (h secHeap) down(i int) {
	n := len(h)
	for {
		l, r, m := 2*i+1, 2*i+2, i
		if l < n && h[l] < h[m] {
			m = l
		}
		if r < n && h[r] < h[m] {
			m = r
		}
		if m == i {
			return
		}
		h[i], h[m] = h[m], h[i]
		i = m
	}
}

func (h *secHeap) pop() {
	s := *h
	n := len(s) - 1
	s[0] = s[n]
	*h = s[:n]
	h.down(0)
}

func (l *dirtyList) count(t time.Time, n int) {
	if t.IsZero() {
		return
	}
	if l.byTime == nil {
		l.byTime = make(map[int64]int)
	}
	k := t.Unix()
	before := l.byTime[k]
	l.byTime[k] = before + n
	if l.byTime[k] == 0 {
		delete(l.byTime, k)
	} else if _, queued := l.inHeap[k]; !queued {
		if l.inHeap == nil {
			l.inHeap = make(map[int64]struct{})
		}
		l.inHeap[k] = struct{}{}
		l.keys.push(k)
	}
}

// settleOldest discards the heap's stale tops, rebuilds the heap when stale
// entries dominate, and publishes the oldest second. The caller holds mu.
func (l *dirtyList) settleOldest() {
	if len(l.keys) > 2*len(l.byTime)+16 {
		l.keys = l.keys[:0]
		clear(l.inHeap)
		for k := range l.byTime {
			l.keys = append(l.keys, k)
			l.inHeap[k] = struct{}{}
		}
		for i := len(l.keys)/2 - 1; i >= 0; i-- {
			l.keys.down(i)
		}
	}
	for len(l.keys) > 0 {
		if _, live := l.byTime[l.keys[0]]; live {
			break
		}
		delete(l.inHeap, l.keys[0])
		l.keys.pop()
	}
	if len(l.keys) == 0 {
		l.oldest.Store(noOldest ^ oldestBits)
		return
	}
	l.oldest.Store(l.keys[0] ^ oldestBits)
}

// oldestDirty returns the start of the second of the oldest dirty write of any
// file, so it can overstate that write's age by under a second and never
// understates it, or the zero time when no listed file has a known one. It is
// one atomic load of the value apply maintains.
func (c *Core) oldestDirty() time.Time {
	if k := c.dirtyFiles.oldest.Load() ^ oldestBits; k != noOldest {
		return time.Unix(k, 0)
	}
	return time.Time{}
}

type dirtyEntry struct {
	tag    core.ShareTag
	oldest time.Time
}

// apply folds one update's outcome into the list: a file that became dirty is
// added, one that became clean is removed, and a listed file takes the oldest
// time the update left it.
func (l *dirtyList) apply(d index.HeldDelta) {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer l.settleOldest()
	old, listed := l.m[d.File]
	switch {
	case d.Dirty == index.BecameClean:
		if listed {
			l.count(old.oldest, -1)
		}
		delete(l.m, d.File)
	case d.Dirty == index.BecameDirty || listed:
		if l.m == nil {
			l.m = make(map[core.FileID]dirtyEntry)
		}
		if listed {
			l.count(old.oldest, -1)
		}
		l.count(d.OldestDirty, 1)
		l.m[d.File] = dirtyEntry{tag: d.Tag, oldest: d.OldestDirty}
	}
}

// DirtyFiles returns a copy of the files of s's share that hold content whose
// offloaded bit is unset, in no order. It reads the list the core maintains.
func (c *Core) DirtyFiles(s *Share) []DirtyFile {
	c.dirtyFiles.mu.Lock()
	defer c.dirtyFiles.mu.Unlock()
	var out []DirtyFile
	for id, e := range c.dirtyFiles.m {
		if e.tag == s.Tag {
			out = append(out, DirtyFile{File: id, Oldest: e.oldest})
		}
	}
	return out
}
