package capacity

import (
	"fmt"
	"sync/atomic"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/lockorder"
)

// RecordCharge is what one record without bytes costs the headroom: the
// largest of them with its footer entry — a 96-byte header, a maximum
// 56-byte payload (a clone target's spec) and a 72-byte footer entry — one
// charge for every class, since a segment cannot seal until its footer
// names every record in it.
const RecordCharge = 224

// RecordClass is which record without bytes an admission is for. The classes
// split three ways at the admission table: what frees nothing (Hold, Stamp,
// CloneTarget), what an open appends to its own reserve (Open), and the
// repack copy, which draws on the repack reserve, not the headroom. Seal, the
// marker a share-scoped repack closes an active segment with, is a class that
// returns space, so it draws like a release does.
type RecordClass uint8

const (
	Release RecordClass = iota + 1
	Truncate
	Deallocate
	Delete
	CloneTarget
	Offloaded
	Unmark
	Loss
	Hold
	Unhold
	Stamp
	Forget
	Reconciled
	Open
	RepackCopy
	Seal
)

var classNames = [...]string{
	"", "release", "truncate", "deallocate", "delete", "clone_target",
	"offloaded", "unmark", "loss", "hold", "unhold", "stamp", "forget",
	"reconciled", "open", "repack_copy", "seal",
}

// String returns the class's name, or its number for a value the table does
// not hold.
func (c RecordClass) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return fmt.Sprintf("RecordClass(%d)", uint8(c))
}

// OpenReserveRecords returns the open reserve: the last 4,096 records of the
// headroom, or half of it where headroomRecords is below 8,192. Only the
// header-only records an open appends before it serves draw on it.
func OpenReserveRecords(headroomRecords int) int {
	if headroomRecords < 8192 {
		return max(headroomRecords/2, 0)
	}
	return 4096
}

// HeadroomConfig is the facts a Headroom is built over: the plain-func
// snapshots of the wiring, set once at construction, and the journal's
// constants. Every func is required; a nil one panics at construction, which
// is a wiring bug and reads as one.
//
// decision: the funcs carry no lock of the capacity domain. The set's read
// is a lock-free snapshot of a counter the wiring owns; the drawn figure
// below is this package's own, so the check and the draw are one section
// that holds no other layer's lock.
type HeadroomConfig struct {
	// Records is the configured headroom, in records without bytes.
	Records int

	// Footprint reports the journal's accounted used bytes: every segment
	// and every spare at its cap.
	Footprint func() int64

	// MaxBytes is the journal's configured maximum, in bytes. Zero or
	// negative bounds nothing.
	MaxBytes int64

	// SegmentCap is the size a segment is accounted at, in bytes.
	SegmentCap int64

	// Admit is the read-only check: a non-nil error is the sentinel that
	// journal-wide state carries, and an admission is refused with it at
	// once, consulting no limit and moving no counter.
	Admit func() error
}

