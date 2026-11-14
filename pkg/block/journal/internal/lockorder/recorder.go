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
	OutOfOrder Kind = iota + 1
	// SameDomain is two locks of one domain held at once, so two files'
	// append locks together, or one file index lock read-locked twice,
	// which deadlocks against a writer waiting between the two holds.
	SameDomain
	// GuardUnderIndex is the storage guard acquired under the file index:
	// the order that deadlocks against reclamation, which repoints under
	// the index and then takes the guard.
	GuardUnderIndex
	// AcquireUnderMap is anything acquired while the file map is held;
	// its domain covers only finding, adding or removing one entry.
	AcquireUnderMap
	// GuardWaitUnderLock is the storage guard's drain wait taken while a
	// lock of another domain is held: the one guarded form that blocks
	// must run holding nothing but the wait.
	GuardWaitUnderLock
	// IOUnderIndex is I/O performed under the file index, which is
	// memory; nothing slow may run under it.
	IOUnderIndex
	// IOUnderMap is I/O performed under the file map, which is held only to
	// find, add or remove one entry.
	IOUnderMap
	// HeldAtCallback is a lock still held on entry to the offload
	// callback, which hands control to code that may take seconds.
	HeldAtCallback
	// HeldAtWait is a rank still held at a wait that must not be made
	// under it.
	HeldAtWait
	// AcquiredUnderLeaf is a lock acquired while a lock Init'ed as a leaf
	// is held: the leaf's callers come at it from every direction, so its
	// hold promises that nothing — in order or not — is taken under it, and
	// the first caller to take anything is the cycle the ranks alone
	// cannot name.
	AcquiredUnderLeaf
)

var kindNames = [...]string{
	OutOfOrder:         "out of order",
	SameDomain:         "same domain",
	GuardUnderIndex:    "guard under index",
	AcquireUnderMap:    "acquire under map",
	GuardWaitUnderLock: "guard wait under lock",
	IOUnderIndex:       "io under index",
	IOUnderMap:         "io under map",
	HeldAtCallback:     "held at callback",
	HeldAtWait:         "held at wait",
	AcquiredUnderLeaf:  "acquired under leaf",
}

