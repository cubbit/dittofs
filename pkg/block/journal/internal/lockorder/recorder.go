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
	// GuardWaitUnderLock is the storage guard's drain wait taken while a
	// lock of another domain is held: the one guarded form that blocks
	// must run holding nothing but the wait.
	GuardWaitUnderLock
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
	OutOfOrder:         "out of order",
	SameDomain:         "same domain",
	GuardUnderIndex:    "guard under index",
	AcquireUnderMap:    "acquire under map",
	GuardWaitUnderLock: "guard wait under lock",
	IOUnderIndex:       "io under index",
	HeldAtCallback:     "held at callback",
	HeldAtWait:         "held at wait",
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

	r.recordAcquisition(heldNow, rank, false, id)

	r.byGoroutine[gid] = append(heldNow, heldLock{rank: rank, id: id})
	holders := r.holder[id]
	if holders == nil {
		holders = make(map[uint64]int)
		r.holder[id] = holders
	}
	holders[gid]++
}

// AcquireRef records that the calling goroutine takes a storage guard
// reference of id — a pin: a shared hold that never waits on anything, so
// while it sits on the books it orders nothing the same goroutine takes
// later. The rules still bind the reference's own acquisition (under the
// file index or the file map it is reported like any hold), and the drain
// wait — a plain StorageGuard acquisition — is checked both against the
// locks the goroutine holds (it must hold none) and against a reference
// of its own segment (a certain self-deadlock). A rank of 0 is never
// recorded, like Acquire.
func (r *Recorder) AcquireRef(id any) {
	if r == nil {
		return
	}
	gid := goroutineID()
	r.mu.Lock()
	defer r.mu.Unlock()

	heldNow := r.byGoroutine[gid]
	r.recordAcquisition(heldNow, StorageGuard, true, id)

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

// orderingHeld is the held set whose locks order what the same goroutine
// acquires over them: every hold except a storage guard reference, which is
// a pin and orders nothing.
func orderingHeld(held []heldLock) []heldLock {
	out := held[:0:0]
	for _, h := range held {
		if _, isRef := h.id.(refIdentity); isRef {
			continue
		}
		out = append(out, h)
	}
	return out
}

// recordAcquisition runs the priority-ordered rules for an acquisition that
// is not a re-entrant hold, and records the first rule it breaks. isRef names
// an acquisition that is a storage guard reference of refID; every other
// acquisition passes isRef false. The rules, in priority order: same domain;
// guard under index; acquire under map; the drain wait's wait-under-lock;
// out of order.
//
// decision: a held reference is a pin — it never waits on another
// reference — so it is excluded from the held set that orders the same
// goroutine's later acquisitions, and no rule that compares that set ever
// sees it. Two checks still reach pins, both because a drain wait waits:
// the drain wait — a plain StorageGuard acquisition, which by definition is
// the reclamation path's exclusive hold; a future shared-read use of the
// rank would need its own entry point — reports against every guard
// reference the goroutine holds, of its own segment or any other, and a
// reference taken while a plain guard hold is on the books reports too, in
// both directions. The two-segment cycle is the reason: one goroutine
// pinning S1 while it drains S2, and another the mirror, is a real
// deadlock no same-segment check catches. Production retirers hold no
// pins, so the pair costs test choreography alone. Withdraw the ordering
// exclusion if a pin ever waits on a lock.
func (r *Recorder) recordAcquisition(heldNow []heldLock, rank Rank, isRef bool, refID any) {
	// broke names a rule that fired: OutOfOrder is itself the zero Kind, so
	// the kind alone cannot say one did.
	kind, broke := Kind(0), false
	// order is the held set the acquisition is checked against: every hold,
	// for the reference being taken now — a pin held beside a pin is
	// nothing, but a ref taken under a lock is reported — and the
	// pin-free set for every other acquisition.
	order := heldNow
	if !isRef {
		order = orderingHeld(heldNow)
	}

	for _, h := range heldNow {
		if h.rank != rank {
			continue
		}
		_, heldIsRef := h.id.(refIdentity)
		switch {
		case isRef && heldIsRef:
			// decision: a reference beside another reference is never
			// reported, of one segment or two: references are pins,
			// shared holds that never wait on another reference.
			// Withdraw the exemption if a reference acquire ever blocks
			// on another reference.
			continue
		case isRef && !heldIsRef, !isRef && heldIsRef:
			// decision: a plain guard hold beside a guard reference is
			// reported in both orders — its own segment or any other —
			// for the wait the plain hold parks in can be the other side
			// of the two-segment cycle the marker above names, and
			// reclamation holds nothing across the wait, so no
			// legitimate drain ever sees a pin. A future shared-read use
			// of the plain rank would need its own entry point.
		default:
			// Two plain holds of one rank: the ordinary same-domain
			// violation.
		}
		kind, broke = SameDomain, true
		break
	}
	if !broke && rank == StorageGuard {
		for _, h := range order {
			if h.rank == FileIndex {
				kind, broke = GuardUnderIndex, true
				break
			}
		}
	}
	if !broke {
		for _, h := range order {
			if h.rank == FileMap {
				kind, broke = AcquireUnderMap, true
				break
			}
		}
	}
	if !broke && !isRef && rank == StorageGuard {
		// The drain wait: the one guarded form that blocks on holders. It
		// must run holding no lock of any domain — reclamation takes the
		// guard "holding no other lock" — so any held lock reports here,
		// after the rules that name which lock it was taken under.
		for _, h := range order {
			if h.rank != StorageGuard {
				kind, broke = GuardWaitUnderLock, true
				break
			}
		}
	}
	if !broke {
		for _, h := range order {
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