// Headroom is the admission table for records without bytes and the
// counter they move. It is safe from any goroutine.
//
// decision: the headroom is measured by its ledger alone: its size is the
// configured Records x RecordCharge and what is left is that size less the
// records drawn while the journal was at its limit. The spare pool is not
// consulted, because it counts only the headroom spares ready now: a
// header-only roll that takes the headroom spare drops that figure to zero
// until the refill lands, and a measure that followed it would refuse every
// class, an open's records included, at the moment the journal needs them.
// Overturned by a journal whose headroom spares are lost to something other
// than a roll, which this ledger would not see.
//
// ponytail: the check and the draw share one mutex, so every
// header-only record crosses it briefly and no I/O runs under it. Upgrade
// to per-class atomics with a compare-and-swap loop on the drawn figure
// once a profile of a header-only-heavy load shows the mutex.
type Headroom struct {
	cfg     HeadroomConfig
	reserve int // the open reserve, in records, fixed at construction

	// decision: mu is the capacity domain, rank capacity in the lock order.
	// Nothing is acquired under it besides what a caller held: the at-limit
	// read, the below-half read and every admission hold no other lock
	// across it, and the check and the move in Drawn and Refund are the
	// section alone. A recorded wrong order under it must be reported,
	// not exempted.
	mu lockorder.Mutex
	// drawn is the record bytes the headroom has admitted and neither a
	// Refund nor a Credit has returned. Every check and move of it runs
	// under mu; it is an atomic only so Stats reads it without mu.
	//
	// decision: drawn lives in memory and every open starts it at zero, so a
	// restart gives back the whole drawing side. That departs from RFC 1
	// section 6, which promises only the open reserve whole at the next open:
	// on disk nothing tells a record drawn at the limit from the same kind
	// appended below it, so an open cannot recount the draws the segments it
	// adopted still hold. The footprint does not overrun for it: the pool
	// makes every spare inside the maximum, and a header-only record appended
	// into an existing segment moves no footprint. The ceiling runs the other
	// way. The headroom spares the drawn records rolled into before the
	// restart are segments now, which the pool cannot make again inside the
	// maximum, while the ledger reads whole; the open re-adopts what spares
	// are left into the repack reserve first (see segment.AdoptClasses), so
	// the headroom gets only those past the reserve's. So after a restart at
	// the limit Admit admits records whose roll — each stream's first after
	// the open, and any once its active segment fills — finds neither a
	// content nor a headroom spare, and the stream refuses it naming
	// LimitHeadroom, the refusal the headroom's per-stream sizing exists to
	// keep away, while HeadroomBytes reads nearly whole. It lasts until a
	// retirement frees a segment the pool remakes as a headroom spare, once
	// the repack reserve, which a retirement's recycle refills first, is
	// whole. The
	// open's own records are bounded by the open reserve in the recovery pass
	// itself; their loss records also draw here, by the Open class, at a
	// limit the open reached. Overturned by recomputing the ledger at open
	// from the headroom spares actually ready, by a ruling that the drawing
	// side persists across an open, or by a restart at the limit seen
	// refusing a roll that way in the field.
	//
	// decision: what the ledger drew comes back only through Credit, when a
	// retirement takes the segment that holds the records, and at the limit
	// only repack retires: the credit is worth what repack retires there. A
	// repack at the limit leaves a segment standing for records the liveness
	// tracker still counts, such as an unsettled removal's, so the headroom
	// those records drew comes back only once they settle and a later pass
	// retires the segment. Overturned by a repack pass at the limit that has
	// retired every sealed segment the liveness tracker does not hold and
	// still leaves HeadroomBytes below half.
	drawn atomic.Int64
	// bySeg is, per segment, the record bytes of drawn records that
	// appended into it: what Credit returns to drawn once the segment is
	// retired. Guarded by mu.
	//
	// ponytail: a draw whose segment is retired between its append and
	// its Drawn call stays in the ledger until the next open, the safe
	// side: it is never credited, never credited twice, and its entry names
	// a segment that is gone. The removal and forget paths register the
	// record before the segment can be retired, but the sealer's marker does
	// not: SealOpen leaves the segment Sealed before the sealer calls Drawn,
	// so a retirement tick in that gap leaves one record's charge stuck. The
	// loss path calls Drawn from the settle the durability layer's admission
	// hands back, once its append has returned — and that append includes
	// the record's sync, a group-commit wait — so its gap spans the wait, at
	// the same one-charge risk.
	// Upgrade by naming the segment at the admission if that charge ever
	// shows in the gauge.
	bySeg map[core.SegmentID]int64
	// draws is the headroom_draws counter, records appended while the
	// journal was at its limit. An atomic, so Stats reads it without mu.
	draws atomic.Uint64
}

// NewHeadroom returns a Headroom over cfg. A nil injected func is a wiring
// bug and panics here.
func NewHeadroom(cfg HeadroomConfig) *Headroom {
	if cfg.Footprint == nil || cfg.Admit == nil {
		panic("capacity: the headroom needs every injected fact")
	}
	return &Headroom{
		cfg:     cfg,
		reserve: OpenReserveRecords(cfg.Records),
		bySeg:   map[core.SegmentID]int64{},
	}
}

// SetRecorder installs the lock-order recorder the admission table's mutex
// reports to, with its capacity rank: the admission's mutex and the pool's
// are of the same domain, so no two of them are held at once. It runs
// before the Headroom is shared — before any concurrent Admit, Drawn or
// Refund — and a nil recorder leaves the mutex a plain one, so production
// behaviour is unchanged.
func (h *Headroom) SetRecorder(rec *lockorder.Recorder) {
	h.mu.Init(lockorder.Capacity, rec)
}

// Draw reports whether the caller appends the record it asked about. Pair
// each admitted draw with exactly one Drawn or one Refund: a token replayed
// double-counts or over-credits, and the clamp in Refund only floors it. An
// admission answers Draw{Append: false} only when the record must not be
// appended — an Offloaded record below half, whose bits are set in memory
// only, so the extents become evictable at once and a crash loses them,
// which is the safe direction.
type Draw struct {
	Append bool
	// drew names whether this admission moved the drawn figure — true only
	// while the journal was at its limit — so its Drawn and Refund hand
	// back exactly what it took: a below-limit admission drew nothing and
	// counts nothing, whichever way the limit moved in between.
	drew bool
}

