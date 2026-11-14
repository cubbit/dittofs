package datapath

import (
	"sync"
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
	//
	// ponytail: the oldest is the lowest key, found by a scan, so its cost
	// grows with the seconds the dirty backlog spans, not with files or
	// extents. Upgrade to a min-heap with lazy deletion when a backlog that
	// spans many seconds shows the scan in a profile.
	byTime map[int64]int
}

func (l *dirtyList) count(t time.Time, n int) {
	if t.IsZero() {
		return
	}
	if l.byTime == nil {
		l.byTime = make(map[int64]int)
	}
	k := t.Unix()
	if l.byTime[k] += n; l.byTime[k] == 0 {
		delete(l.byTime, k)
	}
}

// oldestDirty returns the start of the second of the oldest dirty write of any
// file, so it can overstate that write's age by under a second and never
// understates it, or the zero time when no listed file has a known one.
func (c *Core) oldestDirty() time.Time {
	l := &c.dirtyFiles
	l.mu.Lock()
	defer l.mu.Unlock()
	first := true
	var lo int64
	for k := range l.byTime {
		if first || k < lo {
			lo, first = k, false
		}
	}
	if first {
		return time.Time{}
	}
	return time.Unix(lo, 0)
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
