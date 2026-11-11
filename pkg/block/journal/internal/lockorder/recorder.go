package lockorder

import (
	"runtime"
	"runtime/debug"
	"sync"
)

// Kind names the rule a violation broke.
type Kind int

const (
	// OutOfOrder is a lock acquired while a lock of a strictly higher rank
	// is held.
	OutOfOrder Kind = iota
	// SameDomain is two locks of one domain held at once, so two files'
	// append locks together, or two read locks of distinct instances of
	// one domain. Two holds of the same lock instance are exempt, since a
	// shared lock is meant to nest in itself.
	SameDomain
	// GuardUnderIndex is the storage guard acquired under the file index:
	// the order that deadlocks against reclamation, which repoints under
	// the index and then takes the guard.
	GuardUnderIndex
	// AcquireUnderMap is anything acquired while the file map is held;
	// its domain covers only finding, adding or removing one entry.
	AcquireUnderMap
	// IOUnderIndex is I/O performed under the file index, which is
	// memory; nothing slow may run under it.
	IOUnderIndex
	// HeldAtCallback is a lock still held on entry to the offload
	// callback, which hands control to code that may take seconds.
	HeldAtCallback
	// HeldAtWait is a rank still held at a wait that must not be made
	// under it.
	HeldAtWait
)

var kindNames = [...]string{
	OutOfOrder:      "out of order",
	SameDomain:      "same domain",
	GuardUnderIndex: "guard under index",
	AcquireUnderMap: "acquire under map",
	IOUnderIndex:    "io under index",
	HeldAtCallback:  "held at callback",
	HeldAtWait:      "held at wait",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "unknown"
}

// Violation is one broken rule. Rank is the rank of the offending lock: the
// one being acquired for an acquisition rule, the one being waited for at
// an assert-not-held, the first held at a callback assert. Held is the
// caller's held ranks at that moment, in acquisition order, Where is the
// label an assertion or I/O carried, and Stack is where the rule broke.
type Violation struct {
	Kind  Kind
	Rank  Rank
	Held  []Rank
	Where string
	Stack string
}

// heldLock is one lock a goroutine holds. id is the lock's identity, by
// convention its pointer, so two locks of one domain are told apart while
// one lock nesting in itself is recognised.
type heldLock struct {
	rank Rank
	id   any
}

// Recorder watches ranked acquisitions and records every rule they break.
// It is test machinery: the ranked mutexes reach it through an atomic
// pointer, so a nil one costs one load per acquisition and its checks never
// run in production. Every method returns immediately on a nil receiver.
// The zero value is not usable; build it with NewRecorder. Its own mutex is
// a plain sync.Mutex, unranked, because the recorder is not part of the
// lock order it polices.
type Recorder struct {
	mu sync.Mutex
	// byGoroutine is each goroutine's held locks in acquisition order;
	// holder is the goroutine that last acquired a lock identity, so a
	// release runs on whichever goroutine owns it.
	byGoroutine map[uint64][]heldLock
	holder      map[any]uint64
	violations  []Violation
	checks      map[string]int
}

func NewRecorder() *Recorder {
	return &Recorder{
		byGoroutine: make(map[uint64][]heldLock),
		holder:      make(map[any]uint64),
		checks:      make(map[string]int),
	}
}

