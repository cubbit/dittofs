package datapath

import (
	"context"
	"fmt"
	"math"

	"github.com/cubbit/dittofs/pkg/journal/internal/capacity"
	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
)

// WriteAt appends p at off of file id as a write record under s's tag, then
// publishes it to the placement index, and returns the version it carries.
// mtime is the modification time the record carries.
//
// A cancelled ctx is refused first and changes nothing; a handle whose close
// began is refused next, with core.ErrClosed, and a call that runs holds its
// handle's Close back until it returns. The file's tag is checked next — a
// file that belongs to another share is refused with core.ErrOtherShare,
// whatever the call's own shape, before the payload's length or the extent's
// are even looked at. A zero-length p then returns the zero version and does
// nothing: no version is drawn and nothing is appended or counted. A
// negative off, or an off whose end overflows, is refused with
// core.ErrInvalidExtent before anything is drawn or appended. The tag check
// claims nothing, so a shape refused after it leaves the file as it was.
//
// The write's record bytes are reserved against the journal's limits before
// its version is drawn or a byte written: the one admission, capacity.Take,
// refuses naming the limit that would not take them — the share's, the
// journal's, the index's, the version limit's, the headroom's or the spare
// pool's — with the shortfall, at once, with no I/O and holding nothing
// behind. Its grant covers the pieces the split it sized drew: they take
// their spares from the grant's hold, and the unused part returns when the
// write does. A reserved write whose segment the stream's other writers filled
// rolls anyway; while its hold has fallen below what the reservation's remaining
// bytes need, the roll takes a replacement spare back from the pool (RFC 1 §6).
// The stream cannot tell such a roll from one the write's own bytes forced, so a
// stray replacement may leave the hold one above that need until the
// reservation's next draw; the over-hold is bounded by what Reserve took, is
// returned at Release, and fails closed. An unreserved write has no reservation
// to replenish, and one piece more than its split can still roll — the room
// another appender spent between the take and its append, a window neither its
// charge nor its hold covers — and that one rolls unreserved; see the decision
// at the loop below for its ceiling and what closes it. A reserved write too can
// meet the no-spare refusal part-way: after a replacement, held exceeds ready
// while the maker refills, so its next roll may be refused `spare`, and a full
// device's ENOSPC refuses it the same way. RFC 1 §6's promise that it "never
// meets the no-spare refusal while the pool can be refilled" is stronger than a
// no-wait pool keeps; the ceiling is stated at Hold.Take.
//
// The version is drawn, and the record appended and published, with the
// file's append domain held, so the versions of one file are appended in the
// order they were assigned. The tag is checked again under that domain,
// ahead of the capacity admission and the draw, so checking a file's tag and
// claiming it are one step: two first writes of one new FileID through two
// handles cannot both pass the check. The domain is the only one held across
// the record write, and publishing follows the write of every piece: the index
// never names a record that is not yet in its segment. Publishing never waits
// on a sync.
//
// Any error returns the zero version. An unreserved write whose later piece
// fails to append publishes none of its pieces, so the index holds nothing of
// it; a reserved write keeps the pieces it already published. The error says
// the write as a whole did not complete, and the grant's unused bytes and
// spares return with it. A publish the index refuses part-way leaves an
// unreserved write held in part too, which the decision at that publish
// records.
func (c *Core) WriteAt(ctx context.Context, s *Share, id core.FileID, off int64, p []byte, mtime int64) (core.Version, error) {
	exit, err := beginCall(ctx, s)
	if err != nil {
		return core.Version{}, err
	}
	defer exit()
	// The check takes no share of the append domain: it claims nothing, so
	// the call stands down before the domain is taken. It refuses a call
	// naming a file another share owns before the call's own shape is looked
	// at — a zero-length write or an invalid extent names a file all the
	// same, and the wrong handle never reaches those refusals. The check
	// runs again under the domain, where the claim it precedes is made.
	if err := c.checkTag(s, id); err != nil {
		return core.Version{}, err
	}
	if len(p) == 0 {
		return core.Version{}, nil
	}
	if off < 0 || off > math.MaxInt64-int64(len(p)) {
		return core.Version{}, core.ErrInvalidExtent
	}
	v, n, err := c.appendAndPublish(ctx, s, id, off, p, mtime, nil)
	c.writtenBytes.Add(uint64(n))
	return v, err
}

