package capacity

import (
	"fmt"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
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
// repack copy, which draws on the repack reserve, not the headroom.
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
)

var classNames = [...]string{
	"", "release", "truncate", "deallocate", "delete", "clone_target",
	"offloaded", "unmark", "loss", "hold", "unhold", "stamp", "forget",
	"reconciled", "open", "repack_copy",
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

	mu sync.Mutex
	// drawn is the record bytes the headroom has admitted and no Refund
	// has returned; guarded by mu.
	//
	// ponytail: drawn only grows until a retirement returns headroom. With
	// the default 65,536 records, about 30,720 cumulative at-limit draws put
	// the journal below half, so content writes are refused each time it
	// returns to the limit, and about 61,440 exhaust it, so every non-Open
	// record at the limit is refused; drawn lives in memory, so a restart
	// resets it. Upgrade by crediting it from the retirement path when repack
	// lands.
	drawn int64
	// draws is the headroom_draws counter, records appended while the
	// journal was at its limit; guarded by mu.
	draws uint64
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
	}
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
	free := total - recordsOf(h.drawn)
	switch {
	case free <= h.reserve:
		// Exhausted for this class: only the open reserve is left, which
		// an open's own records still draw on, down to nothing.
		if c == Open && free > 0 {
			h.drawn += RecordCharge
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
	h.drawn += RecordCharge
	h.mu.Unlock()
	return Draw{Append: true, drew: true}, nil
}

// Drawn counts one admitted record once it has appended: the headroom_draws
// counter, raised for every draw that moved the ledger, so the two agree
// whichever way the footprint moved after the admission. A record that drew
// nothing — below the limit, a repack copy, an Offloaded never appended —
// counts nothing.
func (h *Headroom) Drawn(d Draw) {
	if !d.drew {
		return
	}
	h.mu.Lock()
	h.draws++
	h.mu.Unlock()
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
	h.drawn -= RecordCharge
	if h.drawn < 0 {
		h.drawn = 0
	}
	h.mu.Unlock()
}

// atLimit reports the one state in which the headroom is drawn: the journal
// at its limit, the footprint plus a segment cap past the maximum — the
// same predicate headroom_draws counts by. The footprint is a snapshot
// read outside any held mutex: the set's lock is of this same capacity
// domain, and no two locks of one domain are held at once.
func (h *Headroom) atLimit() bool {
	return h.cfg.MaxBytes > 0 && h.cfg.Footprint()+h.cfg.SegmentCap > h.cfg.MaxBytes
}

// Draws returns the headroom_draws counter: the records appended while the
// journal was at its limit.
func (h *Headroom) Draws() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draws
}

// BelowHalf reports whether the headroom's drawing side has fallen below
// half its size without the open reserve — the content-write check this
// table consults beside its own classes. It reads true only while the
// journal is at its limit: headroom drawn during a past episode does not
// throttle content writes once the journal left the limit.
//
// decision: the gate is the same at-limit predicate the draw itself runs
// under, so the figure BelowHalf reports moves only with admissions the
// limit gated. The footprint read runs before the section for the same
// domain reason as Admit's.
func (h *Headroom) BelowHalf() bool {
	if h.cfg.Records <= 0 || !h.atLimit() {
		return false
	}
	total := h.cfg.Records
	size := total - h.reserve
	h.mu.Lock()
	defer h.mu.Unlock()
	free := total - recordsOf(h.drawn)
	return free-h.reserve < size/2
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
