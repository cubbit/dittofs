package capacity

import (
	"context"
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
// decision: the funcs carry no lock of the capacity domain. The pool's and
// the set's reads are lock-free snapshots of counters the wiring owns; the
// drawn figure below is this package's own, so the check and the draw are
// one section that holds no other layer's lock.
type HeadroomConfig struct {
	// Records is the configured headroom, in records without bytes.
	Records int

	// Pool reports the headroom bytes allocated now, as the spare pool
	// holds them: the headroom spares created ahead of need.
	Pool func() (headroomBytes int64)

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
// decision: the headroom's size is the smaller of what the spare pool
// holds and what the settings asked for (Records x RecordCharge): the pool
// allocates whole segments, so its HeadroomBytes can sit above the
// configured figure by the rounding, and bytes over it are not a headroom
// the records can draw on; the missing spares of a pool still making them
// are not one either.
//
// ponytail: the check and the draw share one mutex, so every
// header-only record crosses it briefly and no I/O runs under it. Upgrade
// to per-class atomics with a compare-and-swap loop on the drawn figure
// once a profile of a header-only-heavy load shows the mutex.
type Headroom struct {
	cfg     HeadroomConfig
	reserve int // the open reserve, in records, fixed at construction

	mu    sync.Mutex
	drawn int64  // the record bytes the headroom has admitted; guarded by mu
	draws uint64 // the headroom_draws counter; guarded by mu
}

// NewHeadroom returns a Headroom over cfg. A nil injected func is a wiring
// bug and panics here.
func NewHeadroom(cfg HeadroomConfig) *Headroom {
	if cfg.Pool == nil || cfg.Footprint == nil || cfg.Admit == nil {
		panic("capacity: the headroom needs every injected fact")
	}
	return &Headroom{
		cfg:     cfg,
		reserve: OpenReserveRecords(cfg.Records),
	}
}

// Draw reports whether the caller appends the record it asked about. An
// admission answers Draw{Append: false} only when the record must not be
// appended — an Offloaded record below half, whose bits are set in memory
// only, so the extents become evictable at once and a crash loses them,
// which is the safe direction.
type Draw struct {
	Append bool
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
func (h *Headroom) Admit(ctx context.Context, c RecordClass) (Draw, error) {
	if err := h.cfg.Admit(); err != nil {
		return Draw{}, err
	}
	if c == RepackCopy {
		// The repack copy draws on the repack reserve, a reserve of its
		// own outside the headroom and every share's limit, so no
		// headroom figure can refuse it.
		return Draw{Append: true}, nil
	}

	pool := recordsOf(h.clampedPool())
	size := pool - h.reserve // the drawing side: the headroom without the open reserve
	half := size / 2

	h.mu.Lock()
	free := pool - recordsOf(h.drawn)
	switch {
	case free <= h.reserve:
		// Exhausted for this class: only the open reserve is left, which
		// an open's own records still draw on, down to nothing.
		if c == Open && free > 0 {
			h.drawn += RecordCharge
			h.mu.Unlock()
			return Draw{Append: true}, nil
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
			return Draw{Append: false}, nil
		}
	}
	h.drawn += RecordCharge
	h.mu.Unlock()
	return Draw{Append: true}, nil
}

// Drawn tells the headroom one admitted record of class c has been
// appended: it moves the headroom_draws counter when the journal is at its
// limit — the footprint plus a segment cap past the maximum — and nothing
// else, for the drawn figure moved already, in Admit's own section.
//
// decision: the charge moves at admission rather than at the landing —
// Admit and Drawn are separate calls, so only a draw inside Admit keeps
// the check honest under interleaving: an admission of the last free record
// must refuse the next one, whichever caller's append lands first, which a
// figure that moved only at Drawn could not do. An Offloaded record below
// half was never drawn, so it moves nothing here either.
func (h *Headroom) Drawn(c RecordClass) {
	// A repack copy draws on the repack reserve, not the headroom, so it
	// moves no counter here.
	if c == RepackCopy {
		return
	}
	// The footprint is a snapshot read outside the section: the set's own
	// read takes a lock of this same capacity domain, and no two locks of
	// one domain are held at once.
	atLimit := h.cfg.MaxBytes > 0 && h.cfg.Footprint()+h.cfg.SegmentCap > h.cfg.MaxBytes
	if !atLimit {
		return
	}
	h.mu.Lock()
	h.draws++
	h.mu.Unlock()
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
// table consults beside its own classes.
func (h *Headroom) BelowHalf() bool {
	pool := recordsOf(h.clampedPool())
	size := pool - h.reserve
	h.mu.Lock()
	defer h.mu.Unlock()
	free := pool - recordsOf(h.drawn)
	return free-h.reserve < size/2
}

// clampedPool returns the headroom bytes the records draw on: the smaller
// of what the spare pool holds and what the settings asked for, per the
// construction decision above.
func (h *Headroom) clampedPool() int64 {
	poolBytes := h.cfg.Pool()
	if configured := int64(h.cfg.Records) * RecordCharge; poolBytes > configured {
		poolBytes = configured
	}
	return poolBytes
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