// appendAndPublish does WriteAt's work under the file's append domain and
// returns the version the write carries and the payload bytes published. It is
// the one append loop both paths share: an admitted write passes a nil drawn
// grant and the capacity admission takes its grant above, while a write drawn
// from a reservation passes the grant Draw returned — the admission's every
// limit is already held by the reservation, so none runs here. The reserved
// write's version is drawn here too, from the reservation, under the file's
// append domain: the ordering the plain path keeps.
func (c *Core) appendAndPublish(ctx context.Context, s *Share, id core.FileID, off int64, p []byte, mtime int64, res *capacity.Reservation) (core.Version, int64, error) {
	unlock := c.domains.Lock(id)
	defer unlock()

	// The tag check runs here again, under the domain the claim publishes
	// under, so the two are one step: a first write through another handle
	// waits on the domain and meets the claim, not an unclaimed entry. A
	// refused write draws no version, appends nothing and counts no written
	// byte. The capacity admission below slots in right after this check,
	// before the draw; the check takes the file index shared and releases
	// it before the admission runs, so the admission names no lock of the
	// file's.
	if err := c.checkTag(s, id); err != nil {
		return core.Version{}, 0, err
	}
	stream := c.streams.For(id)
	// The write's grant: for a reserved write, one drawn from its reservation,
	// the draw taken here under the domain exactly as the plain path's Take is,
	// so the version it carries is drawn in the file's append order; for a
	// plain write, one the take draws from the room the stream names — against
	// the share's limit, the journal's maximum, the index's bound, the version
	// limit, the headroom's half rule and the spares its pieces need, in one
	// decision.
	var grant *capacity.Grant
	if res != nil {
		g, err := res.Draw(int64(len(p)))
		if err != nil {
			return core.Version{}, 0, err
		}
		grant = g
	} else {
		g, err := c.cap.Take(ctx, capacity.Request{
			Tag:        s.Tag,
			File:       id,
			Stream:     c.streams.IndexFor(id),
			Extent:     core.Extent{Off: off, Len: int64(len(p))},
			Payload:    int64(len(p)),
			ShareLimit: s.Limit(),
		})
		if err != nil {
			return core.Version{}, 0, err
		}
		grant = g
	}
	defer grant.Release()
	// The version the write carries: the one the admission drew in its own
	// section, where its check and draw were one step, or the one a
	// reservation drew for its write here — never a draw outside the domain,
	// which would let a plain writer of the same file append a newer version
	// first.
	v := grant.Version()
	var roll segment.Roll
	if hold := grant.Hold(); hold != nil {
		roll = segment.Roll{Hold: hold}
	}
	if res == nil {
		// An unreserved write may hold its pieces back, so the stream pins
		// the segment of each one inside its append.
		//
		// ponytail: every unreserved write, a one-piece write included, pays
		// one pin and one release, which locks the segment's state once; the
		// stream cannot tell before an append whether more pieces follow.
		// Upgrade to a lock-free release when a profile of small writes shows
		// it.
		roll.Pin = c.liveness
	}
	// A reserved write's roll is replaced only while the reservation's hold has
	// fallen behind what its still-undrawn bytes need. A hold at or above that
	// figure has what those bytes' own rolls will take, so a replacement there
	// would raise it past need. The stream cannot say whose roll forced the
	// move, so a copy of small writes alone on one stream can still have one of
	// its own straddles replaced, leaving the hold one above the need until the
	// next draw lowers it: bounded by what Reserve took, returned at Release,
	// and failing closed. Overturned by an exact own-vs-interleaved roll signal,
	// or by spare_refusals showing unreserved writes starved behind an open
	// reservation's over-hold. An admitted write's hold is not a reservation's
	// and closes with its grant: only a drawn grant replaces.
	if c.beforeAppend != nil {
		c.beforeAppend()
	}
	var done, published int64
	// ponytail: the first four pieces of an unreserved write wait in this
	// array, so the common write allocates nothing for them; a write of more
	// pieces grows the slice on the heap. Upgrade to a pooled slice when a
	// profile of writes spanning many segments shows it.
	var waiting [4]piece
	placed := waiting[:0]
	// Every piece pinned is released on the way out, a refused write's
	// included: its records are dead weight its segments may drop.
	defer func() {
		for _, pc := range placed {
			c.liveness.Unpin(pc.pl.Loc.Segment)
		}
	}()
	for done < int64(len(p)) {
		seq, err := c.counters.NextSeq()
		if err != nil {
			return core.Version{}, published, err
		}
		rest := p[done:]
		pl, err := stream.AppendRoll(format.RecordHeader{
			Kind:    format.KindWrite,
			File:    id,
			Extent:  core.Extent{Off: off + done, Len: int64(len(rest))},
			Seq:     seq,
			Version: v,
			Tag:     s.Tag,
		}, format.RecordBody{Payload: rest, MTime: mtime}, roll)
		if err != nil {
			// decision: an unreserved write that fails on a later piece publishes
			// nothing and returns no version: the pieces it already appended stay on
			// disk and counted as the share's, unnamed by the index, and the grant's
			// unused bytes return with it. A reopen's scan names them, as it does a
			// record it verifies, so the running journal and a restarted one
			// differ on a refused write until its segment retires. A reserved write
			// publishes each piece as it lands and keeps the ones a refusal leaves
			// behind, which RFC 1 §6 specifies for it, so its holder reserves again
			// for what they do not cover. The window a later piece can fail in is an
			// unreserved roll the grant no longer covers: the grant fits the write to
			// its stream's roomless state and holds only the spares the pieces its
			// split sized need, so a roll whose room something else spent — the idle
			// sealer sealing the active segment, a durability re-append or loss record
			// that never passed the admission, another same-stream writer whose own
			// spares-holding grant reserved the room this one claimed — draws a piece
			// beyond the split, and the hold serves it only while a spare of its own
			// is left: past that the roll runs unreserved and meets the pool's refusal
			// here. The direction fails closed: the write that drew the piece is
			// refused rather than admitted past a limit. A reserved write's
			// interleaved roll is replaced while its hold is short of what the
			// reservation still needs (the Replace below); its own ceiling is a spare
			// the pool cannot make, where a full device's ENOSPC still refuses it
			// here. The remaining cases are failures of the medium itself, which no
			// reservation can predict. Overturned by a stream that promises the room
			// between a take and an append, which no interleaving caller can, or by a
			// write contract that reports the extent a refused write held.
			return core.Version{}, published, err
		}
		// The stream pinned an unreserved piece's segment inside the append, so
		// the piece joins placed before anything can return and the deferred
		// release finds it.
		pc := piece{pl: pl, seq: seq, off: off + done}
		if res == nil {
			placed = append(placed, pc)
		}
		if pl.N <= 0 {
			// A stream that places nothing and reports no error would have
			// this loop append the same rest forever.
			return core.Version{}, published, fmt.Errorf("datapath: the stream placed no byte of a %d-byte piece of file %v at offset %d", len(rest), id, off+done)
		}
		// The record is in its segment and counted as the share's: its
		// own record size — the header, body and padding it occupies —
		// is what the reservation held for it, and this hands it over,
		// so ReservedBytes never covers a record the share already
		// counts and never misses one still landing.
		if c.beforeWrote != nil {
			c.beforeWrote()
		}
		grant.Wrote(pl.Loc.Len)
		// A reserved write's roll is the reservation's to replace while its
		// hold has fallen behind what its remaining bytes need: the roll took
		// a spare interleaving consumed, and the reservation takes one back
		// ahead of every unreserved write (RFC 1 §6). The replacement raises
		// the hold without refusing, so a pool that cannot serve it makes up
		// the demand instead. At or above that need the hold already has its
		// own rolls' spares, and a plain write's grant holds none of the
		// reservation's: it never replaces.
		if pl.Rolled && grant.Drawn() {
			if h := grant.Hold(); h != nil && h.Left() < grant.NeedsSpares() {
				h.Replace()
			}
		}
		if c.afterAppend != nil {
			c.afterAppend()
		}
		done += pl.N
		// An unreserved write holds every piece back until the last is in.
		if res == nil {
			continue
		}
		if err := c.publishPiece(s, id, v, grant, pc); err != nil {
			return core.Version{}, published, err
		}
		published += pl.N
	}
	// An unreserved write publishes its pieces only once every one of them is
	// in its segment: a roll refused part-way leaves the records it appended
	// on disk and counted, never named by the index, rather than a write held
	// in part under a version the caller never receives. A reserved write
	// publishes each piece as it lands, for a refusal on its roll keeps the
	// pieces already published and spends its version.
	for _, pc := range placed {
		if err := c.publishPiece(s, id, v, grant, pc); err != nil {
			return core.Version{}, published, err
		}
		published += pc.pl.N
	}
	return v, published, nil
}

