package lockorder

import (
	"sync"
	"sync/atomic"
)

// Rank orders the lock domains: file append → capacity → file index →
// file map → segment append → storage guard. An acquisition follows the
// order, and no two locks of one domain are held at once. Zero is unranked:
// a lock with rank 0 is still excluded from checks while its mutex works.
type Rank uint8

const (
	// FileAppend serialises one file's appends; the one domain held across
	// a record write.
	FileAppend Rank = iota + 1
	// Capacity guards the reservation counters, store-wide.
	Capacity
	// FileIndex guards one file's placement index entries; no I/O runs
	// under it.
	FileIndex
	// FileMap guards finding, adding or removing one entry of the map of
	// which files have index entries; nothing is acquired under it.
	FileMap
	// SegmentAppend guards the write position of one active segment.
	SegmentAppend
	// StorageGuard guards a segment's bytes against being freed or moved.
	// The last lock in the order, so nothing is acquired after it.
	StorageGuard
)

var rankNames = [...]string{
	FileAppend:    "file append",
	Capacity:      "capacity",
	FileIndex:     "file index",
	FileMap:       "file map",
	SegmentAppend: "segment append",
	StorageGuard:  "storage guard",
}

func (r Rank) String() string {
	if int(r) < len(rankNames) {
		return rankNames[r]
	}
	return "unranked"
}

// Mutex is a sync.Mutex that reports every acquisition to a recorder when
// one is installed. The zero value works as a plain, unranked mutex.
type Mutex struct {
	mu   sync.Mutex
	rank Rank
	rec  atomic.Pointer[Recorder]
}

// Init installs the rank and the recorder, which may be nil: a nil recorder
// leaves the mutex recording nothing. Init may be called at any point in the
// mutex's life; the next Lock or Unlock reports to what it last installed.
func (m *Mutex) Init(rank Rank, rec *Recorder) {
	m.rank = rank
	m.rec.Store(rec)
}

func (m *Mutex) Lock() {
	// ponytail: a load on every lock. Upgrade is a build tag that compiles
	// the recorder out of production, once a profile of the 64-file benchmark
	// shows the load.
	if r := m.rec.Load(); r != nil {
		r.Acquire(m.rank, m)
	}
	m.mu.Lock()
}

func (m *Mutex) Unlock() {
	if r := m.rec.Load(); r != nil {
		defer r.Release(m.rank, m)
	}
	m.mu.Unlock()
}

// RWMutex is the same arrangement over sync.RWMutex, for the storage guard:
// readers hold it shared, an unlink or a recycle exclusively. Readers and
// writers all record an acquisition under the one rank, so two shared holds
// appear to the recorder as two held locks of one domain when they are the
// same instance's — which the same-domain check exempts, since sharing is
// the point of a read lock.
type RWMutex struct {
	mu   sync.RWMutex
	rank Rank
	rec  atomic.Pointer[Recorder]
}

func (rw *RWMutex) Init(rank Rank, rec *Recorder) {
	rw.rank = rank
	rw.rec.Store(rec)
}

func (rw *RWMutex) Lock() {
	if r := rw.rec.Load(); r != nil {
		r.Acquire(rw.rank, rw)
	}
	rw.mu.Lock()
}

func (rw *RWMutex) Unlock() {
	if r := rw.rec.Load(); r != nil {
		defer r.Release(rw.rank, rw)
	}
	rw.mu.Unlock()
}

func (rw *RWMutex) RLock() {
	if r := rw.rec.Load(); r != nil {
		r.Acquire(rw.rank, rw)
	}
	rw.mu.RLock()
}

func (rw *RWMutex) RUnlock() {
	if r := rw.rec.Load(); r != nil {
		defer r.Release(rw.rank, rw)
	}
	rw.mu.RUnlock()
}
