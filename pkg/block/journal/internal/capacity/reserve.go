package capacity

import (
	"context"
	"fmt"
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
	// Stream is the append stream they will land on. A reservation's writes
	// land on their files' own streams, so Draw consults no stream and does no
	// per-stream room grant; the field is carried for a caller that names one.
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
// piece for its head and one for its tail beside the caps it crosses.
//
// ponytail: counting pieces as interleaved over-reserves footer and index
// entries by up to one per write, a few KiB for a clone's copies. Upgrade to
// giving a reservation's writes an append stream of their own, which makes its
// spares exact, if spare_refusals shows reserved writes among them.
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
		c: c, tag: r.Tag,
		bytes: r.Bytes, bytesAsk: r.Bytes + footer,
		entries: idx, versions: versions, hold: hold,
		leftBytes: r.Bytes, leftEntries: idx, leftVersions: versions,
	}, nil
}

// Reservation is what Reserve took: the record bytes and footer entries held
// against the share's limit and the journal's maximum, the index entries and
// versions held against their bounds, and the pool's hold on the spares the
// bytes roll into. It is not safe from two goroutines at once; the caller
// that reserved owns it.
type Reservation struct {
	c   *Capacity
	tag core.ShareTag

	// bytes is the record bytes the ask named, and bytesAsk is those bytes
	// with the footer entry of every piece: the journal-side figure the open
	// reservation charges until its Release, for its pieces land in spares it
	// already holds. entries and versions are the index entries and versions
	// it holds.
	bytes    int64
	bytesAsk int64
	entries  int64
	versions uint64
	// hold is the pool's hold on the spares the bytes roll into.
	hold *segment.Hold

	// leftBytes, leftEntries and leftVersions are what the reservation's
	// writes have not drawn yet: Release returns these and no more, Draw
	// refuses a write they cannot cover, and a drawn grant's unlanded bytes
	// come back to leftBytes.
	leftBytes    int64
	leftEntries  int64
	leftVersions uint64

	// released reports the first Release, making the later ones no-ops.
	released bool
}

// Tag returns the share the reservation's writes run as.
func (r *Reservation) Tag() core.ShareTag { return r.tag }

// Remaining returns the record bytes the reservation's writes have not drawn
// yet.
func (r *Reservation) Remaining() int64 {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.leftBytes
}

// Draw returns a Grant for one write of payload bytes, drawn from what the
// reservation holds: the record bytes the write's split will append, the
// index entries those pieces need, one of its versions, and the spares of its
// hold, which its pieces roll onto ahead of every unreserved write. The
// version is drawn here and returned by the grant, so the write that takes it
// appends under it. No limit of the journal and no headroom rule is consulted:
// the reservation took them all, and while it holds enough the write is
// refused by none.
//
// A reservation that no longer holds enough is refused with
// core.ErrReservationExhausted before any version is drawn and any counter
// moves: no limit refused the write — the reservation a caller took for it is
// spent — so the refusal is not a NoSpaceError. A released reservation refuses
// every draw with core.ErrClosed, and a write of no bytes is a caller bug,
// core.ErrInvalidExtent.
//
// decision: the record bytes are WriteCharge, the write's worst case over
// every room its stream can have, because a reservation's writes name their
// streams only as they land. The figure is never below what the write appends
// into any room, so the grant's Wrote never clamps it and the share's limit
// never undercounts what the write really spent. A caller sizes n with the
// same figure.
func (r *Reservation) Draw(payload int64) (*Grant, error) {
	if payload <= 0 {
		return nil, core.ErrInvalidExtent
	}

	r.c.mu.Lock()
	if r.released {
		r.c.mu.Unlock()
		return nil, core.ErrClosed
	}
	bytes := WriteCharge(payload, r.c.cfg.SegmentCap)
	// The pieces are the write's head plus one per cap its own bytes cross.
	// Their sum over the reservation's writes is at most 2 x writes plus one
	// per cap the whole ask can cross, the count Reserve took: a write's
	// share is its own bytes over the cap, rounded up and added to its head,
	// and the rounding's one per write rides inside the second piece each
	// write was counted. So a reservation's entries last its every write.
	pieces := 1
	if bytes > 0 {
		pieces += int((bytes + r.c.cfg.SegmentCap - 1) / r.c.cfg.SegmentCap)
	}
	switch {
	case bytes > r.leftBytes:
		missing := bytes - r.leftBytes
		r.c.mu.Unlock()
		return nil, fmt.Errorf("%w: %d record bytes missing of the reservation's %d", core.ErrReservationExhausted, missing, r.bytes)
	case int64(pieces) > r.leftEntries:
		missing := int64(pieces) - r.leftEntries
		r.c.mu.Unlock()
		return nil, fmt.Errorf("%w: %d index entries missing of the reservation's %d", core.ErrReservationExhausted, missing, r.entries)
	case r.leftVersions < 1:
		r.c.mu.Unlock()
		return nil, fmt.Errorf("%w: the reservation's %d versions are drawn", core.ErrReservationExhausted, r.versions)
	}
	// decision: the version is drawn under the section, so the check that the
	// reservation still holds one and the draw of it are one step. Drawn
	// later, a plain write passing Take between the two could take the version
	// the reservation holds, and the reserved write would meet the version
	// limit the reservation exists to keep it clear of.
	v, err := r.c.cfg.DrawVersion()
	if err != nil {
		r.c.mu.Unlock()
		return nil, err
	}
	r.leftBytes -= bytes
	r.leftEntries -= int64(pieces)
	r.leftVersions--
	// decision: the entries and the version stay in the reservation's held
	// figures until the write has claimed them — heldEntries until the publish
	// has reserved the piece, heldVersions until this draw, which is the
	// version's own claim. Releasing them here would open a window a plain
	// write's index check could pass, then eat them, and the reserved write
	// would meet the bound at its publish after drawing its version and
	// writing its record.
	r.c.heldVersions--
	r.c.mu.Unlock()

	// decision: the grant borrows the reservation's hold rather than closing
	// it when the write returns, because the spares it carries belong to the
	// reservation and serve its later writes: a write that rolls hands one
	// over, and the rest stay held until Release. It names no stream and holds
	// no room: a reservation's writes land on their files' own streams, and
	// its ask was charged to the journal's maximum rather than to a stream's
	// room.
	return &Grant{
		c: r.c, tag: r.tag, bytes: bytes, left: bytes,
		hold: r.hold, version: v, reservation: r, heldEntries: int64(pieces),
	}, nil
}

