package datapath

import (
	"sync"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
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

	// rec is the recorder entries created later Init their mutex with, so a
	// lock made after SetRecorder joins the same account.
	rec atomic.Pointer[lockorder.Recorder]
}

type fileLockShard struct {
	// decision: mu is a leaf. It covers only finding or creating an entry
	// and releasing one, is never held while an entry's mutex is held or
	// waited on, and takes no lock itself.
	mu sync.Mutex
	m  map[core.FileID]*fileLockEntry
}

// fileLockEntry is one file's domain. refs counts holders and waiters and is
// guarded by the owning shard's mu.
type fileLockEntry struct {
	// decision: mu is the file append domain, rank file append, the one
	// domain held across a record's I/O and its publish.
	mu   lockorder.Mutex
	refs int
}

func newFileLocks() *fileLocks {
	t := &fileLocks{}
	for i := range t.shards {
		t.shards[i].m = make(map[core.FileID]*fileLockEntry)
	}
	return t
}

// SetRecorder installs the lock-order recorder the entries' mutexes report
// to, with their file append rank. Entries are created lazily, so each new
// one reads the stored recorder and Inits with it at creation — a rank of 0,
// an entry whose Init was skipped, is never recorded, which is why the
// entry's Init is not optional. It runs before the table is shared — before
// any concurrent Lock — and a nil recorder leaves every entry a plain mutex,
// so production behaviour is unchanged.
func (t *fileLocks) SetRecorder(rec *lockorder.Recorder) {
	t.rec.Store(rec)
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
		// Init at creation, under the shard's leaf: the entry's mutex
		// is used concurrently the moment it is published, and a lock
		// whose Init was skipped keeps rank 0 and is never recorded.
		e.mu.Init(lockorder.FileAppend, t.rec.Load())
		// Init at creation, under the shard's leaf: the entry's mutex is
		// used concurrently the moment it is published, and a lock whose
		// Init was skipped keeps rank 0 and is never recorded.
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