// Admit consults the headroom for one record of class c, returning whether
// the caller appends it. A refusal is a *core.NoSpaceError naming the
// headroom — never a share's limit, the journal's maximum or the spare
// pool — and it moves no counter: the drawn figure a refusal leaves is the
// one the next admission reads.
//
// decision: below half of the headroom without the open reserve, the
// classes that free nothing (hold, stamp, clone target) are refused rather
// than draw, so what remains serves the records that return space; an
// Offloaded record is dropped rather than refused, because losing one
// costs a redundant upload, never content. The half kept for
// space-returning records, and the default of journal.headroom_records it
// is half of, are unmeasured: overturned by a journal held at its limit
// whose removals reach the refusal, or whose headroom sits mostly undrawn.
//
// decision: the open reserve is not part of what any class but Open
// finds — the headroom is exhausted once only the reserve is left, and the
// half above is half of the headroom without it — because no record
// appended while the journal serves draws on it: a journal that ran its
// headroom out before a crash still finds the reserve whole at the next
// open.
//
// decision: the headroom is drawn only while the journal is at its limit —
// the footprint plus a segment cap past the maximum, the same predicate
// headroom_draws counts by — because below it a record without bytes lands
// in a content spare, and burning the headroom on records the content pool
// already covered would drain the reserve of a healthy journal.
func (h *Headroom) Admit(c RecordClass) (Draw, error) {
	if err := h.cfg.Admit(); err != nil {
		return Draw{}, err
	}
	if c == RepackCopy {
		// The repack copy draws on the repack reserve, a reserve of its
		// own outside the headroom and every share's limit, so no
		// headroom figure can refuse it.
		return Draw{Append: true}, nil
	}
	// The at-limit read runs before the section: the set's lock is of this
	// same capacity domain, and no two locks of one domain are held at
	// once. Below the limit every class is admitted and draws nothing, and
	// so is every class of a headroom of no records, which has none to
	// measure.
	if h.cfg.Records <= 0 || !h.atLimit() {
		return Draw{Append: true}, nil
	}

	total := h.cfg.Records
	size := total - h.reserve // the drawing side: the headroom without the open reserve
	half := size / 2

	h.mu.Lock()
	free := total - recordsOf(h.drawn.Load())
	switch {
	case free <= h.reserve:
		// Exhausted for this class: only the open reserve is left, which
		// an open's own records still draw on, down to nothing.
		if c == Open && free > 0 {
			h.drawn.Add(RecordCharge)
			h.mu.Unlock()
			return Draw{Append: true, drew: true}, nil
		}
		h.mu.Unlock()
		return Draw{}, refuseHeadroom(1)
	case free-h.reserve < half:
		// Below half the drawing side: what frees nothing is refused, and
		// an Offloaded record is answered not-appendable.
		switch c {
		case Hold, Stamp, CloneTarget:
			h.mu.Unlock()
			return Draw{}, refuseHeadroom(1)
		case Offloaded:
			h.mu.Unlock()
			// The bits are set in memory only; no record is appended and
			// nothing is drawn.
			return Draw{Append: false}, nil
		}
	}
	h.drawn.Add(RecordCharge)
	h.mu.Unlock()
	return Draw{Append: true, drew: true}, nil
}

// Drawn counts one admitted record once it has appended into segment seg:
// the headroom_draws counter, raised for every draw that moved the ledger,
// so the two agree whichever way the footprint moved after the admission,
// and the segment's share of the ledger, which its retirement credits back.
// A record that drew nothing — below the limit, a repack copy, an Offloaded
// never appended — counts nothing. Every draw is attributed to the segment
// it appended into, an open's draw on the open reserve included, so its
// segment's retirement credits it back; a draw whose segment is 0, named by
// no append, is counted but attributed to none, so the ledger's per-segment
// figures never sum past drawn. Only tests that draw without appending pass
// 0: every production caller names the segment its record landed in, and
// the loss path refunds a record that landed nowhere instead.
func (h *Headroom) Drawn(d Draw, seg core.SegmentID) {
	if !d.drew {
		return
	}
	h.draws.Add(1)
	if seg == 0 {
		return
	}
	h.mu.Lock()
	h.bySeg[seg] += RecordCharge
	h.mu.Unlock()
}

