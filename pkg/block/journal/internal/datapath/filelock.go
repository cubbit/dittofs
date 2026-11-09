package datapath

import (
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// fileLockShards is the number of shards of the table. It is a power of two so
// a shard is picked with a mask.
const fileLockShards = 64

// fileLocks is the table of file append domains: one mutex per file, held
// across a file's append, which includes I/O.
//
// decision: the file append domain is the one domain held across I/O, and it
// guards exactly one file, so two files never wait on each other. That is why
// it lives apart from the short, I/O-free locks around it. Revisit if a domain
// is ever needed that spans more than one file.
//
// ponytail: every write takes a shard lock on entry and again on exit, so the
// shards are a contention ceiling under many writers of distinct files.
// Upgrade to storing the lock in the placement index's file entry, which needs
// that entry to outlive a file's last extent, when the shard locks show up in
// a profile.
//
// An entry exists only while a holder or waiter references it, so the table is
// bounded by the files in flight rather than the files ever written.
type fileLocks struct {
	shards [fileLockShards]fileLockShard
}

type fileLockShard struct {
	// mu covers only finding or creating an entry and releasing one; it is
	// never held while an entry's mutex is held or waited on.
	mu sync.Mutex
	m  map[core.FileID]*fileLockEntry
}

// fileLockEntry is one file's domain. refs counts holders and waiters and is
// guarded by the owning shard's mu.
type fileLockEntry struct {
	mu   sync.Mutex
	refs int
}

func newFileLocks() *fileLocks {
	t := &fileLocks{}
	for i := range t.shards {
		t.shards[i].m = make(map[core.FileID]*fileLockEntry)
	}
	return t
}

// shardOf picks id's shard from an FNV-1a hash of its bytes.
func (t *fileLocks) shardOf(id core.FileID) *fileLockShard {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	b := id.Bytes()
	h := uint64(offset)
	for _, c := range b {
		h ^= uint64(c)
		h *= prime
	}
	return &t.shards[h&(fileLockShards-1)]
}

// Lock takes id's file append domain, waiting for its current holder, and
// returns the release. The release must be called exactly once.
func (t *fileLocks) Lock(id core.FileID) (unlock func()) {
	s := t.shardOf(id)
	s.mu.Lock()
	e, ok := s.m[id]
	if !ok {
		e = &fileLockEntry{}
		s.m[id] = e
	}
	e.refs++
	s.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		s.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(s.m, id)
		}
		s.mu.Unlock()
	}
}

// size returns the number of entries now.
//
// decision: nothing but tests reads this, so it takes every shard lock in
// turn and is not a consistent snapshot under concurrent use.
func (t *fileLocks) size() int {
	n := 0
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		n += len(s.m)
		s.mu.Unlock()
	}
	return n
}
