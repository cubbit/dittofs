package segment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// The pool's own refusals, beside the NoSpaceError the shared limits return.
var (
	// ErrBadClass is the refusal of an adoption naming a class the pool does
	// not hold: the spare classes are content, headroom and reserve, and a
	// spare found at open belongs to exactly one of them, which the caller
	// decides in the scan that found it.
	ErrBadClass = errors.New("segment: bad spare class")
	// ErrStarted is the refusal of an adoption into a pool that has started:
	// open syncs the directory once before any re-adopted spare is taken, and
	// a spare adopted after that point could be taken while its entry is one
	// a power cut could still lose.
	ErrStarted = errors.New("segment: the pool has started")
)

// Class is which third of the pool a spare stands in. The classes never lend
// to one another: a roll that finds only the other classes' spares is refused
// `spare` rather than served out of class.
type Class uint8

const (
	// Content is the class a stream's roll takes from.
	Content Class = iota + 1
	// Headroom is the class the header-only records' rolls take from.
	Headroom
	// Reserve is the class the repack copies' rolls take from.
	Reserve
)

// classNames feeds String below, indexed by Class.
var classNames = [...]string{"", "content", "headroom", "reserve"}

// String returns the class's name: content, headroom or reserve.
func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return fmt.Sprintf("Class(%d)", uint8(c))
}

// Targets is how many spares each class keeps ready, in segments. The caller
// sizes them: content from the spare target it configures, headroom and
// reserve from the capacity rules that size the headroom and the repack
// reserve. A negative target is a caller bug and panics; zero means the class
// keeps none.
//
// ponytail: the targets are static. A burst longer than one segment's fill
// time refuses `spare`. Upgrade to a target that follows the roll rate, once
// spare refusals show it.
type Targets struct {
	Content  int
	Headroom int
	Reserve  int
}

// PoolCounters is the pool's read: one lock and a copy from fields the pool
// maintains at each event that moves one, never a walk of its slices.
type PoolCounters struct {
	// Spares is the ready content spares, the ones held for open holds
	// included.
	Spares int64
	// HeadroomBytes is the ready headroom spares at the segment cap, as
	// allocated now.
	HeadroomBytes int64
	// RepackReserveBytes is the ready reserve spares at the segment cap, as
	// allocated now.
	RepackReserveBytes int64
	// Held is the content spares owed to the pool's open holds.
	Held int64
	// SpareRefusals is how many rolls the pool refused naming `spare`: each
	// counted once by the roll that drew the pool's final answer.
	SpareRefusals int64
}

// Pool is the spare pool: the spares of the three classes the Maker made,
// ready to be taken by a roll that costs no I/O, and the worker that keeps
// each class at its target by making spares off the write path.
//
// decision: ready, held, pending, incoming, started, stopped,
// deviceFull and retirer sit under the capacity domain of RFC 1 §8.2 —
// store-wide counters, no I/O under p.mu, no lock acquired while it is held,
// and never held across a Maker or a Set call: those take locks of this
// same domain, and no two locks of one domain are held at once.
// spareRefusals is a store-wide counter of the same domain kept in an atomic
// without p.mu, for the roll counts it outside any lock the pool might hold.
// Withdrawn the day a recorder of the domain's order finds p.mu held across
// one of those calls.
type Pool struct {
	m   *Maker
	t   Targets
	log *slog.Logger
	// readOnly is the journal's read-only check, asked before each spare is
	// made; immutable after construction, so it names no lock domain. Nil
	// means writable.
	readOnly func() error

	mu         sync.Mutex
	ready      [Reserve + 1][]core.SegmentID // the ready spares per class, FIFO; guarded by mu
	held       int                           // spares owed to open holds; guarded by mu
	pending    int                           // the largest shortfall a refused Hold still asks, a level not a sum; guarded by mu
	incoming   [Reserve + 1]int              // recycles in flight, reserved per class; guarded by mu
	started    bool                          // guarded by mu
	stopped    bool                          // guarded by mu
	deviceFull bool                          // guarded by mu
	retirer    *Retirer                      // the source of recycled spares, nil when none; guarded by mu

	// parked counts the WaitSpares calls sitting on the settle condition,
	// for the test that sequences an event against a wait it must confirm is
	// parked rather than racing its arrival. Guarded by mu.
	parked int

	// settled wakes WaitSpares, broadcast by every refill pass on its way
	// out and by Stop. filling says a pass is running or a kick is queued:
	// true from the moment work is promised — Start's own first pass, the
	// kick any take or retirement enqueues — until the pass it promised ends
	// at a settle. A waiter parks while filling, and reads a pool that holds
	// no promised or running pass however it likes: one short of its targets
	// then is the pool's own last word, whatever pass last left it there,
	// and however long ago. Guarded by mu.
	settled *sync.Cond
	filling bool

	// spareRefusals counts each roll the pool refused naming `spare`. The
	// pool never raises it itself: only the roll does, because only the
	// roll knows the final answer — a header-only record's missed content
	// take, served by the headroom, is no refusal.
	spareRefusals atomic.Int64

	kick chan struct{}
	done chan struct{}
	stop sync.Once
	wg   sync.WaitGroup
}

