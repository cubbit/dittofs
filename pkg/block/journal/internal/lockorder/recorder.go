package lockorder

import (
	"bytes"
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
	// append locks together. Only a nested read hold of one RWMutex is
	// exempt, since readers share.
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

// heldLock is one lock a goroutine holds. id is the lock's identity: the
// lock's pointer for an exclusive hold, a readIdentity for a shared hold
// of one RWMutex, so two locks of one domain stay told apart while a
// reader's nested hold of its own guard is recognised.
type heldLock struct {
	rank Rank
	id   any
}

// Recorder watches ranked acquisitions and records every rule they break.
// It is test machinery: the ranked mutexes reach it through an atomic
// pointer, so a nil one costs one load per acquisition and its checks never
// run in production. Every method returns immediately on a nil receiver.
// A rank of 0 is never recorded: an unranked lock is invisible to the
// checks while its mutex works as a plain one. The zero value is not
// usable; build it with NewRecorder. Its own mutex is a plain sync.Mutex,
// unranked, because the recorder is not part of the lock order it polices.
type Recorder struct {
	mu sync.Mutex
	// byGoroutine is each goroutine's held locks in acquisition order.
	// holder counts, per identity, how many holds of it each goroutine
	// has on the books: several goroutines may hold one identity at
	// once — a contended mutex whose next acquirer's attempt is already
	// recorded, or the readers sharing one guard — so the identity maps
	// to a set of holders, not one.
	byGoroutine map[uint64][]heldLock
	holder      map[any]map[uint64]int
	violations  []Violation
	checks      map[string]int
}

func NewRecorder() *Recorder {
	return &Recorder{
		byGoroutine: make(map[uint64][]heldLock),
		holder:      make(map[any]map[uint64]int),
		checks:      make(map[string]int),
	}
}

// Acquire records that the calling goroutine takes the lock id of the
// given rank, and records a violation when that breaks a rule. It runs
// before the lock is taken, so an order that would deadlock is reported
// first and the account survives a run that then hangs — and so a
// goroutine waiting out another holder puts its hold on the books before
// it owns the lock, which the per-identity holder set keeps apart from
// the holder's own. The rules are checked in a fixed priority — same
// domain, guard under index, acquire under map, out of order — and only
// the highest priority one is recorded. A rank of 0 is never recorded.
func (r *Recorder) Acquire(rank Rank, id any) {
	if r == nil || rank == 0 {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()

	heldNow := r.byGoroutine[gid]
	// decision: a nested read hold of one RWMutex — the same readIdentity
	// twice — is the one exemption from the same-domain rule, since
	// readers share. Every other same-rank acquisition, a Mutex locked
	// twice or a reader taking its own guard exclusively, is a certain
	// self-deadlock and is reported: an exclusive identity is a pointer,
	// which never equals a readIdentity. The exemption's ceiling: a
	// recursive RLock can still deadlock against a writer waiting
	// between the two holds and is not reported; withdraw the exemption
	// if such a pattern ever appears.
	if _, isRead := id.(readIdentity); isRead {
		for _, h := range heldNow {
			if h.id == id && h.rank == rank {
				r.byGoroutine[gid] = append(heldNow, h)
				r.holder[id][gid]++
				return
			}
		}
	}

	r.recordAcquisition(heldNow, rank, false)

	r.byGoroutine[gid] = append(heldNow, heldLock{rank: rank, id: id})
	holders := r.holder[id]
	if holders == nil {
		holders = make(map[uint64]int)
		r.holder[id] = holders
	}
	holders[gid]++
}

// AcquireRef records that the calling goroutine takes a storage guard
// reference of id — a shared hold, the way an RWMutex read hold is. It is
// exempt from the same-domain rule against other guard references only:
// every other rule still binds it, and the exemption never reaches a plain
// StorageGuard acquisition, which a drain wait's exclusive hold must keep
// clear of every reference the goroutine holds. A rank of 0 is never
// recorded, like Acquire.
func (r *Recorder) AcquireRef(id any) {
	if r == nil {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()

	heldNow := r.byGoroutine[gid]
	r.recordAcquisition(heldNow, StorageGuard, true)

	ref := heldLock{rank: StorageGuard, id: refIdentity{e: id}}
	r.byGoroutine[gid] = append(heldNow, ref)
	holders := r.holder[ref.id]
	if holders == nil {
		holders = make(map[uint64]int)
		r.holder[ref.id] = holders
	}
	holders[gid]++
}

// ReleaseRef ends the hold AcquireRef recorded, like Release.
func (r *Recorder) ReleaseRef(id any) {
	if r == nil {
		return
	}
	r.Release(StorageGuard, refIdentity{e: id})
}

// recordAcquisition runs the priority-ordered rules for an acquisition
// that is not a re-entrant hold, and records the first rule it breaks.
// refExempt names an acquisition — a storage guard reference — whose
// same-domain check skips other held references, the one exemption below;
// every other rule it runs unchanged.
func (r *Recorder) recordAcquisition(heldNow []heldLock, rank Rank, refExempt bool) {
	kind, broke := Kind(0), false
	for _, h := range heldNow {
		if h.rank != rank {
			continue
		}
		// decision: a guard reference held beside another guard reference
		// is not reported, because references are shared: a reference
		// never waits on another reference's holder, and a stream rolling
		// onto its next segment holds the old one's ref while it takes
		// the successor's. Withdraw the exemption if a reference acquire
		// ever blocks on another reference; the drain wait — a plain
		// StorageGuard acquisition — stays checked against every hold.
		if refExempt {
			if _, isRef := h.id.(refIdentity); isRef {
				continue
			}
		}
		kind, broke = SameDomain, true
		break
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

// Release records that the lock id is no longer held. It removes the
// caller's own recorded hold of id first, so a concurrent goroutine's hold
// or recorded attempt on the same lock is untouched; only a release from a
// goroutine that holds no hold of id — a cross-goroutine unlock — falls
// back to the holder set. A release of an identity the recorder never saw
// — a lock held before its recorder was installed — is ignored.
func (r *Recorder) Release(_ Rank, id any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	gid := goroutineID()
	// The caller's own list first: the common release, whose hold must
	// not be confused with a concurrent goroutine's hold or recorded
	// attempt on the same identity. The last matching entry is dropped:
	// a writer's hold sits at the top of its own list.
	if hs, ok := r.byGoroutine[gid]; ok {
		for i := len(hs) - 1; i >= 0; i-- {
			if hs[i].id != id {
				continue
			}
			r.drop(gid, hs, i, id)
			return
		}
	}
	// The caller never acquired this identity: a cross-goroutine unlock,
	// which the holder set routes to the one goroutine that holds it.
	// With several holders on the books — contended readers whose
	// attempts are recorded while another holder still owns the lock —
	// a release from a goroutine that holds no hold of it names no
	// single holder, so it is ignored rather than guess.
	if holders := r.holder[id]; len(holders) == 1 {
		for holderGid := range holders {
			if hs, ok := r.byGoroutine[holderGid]; ok {
				for i := len(hs) - 1; i >= 0; i-- {
					if hs[i].id != id {
						continue
					}
					r.drop(holderGid, hs, i, id)
					return
				}
			}
		}
	}
}

// drop removes one recorded hold of id from gid's list, keeping the
// per-identity holder count in step with the list. Borrows both list
// halves rather than name a middle.
func (r *Recorder) drop(gid uint64, hs []heldLock, i int, id any) {
	r.byGoroutine[gid] = append(hs[:i], hs[i+1:]...)
	if len(r.byGoroutine[gid]) == 0 {
		delete(r.byGoroutine, gid)
	}
	holders := r.holder[id]
	if holders == nil {
		return
	}
	if holders[gid] > 1 {
		holders[gid]--
		return
	}
	delete(holders, gid)
	if len(holders) == 0 {
		delete(r.holder, id)
	}
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
	rest, ok := bytes.CutPrefix(buf[:n], []byte("goroutine "))
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
