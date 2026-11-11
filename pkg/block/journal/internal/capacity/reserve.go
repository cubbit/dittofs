package capacity

import (
	"context"
	"math"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// ReserveRequest is one reservation's ask: the record bytes its writes will
// append, how many writes share them, and the limits that govern them.
//
// decision: ReserveRequest is plain data, as Request is: the caller builds
// it, hands it to Reserve once and never reads it again.
type ReserveRequest struct {
	// Tag is the share the reservation's writes run as.
	Tag core.ShareTag
	// Stream is the append stream they will land on.
	Stream int
	// Bytes is the record bytes the writes will append in all.
	Bytes int64
	// Writes is how many writes the bytes are split into.
	Writes int
	// ShareLimit is the share's record-byte limit as the call read it. Zero
	// or negative bounds nothing.
	ShareLimit int64
}

// PiecesFor returns the pieces writes writes of bytes record bytes in all can
// be split into, and the spares they need: two pieces per write plus one per
// cap the bytes can cross, and one spare per cap plus the first segment. A
// caller that names no bytes or no writes has nothing to reserve, and answers
// no piece and no spare.
//
// decision: the count treats every write as interleaved with the stream's
// other writes, so any one of them can straddle a cap whatever its size — one
// piece for its head and one for its tail beside the caps it crosses. The
// count over-reserves footer and index entries by up to one per write, a few
// KiB for a clone's copies. Upgrade to giving a reservation's writes an
// append stream of their own, which makes its spares exact, if spare_refusals
// shows reserved writes among them.
func PiecesFor(bytes int64, writes int, segCap int64) (pieces, spares int) {
	if bytes <= 0 || writes <= 0 || segCap <= 0 {
		return 0, 0
	}
	caps := bytes / segCap
	if bytes%segCap != 0 {
		caps++
	}
	// Saturate at the largest int: a count no limit could hold must refuse,
	// and a wrapped one would read as a fit.
	if writes > math.MaxInt/2 || caps > int64(math.MaxInt-2*writes) {
		return math.MaxInt, math.MaxInt
	}
	return 2*writes + int(caps), int(caps) + 1
}

// Reserve takes at once, and refuses as Take refuses, everything writes
// writes of Bytes record bytes in all will need: the bytes and their footer
// entries against the share's limit and the journal's maximum, an index entry
// per piece against the index's bound, one version per write against the
// version limit, and the spares the bytes roll into from the pool. A caller
// that holds a reservation is refused by none of those limits while it holds
// enough, so an operation that must not fail part-way — a clone's copies —
// finds every refusal before it changes anything.
//
// A refusal is a *core.NoSpaceError naming the limit that refused, in Take's
// order: the read-only sentinel, then the headroom threshold, the version
// limit, the index bound, the share's limit, the journal's maximum, and the
// spare pool last. A request that names no byte or no write names no
// operation that could exist, so it is refused as core.ErrInvalidExtent,
// consulting no limit and holding nothing.
//
// decision: the pieces the ask is sized by are interleaved (PiecesFor), and
// the journal's maximum is charged the ask's bytes and their footer entries
// beside the footprint: a reservation's room is in segments it has not
// allocated yet, so the footprint does not hold it, and two reservations that
// both read the same footprint must not both claim its room.
//
// decision: the held entries and versions ride beside the injected reads
// rather than inside an index or counter reservation, because both layers'
// own admissions are per call and a reservation is not: while a reservation
// holds them, a plain write sees what is left and is refused where it would
// have spent them.
//
// decision: Hold runs outside the critical section because the pool's lock is
// of this same capacity domain, and no two locks of one domain are held at
// once (RFC 1 §8.3): the reservation is visible while Hold runs, so a
// concurrent take may be refused by room a refused Hold then returns. That
// fails closed and is accepted, exactly as Take accepts it; the alternative
// would hold the section across the pool's lock.
func (c *Capacity) Reserve(ctx context.Context, r ReserveRequest) (*Reservation, error) {
	// The read-only check consults no limit, so it runs before the section
	// and reads nothing of the reservation state it refuses to move.
	if err := c.cfg.Admit(); err != nil {
		return nil, err
	}
	if r.Bytes <= 0 || r.Writes <= 0 {
		return nil, core.ErrInvalidExtent
	}

	pieces, spares := PiecesFor(r.Bytes, r.Writes, c.cfg.SegmentCap)
	// An ask whose pieces and footer entries no int64 names cannot fit any
	// journal maximum, which is one: refused before the section so the sums
	// below never wrap a fit.
	//
	// decision: this is the journal's refusal rather than the order's next
	// limit, because the bytes it names are the journal's alone to bound.
	if pieces > math.MaxInt/format.CatalogEntrySize {
		return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitJournal, Missing: math.MaxInt64, Unit: core.UnitBytes})
	}
	footer := int64(pieces) * format.CatalogEntrySize
	idx := int64(pieces)
	versions := uint64(r.Writes)

	// The headroom threshold, read as a snapshot: its own lock is of this
	// domain, so it is never taken under the section. A reservation is a
	// content ask, so the half rule applies to Reserve itself; only the
	// writes it serves are exempt once it holds.
	belowHalf := c.cfg.BelowHalf()
	if belowHalf {
		return nil, c.refuse(ctx, *refuseHeadroom(1))
	}
	// The journal's footprint, the index's entries left and the counter's
	// versions left are plain reads, but the set's is a lock of this same
	// domain: taken here, beside the section rather than under it, as Take
	// takes it.
	footSnap := c.cfg.Footprint()

	c.mu.Lock()
	c.seedFor(r.Tag)

	// Version limit: the versions open reservations hold are not this ask's
	// to draw, so it needs its own beside them.
	if left := c.cfg.VersionsLeft(); left < c.heldVersions+versions {
		missing := c.heldVersions + versions - left
		c.mu.Unlock()
		return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitVersion, Missing: int64(missing), Unit: core.UnitVersions})
	}
	// Index bound: the entries open reservations hold are not this ask's
	// either.
	if left := c.cfg.EntriesLeft(); left < c.heldEntries+idx {
		missing := c.heldEntries + idx - left
		c.mu.Unlock()
		return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitIndex, Missing: missing, Unit: core.UnitEntries})
	}
	// Share limit: the share's settled record bytes, the bytes its open
	// grants hold, and this ask's record bytes, must fit. A footer entry is
	// the journal's, not the share's (RFC 1 §6).
	if r.ShareLimit > 0 {
		if held := c.shareTally[r.Tag] + c.reservedTagged[r.Tag] + r.Bytes; held > r.ShareLimit {
			missing := held - r.ShareLimit
			c.mu.Unlock()
			return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitShare, Missing: missing, Unit: core.UnitBytes})
		}
	}
	// Journal maximum: the ask's bytes and footer entries beside what
	// reservations already hold and the footprint, for a reservation names
	// room in segments not yet allocated.
	if c.cfg.MaxBytes > 0 {
		if held := footSnap + c.reservedAsk + r.Bytes + footer; held > c.cfg.MaxBytes {
			missing := held - c.cfg.MaxBytes
			c.mu.Unlock()
			return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitJournal, Missing: missing, Unit: core.UnitBytes})
		}
	}

	// A turn of the hook, then the counters move: the reservation is visible
	// whole or not at all.
	if h := c.hook; h != nil {
		h()
	}
	c.heldEntries += idx
	c.heldVersions += versions
	c.reservedAsk += r.Bytes + footer
	c.reserved += r.Bytes
	c.reservedTagged[r.Tag] += r.Bytes
	c.mu.Unlock()

	// The spares, held after the section released. A refused hold gives the
	// reservation back whole; the pool's own refusal counts and spans like
	// every other, and an error that is not a limit's own — the filesystem's
	// ENOSPC among them — passes through as itself.
	hold, err := c.cfg.Hold(spares)
	if err != nil {
		c.releaseAsk(r.Tag, r.Bytes, r.Bytes+footer, idx, versions)
		if ns := asNoSpace(err); ns != nil {
			_ = c.refuse(ctx, *ns)
		}
		return nil, err
	}
	return &Reservation{
		c: c, tag: r.Tag, stream: r.Stream,
		bytes: r.Bytes, bytesAsk: r.Bytes + footer,
		entries: idx, versions: versions, hold: hold,
	}, nil
}