// NewPool returns a pool that makes its spares with m and keeps each class of
// t ready, asking readOnly before each spare it makes: a non-nil error from it
// is a journal that is read-only, which makes none. A nil Maker is a caller bug
// and panics; a negative target names storage no class can hold and panics the
// same way; a nil readOnly means writable; a nil logger keeps the pool silent.
func NewPool(m *Maker, t Targets, readOnly func() error, log *slog.Logger) *Pool {
	if m == nil {
		panic("segment: a pool needs its Maker")
	}
	if t.Content < 0 || t.Headroom < 0 || t.Reserve < 0 {
		panic(fmt.Sprintf("segment: the spare targets %+v are not sizes classes can hold", t))
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Pool{
		m:        m,
		t:        t,
		readOnly: readOnly,
		log:      log,
		kick:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	// The settle condition on the pool's own mutex, so a settle broadcast
	// takes no second lock of the capacity domain.
	p.settled = sync.NewCond(&p.mu)
	return p
}

// Adopt accounts a spare found at open into class c: the pool calls the
// ledger, which accounts the file the directory already holds at its cap,
// and then holds the spare ready in its class, counted toward the headroom,
// the repack reserve or a stream's next segment. It is refused once Start has
// run, because open syncs the directory once before any re-adopted spare is
// taken, and the caller of Start runs that sync before it: a spare adopted
// after could be taken while its entry is one a power cut could still lose.
//
// The id must already be below the Maker's seed; the pool does not check that,
// for the seed that hands an id above the directory's names out has already
// broken what keeps a live segment and a fresh spare from meeting under one
// name, and no check here could hold it back together.
func (p *Pool) Adopt(id core.SegmentID, c Class) error {
	if c != Content && c != Headroom && c != Reserve {
		return ErrBadClass
	}

	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if started {
		return ErrStarted
	}
	if err := p.m.set.AdoptSpare(id); err != nil {
		return fmt.Errorf("segment: the pool did not adopt spare %d: %w", uint64(id), err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// A Start that raced between the check and the ledger call has reached
	// its takes without the directory sync the rule above stands on, so this
	// spare is refused its class: it stays accounted and on disk, and the
	// next open re-adopts it. Discarding it instead would strand a cap the
	// ledger already owns, and returning it through DropSpare would unaccount
	// a file that exists.
	if p.started {
		return ErrStarted
	}
	p.ready[c] = append(p.ready[c], id)
	return nil
}

// Start marks the pool started and launches the refill worker, kicking it
// once so an empty pool fills. It is idempotent, and a no-op after Stop.
//
// Start is called only once open's directory sync has returned: until it has,
// re-adopted spares may not be taken, and the pool refuses every Take and
// Hold until Start runs.
func (p *Pool) Start() {
	p.mu.Lock()
	if p.started || p.stopped {
		p.mu.Unlock()
		return
	}
	// The first pass is the pool's own: a waiter arriving between this kick
	// and the worker's relaying of it parks for the first settle rather than
	// reading a pool that has not tried once as short.
	p.filling = true
	// the worker is registered under mu, so a Stop racing Start cannot set
	// stopped, close done and return from its Wait before the worker it must
	// join exists. The goroutine starts under mu too: no lock of the domain
	// is held across anything but this bookkeeping.
	p.started = true
	p.wg.Add(1)
	go p.worker()
	p.mu.Unlock()

	p.Kick()
}

// Take returns the oldest ready spare of class c into the set, as the segment
// a stream appends to. Making a segment happens off the write path, so a
// roll costs only the take: a Take that finds its class empty makes no seam
// call, waits on nothing and returns at once the NoSpaceError the spare pool
// owes — Limit LimitSpare, one segment missing, the segments unit. Spares
// held for an open hold are not available to an unreserved take, so an open
// reservation is never left short of its spares.
//
// A class never serves another's roll: a content roll and a Reserve never
// take one, and one that finds only those is refused `spare`.
func (p *Pool) Take(c Class) (core.SegmentID, error) {
	defer p.Kick()

	if c != Content && c != Headroom && c != Reserve {
		return 0, ErrBadClass
	}
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return 0, core.ErrClosed
	}
	if !p.started {
		p.mu.Unlock()
		return 0, &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
	}

	// decision: a take of a class falls back to no other, because the
	// headroom and the reserve are what keep a full journal able to free
	// space, and a class that borrowed one would leave the reserve that
	// draws on it short.
	var id core.SegmentID
	switch c {
	case Content:
		// The unreserved take leaves the holds their spares: taking one an
		// open hold owes would let a reserved write meet the no-spare
		// refusal the hold exists to keep it from.
		if int64(len(p.ready[Content])) > int64(p.held) {
			id = p.popLocked(Content)
		}
	default:
		// Headroom and Reserve serve the same ready test: neither is
		// diminished by a hold, and neither lends to another class.
		if len(p.ready[c]) > 0 {
			id = p.popLocked(c)
		}
	}
	p.mu.Unlock()

	if id == 0 {
		return 0, &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
	}
	if err := p.m.set.Take(id); err != nil {
		// The take popped the spare off the ready list above, its class saw
		// it go, and the ledger that cannot find it answers for the rest:
		// the pool and the ledger both hold no spare the other lost.
		return 0, fmt.Errorf("segment: the pool took spare %d: %w", uint64(id), err)
	}
	return id, nil
}

// Hold reserves n content spares for a reservation — "the spare segments its
// own bytes roll into, one per cap they can cross plus one, taken from the
// pool for the reservation" — holding them ahead of need: the spares it holds
// are not available to unreserved takes, so a reserved write never meets the
// no-spare refusal while the pool can be refilled. n must be positive.
func (p *Pool) Hold(n int) (*Hold, error) {
	if n <= 0 {
		panic(fmt.Sprintf("segment: a hold of %d spares reserves nothing a roll could take", n))
	}
	defer p.Kick()

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, core.ErrClosed
	}
	if !p.started {
		p.mu.Unlock()
		return nil, &core.NoSpaceError{Limit: core.LimitSpare, Missing: int64(n), Unit: core.UnitSegments}
	}
	// The spare refusal is the one that clears on its own: a Hold that falls
	// short records its shortfall as the demand the refill makes up, so the
	// retry finds it served. Held spares ride above the content target
	// rather than inside it — the target exists so every stream can roll
	// once without waiting for a spare — so the refill is what keeps an open
	// reservation from consuming the spares the unreserved rolls need.
	//
	// The demand is a level, the largest shortfall one refused ask still
	// wants, never a running sum: the same hold refused ten times before a
	// make lands is one shortfall of one spare, and summing the refusals
	// would make ten spares that then sit above the target for good. A
	// second, smaller ask rides inside a larger outstanding one, so the
	// level is the max. A served Hold clears the level: the ask that made
	// it found its spares, and whatever demand another hold still holds is
	// re-recorded by its own next refused retry, which is the loop the
	// level exists to serve.
	ready := int64(len(p.ready[Content]))
	if ready-int64(p.held) < int64(n) {
		missing := int64(n) - (ready - int64(p.held))
		if missing > int64(n) {
			// A replace can leave held above ready, and the refusal never
			// blames a shortage past the hold it refuses: capped at n.
			missing = int64(n)
		}
		if int(missing) > p.pending {
			p.pending = int(missing)
		}
		p.mu.Unlock()
		return nil, &core.NoSpaceError{Limit: core.LimitSpare, Missing: missing, Unit: core.UnitSegments}
	}
	p.pending = 0
	p.held += n
	p.mu.Unlock()
	return &Hold{p: p, left: n}, nil
}

// Hold is a reservation's share of the pool's content spares. Its Take is a
// reserved roll: it takes a held spare ahead of every unreserved write.
// Replace returns one to the reservation once its write stopped short of an
// interleaved roll, and Close returns what is left. A closed hold is spent:
// its Take returns core.ErrClosed, its Replace a no-op. The hold's fields
// are guarded by its pool's mu, the capacity domain its type names.
type Hold struct {
	p      *Pool
	left   int
	closed bool
}

// Take takes one of the hold's spares: a reserved roll, which goes ahead of
// every unreserved write. A hold whose left has run out behaves as an
// unreserved content take, and a hold whose pool has gone short after a
// Replace — no content spare ready — is refused `spare` like one.
func (h *Hold) Take() (core.SegmentID, error) {
	defer h.p.Kick()

	// One critical section decides the whole take: the sections are never
	// released between the guards and the pop, so neither a Stop nor a
	// concurrent Close of this hold can land inside the take and change what
	// the first guard read. All the guards that matter to the answer run
	// under the one hold of the lock.
	h.p.mu.Lock()
	if h.closed || h.p.stopped {
		h.p.mu.Unlock()
		return 0, core.ErrClosed
	}
	if h.left > 0 && len(h.p.ready[Content]) > 0 {
		// A replace can leave the pool owing more spares than it holds, so
		// this take tests only that one is ready — not the len > held an
		// unreserved take is refused under — because the spare it takes is
		// this hold's own demand, the exact one the replace stands for, taken
		// ahead of every unreserved write.
		h.left--
		h.p.held--
		id := h.p.popLocked(Content)
		h.p.mu.Unlock()

		if err := h.p.m.set.Take(id); err != nil {
			return 0, fmt.Errorf("segment: the hold took spare %d: %w", uint64(id), err)
		}
		return id, nil
	}
	spent := h.left <= 0
	h.p.mu.Unlock()

	if spent {
		// The hold spent its own spares and takes as an unreserved one does:
		// one rule for every unreserved take, kept in one place — and the
		// unreserved take rechecks the stopped flag this take released the
		// lock before.
		return h.p.Take(Content)
	}
	return 0, &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
}

// Replace returns one spare to the hold: its reserved write was rolled short
// by the stream's other writes instead of its own, so the reservation takes
// a replacement ahead of every unreserved write. It raises the hold without
// refusing, even against a pool with no spare ready: the unreserved takes are
// then refused until the refill catches up. When a reserved roll was caused
// by interleaving is not the pool's to know — the roll, or the caller above
// it, decides and calls Replace.
func (h *Hold) Replace() {
	h.p.mu.Lock()
	if h.closed {
		h.p.mu.Unlock()
		return
	}
	h.left++
	h.p.held++
	h.p.mu.Unlock()

	h.p.Kick()
}

// Close returns the hold's left spares to unreserved takes. It is idempotent,
// and safe after the pool's Stop, which leaves them accounted and on disk
// for the next open to re-adopt.
func (h *Hold) Close() {
	h.p.mu.Lock()
	if h.closed {
		h.p.mu.Unlock()
		return
	}
	h.closed = true
	h.p.held -= h.left
	h.left = 0
	h.p.mu.Unlock()
}

// wantRecycle reserves a slot for a recycled spare in the first class short of
// its target, in the refill's own order — headroom, then reserve, then
// content, whose target the holds and the refused asks ride above — counting
// the ready spares and the reservations in flight together. It answers false
// when no class is short or the pool is stopped, and the reservation it takes
// when it answers true is what recycled spends and recycleFailed gives back.
//
// It is allowed before Start: open's emptying retires segments before the
// pool starts, and a recycle syncs its own rename's directory entry, so the
// spare a recycle lands is durable whatever the pool's own start sync was
// for — the re-adopted spare's case Adopt stands on.
//
// ponytail: a make in flight is invisible to the reservation. refill picks
// its class, sets the slot aside and runs Make outside the mutex, and over
// that window wantRecycle still counts the class short, so a recycle can
// reserve a slot a make is already filling and land the pool one spare over
// its target. Ceiling: one spare per overlap, held ready and re-adopted at
// the next open rather than spent. Upgrade to a making-class field counted
// beside incoming, once a trace shows an overlapped recycle costing storage
// that mattered.
//
// decision: any class short of its target wants the recycle, not the content
// class alone. "The pool is below its target" is one rule for the three
// classes a spare serves, and RFC 28 §2.2 names only `journal.spare_target`
// without saying which class it sizes; a recycle that could not fill a short
// headroom or reserve would leave the journal one spare less able to free
// space, for a whole segment's worth of no new allocation. Withdrawn if the
// RFC's author restricts recycling to the content class, which these two
// methods then answer for Content alone.
func (p *Pool) wantRecycle() (Class, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		return 0, false
	}
	switch {
	case len(p.ready[Headroom])+p.incoming[Headroom] < p.t.Headroom:
		p.incoming[Headroom]++
		return Headroom, true
	case len(p.ready[Reserve])+p.incoming[Reserve] < p.t.Reserve:
		p.incoming[Reserve]++
		return Reserve, true
	case len(p.ready[Content])+p.incoming[Content] < p.t.Content+p.held+p.pending:
		// Content counts what holds demand and what a refused Hold asked
		// above the target, exactly as the refill does: a recycle filling a
		// slot is worth what a made spare in it is.
		p.incoming[Content]++
		return Content, true
	}
	return 0, false
}

// recycled puts a recycled spare — already in the ledger as a spare under its
// fresh id — into class c, spending the reservation wantRecycle took, worth
// what a made spare in the class is: the content class pays the demand level
// down by the same clamped one a made content spare pays, so a refusal the
// recycle served is not served a second time. After Stop it is still
// appended, accounted and on disk, as refill's own made spare is: the next
// open re-adopts it.
func (p *Pool) recycled(id core.SegmentID, c Class) {
	p.mu.Lock()
	p.incoming[c]--
	p.ready[c] = append(p.ready[c], id)
	// The clamp and the ceiling are refill's own: a landed content spare
	// above the target and the open holds pays one unit of demand, and only
	// while a level stands.
	if c == Content && p.pending > 0 && len(p.ready[Content]) > p.t.Content+p.held {
		p.pending--
	}
	p.mu.Unlock()
}

// recycleFailed gives a recycle's reservation back: the class the recycle
// could not fill is short again, and the kick lets the worker make a spare
// for it instead.
func (p *Pool) recycleFailed(c Class) {
	p.mu.Lock()
	p.incoming[c]--
	p.mu.Unlock()

	p.Kick()
}

// Kick wakes the refill worker, coalescing: one kick that arrives while a
// pass runs holds the next. The worker checks done between its makes, so a
// kick after Stop is a drop. The enqueue runs under mu and marks the pass it
// promises, so a waiter arriving between a kick and the pass it asked for
// parks for that pass's settle rather than reading the pool as idle and
// short; a kick dropped on a stopped pool promises nothing.
func (p *Pool) Kick() {
	p.mu.Lock()
	if !p.stopped {
		p.filling = true
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}
	p.mu.Unlock()
}

// ReadyContent returns the content spares an unreserved take can still reach:
// the ready ones less the spares owed to open holds, zero when the difference
// is empty. It takes the pool's lock briefly and makes no I/O, so it costs
// only the read — the same domain Take and Hold decide in, whose outcome a
// snapshot of this value approaches but cannot stand in for.
func (p *Pool) ReadyContent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return max(len(p.ready[Content])-p.held, 0)
}