// Credit returns to the headroom the draws that appended into seg, once seg
// is retired, unlinked or recycled: the space those records took is free
// again (RFC 1 section 6, the headroom refills as segments are unlinked).
// What an open drew of the open reserve comes back the same way, and the
// RFC's rule that regained headroom goes to the open reserve first holds
// through the one drawn total: the reserve is the last records of what is
// free, so a credit refills whatever was drawn of it before the drawing side,
// and Unused, which leaves the reserve out, sees any. The segment's entry is
// taken whole, so a second Credit of seg returns nothing.
func (h *Headroom) Credit(seg core.SegmentID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, ok := h.bySeg[seg]
	if !ok {
		return
	}
	delete(h.bySeg, seg)
	h.drawn.Store(max(h.drawn.Load()-n, 0))
}

// Refund returns one admission's draw when the append it admitted failed:
// the record never landed, so the headroom never spent it. It is a no-op
// for an admission that drew nothing — below the limit, a RepackCopy, or an
// Offloaded record below half, which was never appended at all — and it
// reverses nothing in headroom_draws, which counts only records that
// appended.
func (h *Headroom) Refund(d Draw) {
	if !d.drew {
		return
	}
	h.mu.Lock()
	h.drawn.Store(max(h.drawn.Load()-RecordCharge, 0))
	h.mu.Unlock()
}

// atLimit reports the one state in which the headroom is drawn: the journal
// at its limit, the footprint plus a segment cap past the maximum — the
// same predicate headroom_draws counts by. The footprint is a snapshot
// read outside any held mutex, and without one: the set's counters are
// atomics, so the read is lock-free. A read in the middle of a change errs
// high (a recycle raises the footprint before it lowers it), which keeps this
// gate closed rather than open at the limit.
func (h *Headroom) atLimit() bool {
	return h.cfg.MaxBytes > 0 && h.cfg.Footprint()+h.cfg.SegmentCap > h.cfg.MaxBytes
}

// Draws returns the headroom_draws counter: the records appended while the
// journal was at its limit.
func (h *Headroom) Draws() uint64 {
	return h.draws.Load()
}

// Spent returns the bytes of the headroom currently drawn.
//
// decision: this method is reached only from datapath tests: the
// recorded-lock fixture, which needs one entry point that takes the
// headroom's mutex with no precondition on the journal's footprint to prove
// its recorder is wired, and the loss-credit test, which reads the open
// reserve's draws that Unused leaves out. Nothing holds its signature up but
// those tests; withdraw it with them.
func (h *Headroom) Spent() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.drawn.Load()
}

// Unused returns the drawing side's unused reservation in bytes: the records
// not yet drawn, without the open reserve, at RecordCharge each. It is the
// figure BelowHalf halves, read at or below the limit alike, and zero for a
// headroom of no records. It names no allocated storage: the headroom's
// spare segments are counted where the set counts every segment.
//
// It takes no lock, so Stats never waits on an admission: a read racing a
// draw answers the figure before or after it.
func (h *Headroom) Unused() int64 {
	return int64(max(h.cfg.Records-recordsOf(h.drawn.Load())-h.reserve, 0)) * RecordCharge
}

// BelowHalf reports whether the headroom's drawing side has fallen below
// half its size without the open reserve — the content-write check this
// table consults beside its own classes. It reads true only while the
// journal is at its limit: headroom drawn during a past episode does not
// throttle content writes once the journal left the limit.
//
// decision: the gate is the same at-limit predicate the draw itself runs
// under, so the figure BelowHalf reports moves only with admissions the
// limit gated. It takes no lock: the footprint read holds none for the same
// domain reason as Admit's, and the drawn figure is one atomic read, as Unused's:
// Unused's own figure is not compared, because its floor at zero answers
// differently from this one where half the drawing side is zero records.
func (h *Headroom) BelowHalf() bool {
	if h.cfg.Records <= 0 || !h.atLimit() {
		return false
	}
	free := h.cfg.Records - recordsOf(h.drawn.Load())
	return free-h.reserve < (h.cfg.Records-h.reserve)/2
}

// refuseHeadroom is the named refusal a class that drew nothing hands its
// caller: ErrNoSpace naming the headroom, one entry short.
func refuseHeadroom(missing int64) *core.NoSpaceError {
	return &core.NoSpaceError{Limit: core.LimitHeadroom, Missing: missing, Unit: core.UnitEntries}
}

// recordsOf returns the whole records a byte figure names, floored.
func recordsOf(bytes int64) int {
	if bytes <= 0 {
		return 0
	}
	return int(bytes / RecordCharge)
}