// Reservation is what Reserve took: the record bytes and footer entries held
// against the share's limit and the journal's maximum, the index entries and
// versions held against their bounds, and the pool's hold on the spares the
// bytes roll into. It is not safe from two goroutines at once; the caller
// that reserved owns it.
type Reservation struct {
	c      *Capacity
	tag    core.ShareTag
	stream int

	// bytes is the record bytes the ask named, and bytesAsk is those bytes
	// with the footer entry of every piece: the two figures the reservation
	// gives back when it releases.
	bytes    int64
	bytesAsk int64
	// entries and versions are the index entries and versions the
	// reservation holds.
	entries  int64
	versions uint64
	// hold is the pool's hold on the spares the bytes roll into.
	hold *segment.Hold

	// released reports the first Release, making the later ones no-ops.
	released bool
}

// Release returns what the reservation still holds: its bytes, footer entries,
// index entries, versions and spares. It is idempotent — a second Release
// does nothing.
func (r *Reservation) Release() {
	if r.released {
		return
	}
	r.released = true
	r.c.releaseAsk(r.tag, r.bytes, r.bytesAsk, r.entries, r.versions)
	if r.hold != nil {
		r.hold.Close()
		r.hold = nil
	}
}

// releaseAsk returns to the counters what a reservation — a failed Reserve or
// a released one — still holds. It runs under the mutex, and is the one place
// a reservation's ask is given back.
func (c *Capacity) releaseAsk(tag core.ShareTag, bytes, bytesAsk, entries int64, versions uint64) {
	c.mu.Lock()
	c.reserved -= bytes
	c.reservedTagged[tag] -= bytes
	c.reservedAsk -= bytesAsk
	c.heldEntries -= entries
	c.heldVersions -= versions
	c.mu.Unlock()
}