// DeviceFull reports whether a make has failed with ENOSPC and no spare has
// been made since — "making a spare failed with ENOSPC" being the state a
// roll that finds no full-size retired segment to hand answers ENOSPC with,
// until a spare is made again. A failure of another kind after the ENOSPC
// does not clear it: no full-size spare has appeared. The other half of that
// state — whether a retirement has since handed one — is the retirement
// executor's, not the pool's.
func (p *Pool) DeviceFull() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deviceFull
}

// countSpareRefusal raises the spare-refusal counter. The pool never calls
// it: only a roll that drew its final answer `spare` does, for a missed
// content take that a fallback served is no refusal.
func (p *Pool) countSpareRefusal() {
	p.spareRefusals.Add(1)
}

// Counters returns the pool's maintained counters: one lock and a copy, the
// slice lengths read as maintained state, no walk of any class.
func (p *Pool) Counters() PoolCounters {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolCounters{
		Spares:             int64(len(p.ready[Content])),
		HeadroomBytes:      int64(len(p.ready[Headroom])) * p.m.set.cap,
		RepackReserveBytes: int64(len(p.ready[Reserve])) * p.m.set.cap,
		Held:               int64(p.held),
		SpareRefusals:      p.spareRefusals.Load(),
	}
}

// WaitSpares blocks until every class holds the spares its target names,
// ready to take — or the pool reports it cannot reach its targets: no pass
// is running, none is queued, and a class still holds fewer ready spares
// than its target — or the pool has stopped. It returns nil once no class is
// ready-short, core.ErrClosed once the pool has stopped, the context's error
// once it is done — checked before every other answer — and the spare
// refusal naming LimitSpare when the pool stands idle and short. Held and
// pending spares ride above the targets and the wait does not count them,
// and neither does it count a recycle reserved but not landed: its failure
// leaves the class short with no wake, so the wait reads only what a roll
// can take.
//
// decision: the wait is event-driven, woken by settle alone. The mechanism is
// a condition under the pool's own mutex, broadcast by every refill pass on
// its way out and by Stop, and the fill the pool promises — a pass a kick has
// queued — is marked before any waiter can read the pool as idle, so a wait
// parks for the settle of the pass it raced and never answers a queued
// refill as a shortage. A wait over a pool already idle reads its state and
// returns without parking at all, and no polling measures anything.
//
// decision: the wait guarantees one thing only — that every class holds its
// target of ready spares, and that the pool is not mid-fill when it says so.
// It is not a promise the caller's next roll lands: a hold that claimed all
// the ready spares makes an unreserved take refuse with the pool at its
// targets, and a recycle reserved in flight can leave a class short again
// the moment its settles — the wait reads ready counts, not reservations, so
// it can answer nil while a waiter's roll is refused either way. Neither
// deadlock nor hang comes of it: a refusal is the caller's retry, and the
// next settle answers the move. Overturned only by a caller that needs the
// wait to promise the roll: then the predicate must count what holds and
// recycles claim, and the recycle paths must settle.
//
// decision: the wait is a test-facing surface only. The journal whose writes
// are refused for want of a spare answers retry, never waits (RFC 1 §3), so
// nothing production reaches this. Withdrawn together with a both-ends
// answer — a Stats that reports the ready spares, or a WriteAt that waits —
// by whoever needs one; the other end then carries it.
func (p *Pool) WaitSpares(ctx context.Context) error {
	p.mu.Lock()
	switch {
	case p.stopped:
		p.mu.Unlock()
		return core.ErrClosed
	case !p.started:
		p.mu.Unlock()
		return &core.NoSpaceError{Limit: core.LimitSpare, Missing: 1, Unit: core.UnitSegments}
	}

	// A done context is answered by its wake: the waiter below sends one on
	// Done, and the loop's first check of it returns. The waiter runs until
	// the wait returns, so a wake it sends late — after the answer a settle
	// already carried — finds nobody parked and is gone by its return.
	answered := make(chan struct{})
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				p.mu.Lock()
				p.settled.Broadcast()
				p.mu.Unlock()
			case <-answered:
			}
		}()
	}

	for {
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			close(answered)
			return err
		}
		// decision: the context's error is answered first on every wake —
		// including a wake a cancel sent over a state the pool changed
		// since — because a caller that stopped waiting is done whatever
		// the pool did. A stopped pool answers ErrClosed before any settle
		// the stop itself sent, so a waiter woken by a stop reads the stop
		// and not the pool's last-word shortage: the wait is over, and a
		// caller that retried on a shortage would retry a pool that is gone.
		if p.stopped {
			p.mu.Unlock()
			close(answered)
			return core.ErrClosed
		}
		if !p.readyShortLocked() {
			p.mu.Unlock()
			// A pass that filled while the caller was arriving still counts:
			// the wait answer is the pool's word, not the caller's timing.
			// The ready counts, not the refill's, are what it reads: a spare a
			// roll can take is the wait's whole subject.
			close(answered)
			return nil
		}
		if !p.filling {
			// No pass is running and none is queued, and the pool holds
			// fewer ready spares than its targets name: the pool's own word
			// for where its fill stopped — whether the wait arrives before
			// the settle's wake or after it — is the refusal to answer, for
			// a caller that cannot wait for a refill nothing queued will
			// run. The next kick refills, and a caller that wants to ride it
			// calls again.
			refusal := &core.NoSpaceError{Limit: core.LimitSpare, Missing: max(p.shortAtLocked(), 1), Unit: core.UnitSegments}
			p.mu.Unlock()
			close(answered)
			return refusal
		}
		p.parked++
		p.settled.Wait()
		p.parked--
	}
}

