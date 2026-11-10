package index

import (
	"bytes"
	"slices"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
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
	mu sync.Mutex
	m  map[core.FileID][2]core.ShareTag
}

// mark records id as rivalled, owned by tag with rival marking its records.
func (c *conflictSet) mark(id core.FileID, owner, rival core.ShareTag) {
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[core.FileID][2]core.ShareTag)
	}
	c.m[id] = [2]core.ShareTag{owner, rival}
	c.mu.Unlock()
}

// drop removes id from the set, whether or not it is a member.
func (c *conflictSet) drop(id core.FileID) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// snapshot returns one Conflict per rivalled file, ordered by FileID bytes.
func (c *conflictSet) snapshot() []Conflict {
	c.mu.Lock()
	out := make([]Conflict, 0, len(c.m))
	for id, tags := range c.m {
		out = append(out, Conflict{File: id, Tags: tags})
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b Conflict) int {
		ab, bb := a.File.Bytes(), b.File.Bytes()
		return bytes.Compare(ab[:], bb[:])
	})
	return out
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