func (k Kind) String() string {
	if k == 0 {
		return "none"
	}
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
// lock's pointer, so two locks of one domain stay told apart. leaf says the
// hold is of a lock Init'ed by InitLeaf, whose promise is that nothing is
// taken under it.
type heldLock struct {
	rank Rank
	id   any
	leaf bool
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
	// recorded, or the readers sharing one lock — so the identity maps
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
// domain, guard under index, acquire under map, guard wait under lock, out
// of order — and only the highest priority one is recorded. A rank of 0 is
// never recorded.
//
// decision: a nested read hold of one RWMutex is reported as the same
// domain like any other pair of same-rank holds. The one RWMutex in the
// journal is the file index lock, where a second RLock under the first
// deadlocks against a writer waiting between the two, which is the
// recursive read-lock case sync.RWMutex documents. Withdraw the report if
// a lock whose readers never wait on a writer is ever given this type.
func (r *Recorder) Acquire(rank Rank, id any) {
	r.acquire(rank, id, false)
}

// acquire is Acquire with the hold's leaf flag, which only the ranked mutex
// itself can know: Mutex.Lock and TryLock pass their own, so a hold of a
// leaf-Init'ed lock carries the promise the recorded rules read there.
// Hand-held probes acquire leaf false: a probe promises nothing. A rank of 0
// is never recorded, and a nil recorder records nothing.
func (r *Recorder) acquire(rank Rank, id any, leaf bool) {
	if r == nil || rank == 0 {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()

	heldNow := r.byGoroutine[gid]
	r.recordAcquisition(heldNow, rank, false)

	r.byGoroutine[gid] = append(heldNow, heldLock{rank: rank, id: id, leaf: leaf})
	holders := r.holder[id]
	if holders == nil {
		holders = make(map[uint64]int)
		r.holder[id] = holders
	}
	holders[gid]++
}

// AcquireRef checks the rules at the moment a storage guard reference is
// requested — at the entry of the cache's Acquire, granted or refused — and
// records nothing on the goroutine's books. A reference is a pin that never
// waits on a lock once granted: the stream keeps its active segment's
// reference from one roll to the next and whichever goroutine rolls next
// releases it, readers hold theirs across their copy, so no goroutine owns
// one. The requesting Acquire can itself wait, on an in-flight open of the
// same segment and on the descriptor budget's reclaim, and can be refused,
// either of which can follow work that already ran under the caller's
// locks, which is why the check runs on the request rather than the grant.
// Requested under the file index or the file map the reference is reported
// like any other acquisition; the id names the segment and is not kept.
func (r *Recorder) AcquireRef(id any) {
	if r == nil {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()

	r.recordAcquisition(r.byGoroutine[gid], StorageGuard, true)
}

// recordAcquisition runs the priority-ordered rules for an acquisition and
// records the first rule it breaks. isRef names a storage guard reference,
// which is exempt from the drain wait's rule: only the drain is the
// exclusive guard that waits for its holders. The rules, in priority
// order: same domain; guard under index; acquire under map; the drain
// wait's wait-under-lock; out of order.
//
// decision: a reference is not on any goroutine's books, so the drain wait
// is checked against the plain locks the goroutine holds and nothing else:
// the drain must also hold no guard reference, and a reference is invisible
// to this check, so a drain entered while its own goroutine holds a pin of
// another segment is unreported. What this gives up is the two-segment
// cycle, one goroutine pinning S1 while it drains S2 and another the
// mirror, and a drain wait made while holding a pin of its own segment.
// Production retirers hold no pins, so the recorder never needs to see
// one. Withdraw it if a retirer ever holds a reference across a drain.
func (r *Recorder) recordAcquisition(heldNow []heldLock, rank Rank, isRef bool) {
	var kind Kind
	for _, h := range heldNow {
		if h.rank == rank {
			kind = SameDomain
			break
		}
	}
	if kind == 0 && rank == StorageGuard {
		for _, h := range heldNow {
			if h.rank == FileIndex {
				kind = GuardUnderIndex
				break
			}
		}
	}
	if kind == 0 {
		for _, h := range heldNow {
			if h.rank == FileMap {
				kind = AcquireUnderMap
				break
			}
		}
	}
	if kind == 0 && !isRef && rank == StorageGuard {
		// The drain wait: the one guarded form that blocks on holders. It
		// must run holding no lock of any domain — reclamation takes the
		// guard "holding no other lock" — so any held lock reports here,
		// after the rules that name which lock it was taken under.
		for _, h := range heldNow {
			if h.rank != StorageGuard {
				kind = GuardWaitUnderLock
				break
			}
		}
	}
	if kind == 0 {
		for _, h := range heldNow {
			if h.rank > rank {
				kind = OutOfOrder
				break
			}
		}
	}
	// The leaf promise comes last, as the catch-all it is: an order the
	// rank rules can already see keeps the report they name, and the one
	// no rank difference reports — an in-order acquisition under a lock
	// whose callers must never take anything under it — lands here.
	if kind == 0 {
		for _, h := range heldNow {
			if h.leaf {
				kind = AcquiredUnderLeaf
				break
			}
		}
	}
	if kind == 0 {
		return
	}
	r.violations = append(r.violations, Violation{
		Kind:  kind,
		Rank:  rank,
		Held:  ranksOf(heldNow),
		Stack: string(debug.Stack()),
	})
}

// Release records that the lock id is no longer held. It removes the
// caller's own recorded hold of id first, so a concurrent goroutine's hold
// or recorded attempt on the same lock is untouched; only a release from a
// goroutine that holds no hold of id — a cross-goroutine unlock — falls
// back to the holder set. A release of an identity the recorder never saw
// — a lock held before its recorder was installed — is ignored.
func (r *Recorder) Release(id any) {
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
	//
	// decision: an ambiguous cross-goroutine release fails closed: it
	// removes nothing, so the original holder's hold stays on the books
	// and its goroutine can later be reported for a lock it no longer
	// holds, or a later acquisition reported against it. The ceiling is
	// an identity held by several goroutines at once whose hold is then
	// released by a goroutine that holds none of it: a file index read
	// lock several readers share, unlocked by a goroutine that did not
	// lock it. Withdraw it if a ranked lock is ever handed to another
	// goroutine to unlock while others hold the same identity.
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
// turns disk latency into index contention, and the file map is held only
// to find, add or remove one entry, so both are violations; under every
// other rank it is allowed — the file append domain is the one held across
// a record write.
func (r *Recorder) IO(op string) {
	if r == nil {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()

	heldNow := r.byGoroutine[gid]
	for _, h := range heldNow {
		var kind Kind
		switch h.rank {
		case FileIndex:
			kind = IOUnderIndex
		case FileMap:
			kind = IOUnderMap
		default:
			continue
		}
		r.violations = append(r.violations, Violation{
			Kind:  kind,
			Rank:  h.rank,
			Held:  ranksOf(heldNow),
			Where: op,
			Stack: string(debug.Stack()),
		})
		return
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