// readyShortLocked reports whether the pool holds, ready to take, fewer
// spares than every class's target — the count a wait answers by, which the
// refill's own shortness does not: a recycle reserved in flight fills a class
// without holding it, and its failure leaves the class short again with no
// settle to wake a waiter that had read the reservation as a spare. A wait
// reads the spares a roll can take now, and the next settle — the failed
// recycle's, or a pass's — answers whatever moved. The caller holds mu.
func (p *Pool) readyShortLocked() bool {
	switch {
	case len(p.ready[Headroom]) < p.t.Headroom:
		return true
	case len(p.ready[Reserve]) < p.t.Reserve:
		return true
	case len(p.ready[Content]) < p.t.Content:
		return true
	}
	return false
}

// shortAtLocked returns how many segments a settled-short pool still owes
// across its classes, counted as the refill chooses them. The caller holds mu.
func (p *Pool) shortAtLocked() int64 {
	var missing int64
	if d := p.t.Headroom - len(p.ready[Headroom]); d > 0 {
		missing += int64(d)
	}
	if d := p.t.Reserve - len(p.ready[Reserve]); d > 0 {
		missing += int64(d)
	}
	if d := p.t.Content - len(p.ready[Content]); d > 0 {
		missing += int64(d)
	}
	return missing
}