// publishPiece names one appended piece in the index under the write's
// version and hands the entry it took to the grant. An error leaves the
// piece's record on disk, counted and unpublished.
func (c *Core) publishPiece(s *Share, id core.FileID, v core.Version, grant *capacity.Grant, pc piece) error {
	pl, seq := pc.pl, pc.seq
	// decision: the index may still refuse a publish after its record
	// is in the segment. The tag check above refuses a file another
	// share owns before a version is drawn, and the file's tag cannot
	// change while this call holds the file's append domain — only the
	// claim that publishes under this same domain writes a tag, and it
	// holds the domain from its check to its publish — so a tag refusal
	// here needs another tag to claim the file inside WriteRetain
	// itself, without the domain: no path does that today, and a path
	// that did would be the fault shape the rival mark exists for,
	// reached only by a bug. The entry bound, likewise, is checked
	// inside the admission's section above — Take's CheckWrite, its
	// held-entry test and its pending count — so a refusal here needs the
	// window between that check and this publish, which a concurrent
	// write's own admission can pass in: each admission names the entries
	// its pieces will add and holds them as pending until they publish.
	// The caller gets the error and the record stays on disk, never
	// published by this run. What is left here is the window no pre-check
	// closes, and a refusal in it is overturn evidence for a bound the
	// admission reserves as its own.
	hd, claimed, err := c.index.WriteRetainClaim(s.Tag, id, index.Placed{
		Extent:  core.Extent{Off: pc.off, Len: pl.N},
		Record:  index.RecordAt{Segment: pl.Loc.Segment, Offset: uint32(pl.Loc.Off)},
		Version: v,
		At:      c.clock.Now(),
	}, seq)
	if err != nil {
		return err
	}
	c.Account(hd)
	// The piece's index entry is the bound's now: the figure that held it
	// — a reservation's held entries, or a plain grant's pending ones —
	// hands over what the index admitted, so a write beside this one finds
	// it spent only once the index holds it — the safe order, for the
	// count and both held figures never all miss it.
	grant.Claimed(claimed)
	// decision: the index exports no write that skips retaining what it
	// supersedes, so a piece whose record a sync already covered lifts
	// here, straight after the publish, rather than leaving the content
	// retained until the durability worker's pass. The lift is per record
	// extent: it drops only what this piece's own record superseded, so a
	// durable piece never drops what a sibling piece of the same version,
	// not yet durable, still has to hold. The index's error from the lift
	// is unreachable today, because the function it runs returns nil; it
	// is ignored, and the worker lifts again after this write releases the
	// file's append domain and logs a failure. Its delta is accounted
	// before the error is looked at. Overturned by an index write that
	// takes a durable flag per piece.
	//
	// ponytail: Durable scans every stream's ledger forward under that
	// stream's mutex, once per published piece, and a ledger holds up to
	// one sync bound of writes, so the cost grows with concurrent small
	// writes. Upgrade to a scan from the end of each ledger, or one stream,
	// inside Durable when a profile of small concurrent writes shows it.
	if c.dur.Durable(pl.Loc) {
		lh, lifted, _ := c.index.LiftSuperseded(id, v, core.Extent{Off: pc.off, Len: pl.N})
		c.Account(lh)
		// Every segment this publish's own lift emptied is offered with
		// the causality record its retirement waits durable on — the
		// same shaped report the removal path makes — so the offer needs
		// no tick scan to find it.
		for _, seg := range lifted {
			if !c.liveness.Held(seg) {
				c.retiring.Emptied(seg, pl.Loc)
			}
		}
	}
	return nil
}

// piece is one appended record of a write waiting to be published: where the
// stream placed it, the sequence number it carries and the file offset it covers.
type piece struct {
	pl  segment.Placed
	seq core.Seq
	off int64
}