// Acquire records that the calling goroutine takes the lock id of the
// given rank, and records a violation when that breaks a rule. It runs
// before the lock is taken, so an order that would deadlock is reported
// first and the account survives a run that then hangs. The rules are
// checked in a fixed priority — same domain, guard under index, acquire
// under map, out of order — and only the highest priority one is recorded.
func (r *Recorder) Acquire(rank Rank, id any) {
	if r == nil {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()

	heldNow := r.byGoroutine[gid]
	// A re-entrant hold of the same lock skips the checks: a shared lock
	// nesting in itself is how readers use it. One entry is kept per
	// hold, so the releases stay balanced.
	for _, h := range heldNow {
		if h.rank == rank && h.id == id {
			r.byGoroutine[gid] = append(heldNow, h)
			r.holder[id] = gid
			return
		}
	}

	r.recordAcquisition(heldNow, rank)

	r.byGoroutine[gid] = append(heldNow, heldLock{rank: rank, id: id})
	r.holder[id] = gid
}

// recordAcquisition runs the priority-ordered rules for an acquisition
// that is not a re-entrant hold, and records the first rule it breaks.
func (r *Recorder) recordAcquisition(heldNow []heldLock, rank Rank) {
	kind, broke := Kind(0), false
	for _, h := range heldNow {
		if h.rank == rank {
			kind, broke = SameDomain, true
			break
		}
	}
	if !broke && rank == StorageGuard {
		for _, h := range heldNow {
			if h.rank == FileIndex {
				kind, broke = GuardUnderIndex, true
				break
			}
		}
	}
	if !broke {
		for _, h := range heldNow {
			if h.rank == FileMap {
				kind, broke = AcquireUnderMap, true
				break
			}
		}
	}
	if !broke {
		for _, h := range heldNow {
			if h.rank > rank {
				kind, broke = OutOfOrder, true
				break
			}
		}
	}
	if !broke {
		return
	}
	r.violations = append(r.violations, Violation{
		Kind:  kind,
		Rank:  rank,
		Held:  ranksOf(heldNow),
		Where: "",
		Stack: string(debug.Stack()),
	})
}

// Release records that the lock id is no longer held: the holder map finds
// the goroutine that acquired it, so the release may run on another
// goroutine than the acquisition. A release of an identity the recorder
// never saw — a lock held before its recorder was installed — is ignored.
func (r *Recorder) Release(_ Rank, id any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	gid, ok := r.holder[id]
	if !ok {
		return
	}
	hs := r.byGoroutine[gid]
	for i := len(hs) - 1; i >= 0; i-- {
		if hs[i].id != id {
			continue
		}
		hs = append(hs[:i], hs[i+1:]...)
		if len(hs) == 0 {
			delete(r.byGoroutine, gid)
			delete(r.holder, id)
			return
		}
		r.byGoroutine[gid] = hs
		// A nested hold of the same identity keeps it registered, so a
		// further release still finds its goroutine.
		stillHeld := false
		for _, h := range hs {
			if h.id == id {
				stillHeld = true
				break
			}
		}
		if !stillHeld {
			delete(r.holder, id)
		}
		return
	}
	delete(r.holder, id)
}

// IO records that the caller is about to do I/O. I/O under the file index
// turns disk latency into index contention, so it is a violation; under
// every other rank it is allowed — the file append domain is the one held
// across a record write.
func (r *Recorder) IO(op string) {
	if r == nil {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()
	heldNow := r.byGoroutine[gid]
	for _, h := range heldNow {
		if h.rank == FileIndex {
			r.violations = append(r.violations, Violation{
				Kind:  IOUnderIndex,
				Rank:  FileIndex,
				Held:  ranksOf(heldNow),
				Where: op,
				Stack: string(debug.Stack()),
			})
			return
		}
	}
}

// AssertNoneHeld records a violation when the calling goroutine holds any
// ranked lock: no lock may be held across the offload callback, which takes
// control for seconds.
func (r *Recorder) AssertNoneHeld(where string) {
	if r == nil {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[where]++
	heldNow := r.byGoroutine[gid]
	if len(heldNow) == 0 {
		return
	}
	r.violations = append(r.violations, Violation{
		Kind:  HeldAtCallback,
		Rank:  heldNow[0].rank,
		Held:  ranksOf(heldNow),
		Where: where,
		Stack: string(debug.Stack()),
	})
}

// AssertNotHeld records a violation when the calling goroutine still holds
// the given rank: a wait that must not be made under it is about to run.
func (r *Recorder) AssertNotHeld(rank Rank, where string) {
	if r == nil {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[where]++
	heldNow := r.byGoroutine[gid]
	for _, h := range heldNow {
		if h.rank == rank {
			r.violations = append(r.violations, Violation{
				Kind:  HeldAtWait,
				Rank:  rank,
				Held:  ranksOf(heldNow),
				Where: where,
				Stack: string(debug.Stack()),
			})
			return
		}
	}
}

// Held returns the calling goroutine's held ranks in acquisition order;
// nil when it holds nothing.
func (r *Recorder) Held() []Rank {
	if r == nil {
		return nil
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.byGoroutine[gid]) == 0 {
		return nil
	}
	return ranksOf(r.byGoroutine[gid])
}

// Checks returns how many assertions carried the label where, whether or
// not they found a violation.
func (r *Recorder) Checks(where string) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checks[where]
}

// Violations returns every violation recorded so far; nil when there were
// none. The slice is the recorder's own; do not modify it.
func (r *Recorder) Violations() []Violation {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.violations) == 0 {
		return nil
	}
	return r.violations
}

// ranksOf flattens held locks into their ranks, in acquisition order.
func ranksOf(hs []heldLock) []Rank {
	out := make([]Rank, len(hs))
	for i, h := range hs {
		out[i] = h.rank
	}
	return out
}

// goroutineID returns the calling goroutine's id, parsed from its own
// stack dump: the only portable account of which goroutine is running.
func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	rest, ok := bytesCutPrefix(buf[:n], "goroutine ")
	if !ok {
		return 0
	}
	var id uint64
	for _, c := range rest {
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}

func bytesCutPrefix(b []byte, prefix string) ([]byte, bool) {
	if len(b) < len(prefix) || string(b[:len(prefix)]) != prefix {
		return nil, false
	}
	return b[len(prefix):], true
}