// settle records a refill pass on its way out — or a Stop — as the pool's
// settle event, waking every waiter: whatever a pass left the pool at is now
// what a waiter reads. A kick the pass leaves queued still promises the pass
// it asked for, so the pool settles as filling only when its kick channel is
// empty: a waiter arriving between a settle and the pass the queued kick
// will run parks for that pass, and is not answered as a shortage the queued
// retry will lift. It takes mu, so no caller of it holds it.
func (p *Pool) settle() {
	p.mu.Lock()
	p.filling = len(p.kick) > 0
	p.settled.Broadcast()
	p.mu.Unlock()
}

// settleLocked is settle for a caller that holds mu: the broadcast reaches the
// waiters before the state the caller built is let go of.
func (p *Pool) settleLocked() {
	p.filling = len(p.kick) > 0
	p.settled.Broadcast()
}

// Stop closes the pool and joins the worker. A make in flight completes
// first, and its spare is held — accounted, on disk — for the next open to
// re-adopt; Stop removes nothing, and the untaken spares stay in the ledger
// and on disk. After Stop, Take and Hold return core.ErrClosed, and Start
// and Kick are no-ops.
func (p *Pool) Stop() {
	p.stop.Do(func() {
		p.mu.Lock()
		p.stopped = true
		p.settleLocked()
		p.mu.Unlock()
		close(p.done)
	})
	p.wg.Wait()
}

