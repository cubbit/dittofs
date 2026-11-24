package lockorder

import (
	"sync"
	"sync/atomic"
)

// Rank orders the lock domains: file append → capacity → file index →
// file map → segment append → storage guard. An acquisition follows the
// order, and no two locks of one domain are held at once. Zero is
// unranked: a lock with rank 0 is never reported to the recorder, while
// its mutex works whether or not one is installed.
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
	if r != 0 && int(r) < len(rankNames) {
		return rankNames[r]
	}
	return "unranked"
}

// Mutex is a sync.Mutex that reports every acquisition to a recorder when
// one is installed. The zero value works as a plain, unranked mutex,
// before Init and without a recorder.
type Mutex struct {
	mu   sync.Mutex
	rank Rank
	// leaf marks a lock Init'ed by InitLeaf: its holds record the leaf
	// flag, and any acquisition made under one reports, in order or not.
	leaf bool
	rec  atomic.Pointer[Recorder]
}

var _ sync.Locker = (*Mutex)(nil)

// Init installs the rank and, when rec is non-nil, the recorder that the
// mutex reports every acquisition to; a rank of 0 keeps the mutex working
// while telling the recorder nothing.
//
// decision: Init writes the rank as a plain field Lock and Unlock read,
// so it must be called before the mutex is shared or used concurrently.
// A recorder installed while the lock is already held never saw that
// hold, so the hold's release is dropped by the recorder and its books
// stay balanced; no atomic rank/recorder pair is needed, since no
// concurrent Init exists under this contract.
func (m *Mutex) Init(rank Rank, rec *Recorder) {
	m.rank = rank
	m.rec.Store(rec)
}

// InitLeaf is Init with the lock marked a leaf of its domain: the recorder
// reports any acquisition made while this lock is held, in order or not.
// A lock whose callers order themselves around it — coming at it last, from
// every direction at once — has no rank that places it in the order, so
// this marker is what the first caller to take another lock under it fails
// the recorded run with, rather than a cycle the ranks alone cannot see.
// The Init contract applies.
func (m *Mutex) InitLeaf(rank Rank, rec *Recorder) {
	m.leaf = true
	m.Init(rank, rec)
}

func (m *Mutex) Lock() {
	// ponytail: a load on every lock, and on every Unlock a load plus, with
	// a recorder installed, a deferred Release call that keeps Unlock out of
	// the inliner's reach. Upgrade is a build tag that compiles the recorder
	// out of production, once a profile of the 64-file benchmark shows the
	// load or the call.
	if r := m.rec.Load(); r != nil {
		r.acquire(m.rank, m, m.leaf)
	}
	m.mu.Lock()
}

func (m *Mutex) Unlock() {
	if r := m.rec.Load(); r != nil {
		defer r.Release(m)
	}
	m.mu.Unlock()
}

// TryLock is the underlying mutex's. A failed attempt records nothing: it
// takes no lock and waits for none. A successful one is an acquisition like
// Lock's, recorded once the lock is held, so the Unlock that follows
// releases a hold of the caller's own rather than another goroutine's
// recorded attempt.
func (m *Mutex) TryLock() bool {
	if !m.mu.TryLock() {
		return false
	}
	if r := m.rec.Load(); r != nil {
		r.acquire(m.rank, m, m.leaf)
	}
	return true
}

// RWMutex is the same arrangement over sync.RWMutex, for the file index
// lock: readers hold it shared, a writer exclusively. A read hold records
// under the lock's own identity, so a nested RLock of the same lock reports
// as the same domain, as does a reader taking it exclusively.
type RWMutex struct {
	mu   sync.RWMutex
	rank Rank
	rec  atomic.Pointer[Recorder]
}

var _ sync.Locker = (*RWMutex)(nil)

// Init installs the rank and the recorder; the contract is Mutex.Init's.
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
		defer r.Release(rw)
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
		defer r.Release(rw)
	}
	rw.mu.RUnlock()
}
