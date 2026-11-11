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
// decision: one mutex guards the map and nothing else is taken while it is
// held: not a file's append domain, the index, a segment or any I/O. Account
// runs under a file's domain and from the loss path, so a lock that waited on
// either would invert the order; a leaf cannot. Overturned if a caller ever
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
	switch _, listed := l.m[d.File]; {
	case d.Dirty == index.BecameClean:
		delete(l.m, d.File)
	case d.Dirty == index.BecameDirty || listed:
		if l.m == nil {
			l.m = make(map[core.FileID]dirtyEntry)
		}
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