// worker is the refill: on each kick it makes spares, one at a time, until
// every class is at its target or a make fails — then it waits for the next
// kick, which every take, refused or not, sends. Making a segment never
// reaches the write path; making spares here is what keeps that true.
//
// ponytail: retries are demand-driven. A failed make is retried on the next
// kick — every take, refused or not, every replacement, the kick a
// retirement makes and the one the retirement queue's draining makes — with
// no timer of its own. Ceiling: an idle journal
// whose last make failed makes no spare until something takes or retires,
// and a failing device sees one make attempt per refused roll, coalesced by
// the one-slot kick. A read-only journal stays so until it is closed and
// reopened, and the reopen builds a new pool, so a pool never sees the check
// clear and no read-only interval is retried. Upgrade to a timed retry on the
// pool's clock once spare refusals show them persisting on an idle device
// that has recovered.
func (p *Pool) worker() {
	defer p.wg.Done()

	for {
		select {
		case <-p.done:
			return
		case <-p.kick:
			if !p.refill() {
				return
			}
		}
	}
}

// refill runs one pass. It returns false once the pool is stopped, so the
// worker exits; true otherwise, whether or not the pass made a spare.
func (p *Pool) refill() bool {
	for {
		p.mu.Lock()
		if p.stopped {
			p.settleLocked()
			p.mu.Unlock()
			return false
		}
		// decision: the headroom and the reserve fill before the content
		// class, because they are what keeps a full journal able to free
		// space — the records that return space roll into them — so a pass
		// that made a content spare while a reserve sat short would trade a
		// permanent wedge for one more served roll. Withdrawn once spare
		// refusals show content rolls refused while a reserve was filling.
		var c Class
		switch {
		case len(p.ready[Headroom])+p.incoming[Headroom] < p.t.Headroom:
			c = Headroom
		case len(p.ready[Reserve])+p.incoming[Reserve] < p.t.Reserve:
			c = Reserve
		case len(p.ready[Content])+p.incoming[Content] < p.t.Content+p.held+p.pending:
			// Content fills above its target by what holds demand and what a
			// refused Hold asked: held spares are beside the target, not
			// inside it. The recycles in flight count too: the worker makes
			// no spare for a slot a recycle is filling.
			c = Content
		default:
			p.settleLocked()
			p.mu.Unlock()
			return true
		}
		p.mu.Unlock()

		// decision: a read-only journal makes no spare, because RFC 1 §5.4
		// says it "takes no spare" and a read-only journal writes nothing it
		// need not. The check runs per make rather than once per pass, so a
		// journal that turns read-only mid-pass stops at the next make, and
		// it is asked only once a class is short — the class has been chosen
		// above — so a pool already at its target never asks. It is asked
		// holding no lock: it is the journal's own policy, not a function of
		// the capacity domain, and no lock of the domain may be held across
		// it. The one spare a read-only journal does make, the probe's (RFC 1
		// §5.4), is made through Maker.Make, not this refill, so this does not
		// stop it. Withdrawn if that spare ever has to come from the pool, or
		// if the journal ever needs the refill to run while read-only.
		if p.readOnly != nil {
			if err := p.readOnly(); err != nil {
				p.log.Debug("segment: the pool made no spare: the journal is read-only",
					"err", err)
				// The pass ends rather than the worker: a read-only journal
				// stays so until it is reopened, which builds a new pool, so
				// the worker idles until Stop rather than exiting early.
				p.settle()
				return true
			}
		}

		// decision: the pool grows only when no full-size retired segment is
		// to hand (RFC 28 §2.2, RFC 1 §6): a queued full-sealed segment is
		// recycled into this very slot, so the pass makes nothing and the
		// retirement queue's emptying, or an unlink's own kick, starts the
		// next. Ceiling: one queued full segment stalls every make even when
		// the pool is short by more than one, and so does a queued segment
		// whose claim another path holds, for as long as that claim lasts, a
		// queued segment held by a read-only interval, and one whose
		// retirement keeps failing and is requeued. The longest stall is
		// the queue's order: it drains in order, so a full segment queued
		// behind N early-sealed ones (each an unlink with a directory sync),
		// or held by a reader through the cache's Close, stops every make
		// until the worker reaches it, and each unlink's kick finds the full
		// set non-empty and returns; after a large repack N can be hundreds.
		// Upgrade: queue the full ids ahead of the early ones.
		// Withdrawn when spare refusals show rolls refused while a queued
		// recycle stays stuck.
		if p.retiredToHand() {
			p.settle()
			return true
		}
		// Make owns the pairing: an error returns no id, so the appends
		// below never see an id of 0 named by a failure.
		id, err := p.m.Make()
		p.mu.Lock()
		if p.stopped {
			// A made spare stays accounted and on disk; the next open
			// re-adopts it, so it is appended to the class the pass chose even
			// now. A failed one returned its reservation in its own cleanup
			// and appended nothing — an id of 0 would name no segment a take
			// could reach.
			if err == nil {
				p.ready[c] = append(p.ready[c], id)
			}
			p.settleLocked()
			p.mu.Unlock()
			return false
		}
		if err != nil {
			// The state a roll reads as a full device is set by the ENOSPC
			// of a failed make and cleared only by a make that succeeds: a
			// failure of any other kind after it leaves the state standing,
			// for no spare has been made since the device filled.
			if errors.Is(err, syscall.ENOSPC) {
				p.deviceFull = true
			}
			p.mu.Unlock()

			// The journal maximum's own refusal is the pool working as
			// configured; anything else is a fault to name.
			var ns *core.NoSpaceError
			if errors.As(err, &ns) && ns.Limit == core.LimitJournal {
				p.log.Debug("segment: the pool did not make a spare", "err", err)
			} else {
				p.log.Warn("segment: the pool did not make a spare", "err", err)
			}
			p.mu.Lock()
			p.settleLocked()
			p.mu.Unlock()
			return true
		}
		p.ready[c] = append(p.ready[c], id)
		// The level is clamped at zero: a spare above the target and every
		// open hold's demand pays the level down by one, but only while a
		// level stands. A make the level drew lands after the hold that
		// raised it closed — Close lowering held under it — and that spare
		// is the target's own need, not spent demand; paying the level for
		// it would run the level negative and leave the pool filling one
		// spare below its target until something else touched the level.
		if c == Content && p.pending > 0 && len(p.ready[Content]) > p.t.Content+p.held {
			p.pending--
		}
		p.deviceFull = false
		p.mu.Unlock()
	}
}

// popLocked returns the oldest spare of class c, removing it from the ready
// list; the caller holds mu and has checked the list is not empty.
func (p *Pool) popLocked(c Class) core.SegmentID {
	id := p.ready[c][0]
	p.ready[c] = p.ready[c][1:]
	if len(p.ready[c]) == 0 {
		// Release the slice's hold on the popped id, so a long-lived pool
		// does not pin one id's storage per take.
		p.ready[c] = nil
	}
	return id
}