// WriteCharge is the record bytes one write of payload bytes can append into
// any room its stream has, and the figure a caller sizes a reservation's n by:
// the roomless split — one record per segment the write can fill — plus the one
// record a split that straddles a partly filled segment appends beyond it. A
// write wholly in room appends one record and one into a roomless stream the
// roomless split; between the two, its head in the room left and its tail in
// the next segment are two records where the roomless split names one for the
// whole.
func WriteCharge(payload, segCap int64) int64 {
	if payload <= 0 {
		return 0
	}
	// The extra is one record of no payload: its header, the write kind's
	// fixed body bytes and the padding to the record grid.
	return recordSplit(payload, 0, segCap) + segment.RecordBytes(0)
}

// Release returns what the reservation still holds: its bytes, footer entries,
// index entries, versions and spares. It is idempotent — a second Release does
// nothing.
//
// decision: the journal's maximum keeps the reservation's whole ask until
// this call, not only what its writes have not drawn: the room the ask names
// is in the spares the reservation holds, so no byte of it returns to the
// maximum before the hold does, and a plain write cannot claim it. It fails
// closed. The entries and versions a write drew stay spent — a write that
// failed part-way may have published its pieces — so a shortfall there
// refuses the reservation's next write rather than admitting it past a bound.
func (r *Reservation) Release() {
	// The whole release runs under the capacity mutex, so a write's draw and
	// the gives-back a drawn grant makes on its way out are ordered against
	// it: a grant released after this one gives its unlanded figures straight
	// to the counters, and one released before it gave them to leftBytes and
	// leftEntries, which this call returns whole. The pool's hold is closed
	// outside the section, for the pool's lock is of this same domain.
	if r.released {
		return
	}
	r.c.mu.Lock()
	if r.released {
		r.c.mu.Unlock()
		return
	}
	r.released = true
	bytes, entries, versions := r.leftBytes, r.leftEntries, r.leftVersions
	r.leftBytes, r.leftEntries, r.leftVersions = 0, 0, 0
	r.c.reserved -= bytes
	r.c.reservedTagged[r.tag] -= bytes
	r.c.reservedAsk -= r.bytesAsk
	r.c.heldEntries -= entries
	r.c.heldVersions -= versions
	hold := r.hold
	r.hold = nil
	r.c.mu.Unlock()
	if hold != nil {
		hold.Close()
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
