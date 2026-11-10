// Package capacity holds the journal's admission: the one function every
// appending call asks once before it draws a version or writes a byte, the
// counters its reservations move, and the named refusals a limit answers
// with.
//
// decision: the package owns one lock domain — capacity, RFC 1 §8.2's — a
// single store-wide mutex over the reservation counters. Every injected fact
// is a plain func or value and every produced type is concrete: no interface
// sits between this package and the datapath that calls it, because both
// run in the same domain and an interface would only hide which side holds
// it. The segment package's hold the grant carries is the one type from
// outside, taken as the concrete pointer the pool returns.
package capacity

import (
	"context"
	"errors"
	"math"
	"sync"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// Config is the facts a Capacity is built over: the injected plain funcs of
// the layers it reads, and the constants of the journal it serves. Every
// func is required; a nil one panics at construction, which is a wiring bug
// and reads as one.
//
// decision: Config is plain data — no lock of its own and none taken while
// it is read — because every field is set once at construction and never
// after; the funcs it carries are the wiring's lock discipline to keep, and
// the ones that take a capacity-domain lock (the pool's ReadySpares, the
// set's Footprint and ShareFigures) are called as snapshots outside the
// decision section, never under it.
type Config struct {
	// Admit is the read-only check: a non-nil error is the sentinel that
	// journal-wide state carries, and a write is refused with it at once,
	// consulting no limit and reserving nothing.
	Admit func() error

	// CheckIndex predicts the index entries a write would add against the
	// index's bound without reserving; its refusal is returned untouched.
	CheckIndex func(file core.FileID, e core.Extent) error

	// VersionsLeft reports how many versions remain below the version
	// limit; zero means the next draw is refused.
	VersionsLeft func() uint64

	// Footprint reports the journal's accounted used bytes: every segment
	// and every spare at its cap.
	Footprint func() (used int64)

	// ShareFigures reads, under the reporting layer's one lock, the share
	// tag's settled record bytes beside its cumulative retired record
	// bytes — the monotone figure of what retirements and recycles have
	// released of the tag. It is read twice — to seed the tag's own tally,
	// and by Retired to count the retirements since — and never under the
	// capacity mutex, for its lock is of this same domain.
	ShareFigures func(tag core.ShareTag) (settled, retired int64)

	// Room reports the payload bytes the stream's active segment can still
	// take: the figure Take grants against, read inside its decision
	// section. Its lock ranks below capacity in the order of RFC 1 §8.3
	// (the segment-append domain), so the read under Take's own mutex is
	// allowed; it must make no I/O.
	Room func(stream int) int64

	// ReadySpares reports the content spares an unreserved take can reach:
	// the ready ones less those owed to open holds.
	ReadySpares func() int

	// Hold takes n content spares from the pool, held for the grant and
	// returned to it when the write no longer needs them.
	Hold func(n int) (*segment.Hold, error)

	// Tracer opens the spans the refusals are recorded in; nil traces
	// nothing.
	Tracer core.Tracer

	// MaxBytes is the journal's configured maximum, in bytes. Zero or
	// negative bounds nothing.
	MaxBytes int64

	// SegmentCap is the size a segment is accounted at, in bytes.
	SegmentCap int64
}

// Request is one write's ask: the record bytes it will append and the
// limits that govern it.
//
// decision: Request is plain data — no lock of its own — because the
// caller builds it, hands it to Take once and never reads it again. The
// figures it carries are plain facts of the write; the room its stream
// holds is not one of them — it is read inside the section, so it moves
// under the same order every other grant does.
type Request struct {
	// Tag is the share the write runs as.
	Tag core.ShareTag
	// File is the file the write names.
	File core.FileID
	// Stream is the write's append stream, for the per-stream room grant.
	Stream int
	// Extent is the bytes of the file the write covers.
	Extent core.Extent
	// Payload is the bytes of the file the write carries. The section
	// draws the split its own room read names and sums the pieces' record
	// bytes — headers, body bytes and padding — into the ask it admits:
	// the caller names the bytes it writes, and the admission owns the
	// figure of what they will cost.
	Payload int64
	// ShareLimit is the share's record-byte limit as the call read it.
	// Zero or negative bounds nothing.
	ShareLimit int64
}

// Capacity is the journal's admission function and its reservation
// counters. It is safe from any goroutine.
//
// decision: every counter the admission reads and moves sits under one
// mutex — the capacity domain of RFC 1 §8.2 — so each take's read and
// decision are one critical section and no reservation can pass between
// two takes' reads (a design that reads a counter and then decides is not
// conformant, RFC 1 §6). The hold itself runs after the section releases:
// the pool's lock is of the same domain, and no two locks of it are held
// at once (RFC 1 §8.3).
//
// ponytail: every write on the journal crosses this one mutex for a few
// counter updates. Upgrade to per-share atomics with a compare-and-swap
// loop on the journal-wide counter once a 64-file benchmark shows the
// mutex in a profile.
type Capacity struct {
	cfg Config

	// mu is the capacity domain: it guards every field below.
	mu sync.Mutex

	// reserved is the record bytes open grants hold and their records have
	// not yet filled — the ReservedBytes gauge (RFC 1 §3.7).
	reserved int64

	// reservedTagged is reserved broken down by share tag, so a refusal
	// can say what this write's share still holds in flight.
	reservedTagged map[core.ShareTag]int64

	// shareTally is the share's record-byte figure this package owns,
	// seeded once per tag from the set's settled bytes and moved only
	// under mu: raised by Wrote as records land, lowered by Retired as
	// the retirement of a segment frees the tag's record bytes.
	shareTally map[core.ShareTag]int64

	// seeded names the tags whose figure has been seeded from the set
	// already, so the seed happens once per tag, before its first take's
	// section and never inside it.
	seeded map[core.ShareTag]bool

	// retiredSeen is the tag's cumulative retired record bytes that the
	// tally's arithmetic has already taken into account — the figure of a
	// Retired call or the seed's own pair read, whichever names more — so
	// every later call subtracts only its positive delta: the retirements
	// whose bytes the tally still holds. The max, never the last value,
	// keeps a call that races the seed with an older read from rolling
	// the baseline back.
	retiredSeen map[core.ShareTag]int64

	// grantedRoom is the room bytes granted per stream and not yet
	// appended: the room a later take sees is what remains after these.
	grantedRoom map[int]int64

	// refused is the reservation_refused counter: every refusal Take
	// returns, its own and the injected checks', counted exactly once
	// each.
	refused uint64

	// pendingSeats is the whole segments open admissions have reserved
	// against the journal's maximum whose spares no hold has served yet.
	// A take whose coverage falls short of its need reserves the
	// difference here inside the decision section, so a second take in the
	// window before the first's hold answers cannot claim the same room
	// (RFC 1 §6: the journal must not accept bytes that exceed the
	// maximum, however its callers interleave). The hold's answer settles
	// it: a served hold's spares are real segments in the footprint, and
	// a refused one returns its reservation whole — both clear the seats
	// the take reserved. It can never go negative: it moves only under mu,
	// and each take's clearing runs exactly once per admission.
	pendingSeats int

	// hook is a test seam that runs inside the decision section, after the
	// counters' read and before the reservation moves. It is nil outside
	// tests.
	//
	// decision: it is a field of the Capacity and not of Config so that no
	// production wiring can set it.
	hook func()

	// wroteHook is a test seam that runs inside Wrote's section with the
	// reserved figure after the move. Nil outside tests.
	wroteHook func(stream int, n, reserved int64)
}

// New returns a Capacity over cfg. A nil injected func is a wiring bug and
// panics here.
func New(cfg Config) *Capacity {
	if cfg.Admit == nil || cfg.CheckIndex == nil || cfg.VersionsLeft == nil ||
		cfg.Footprint == nil || cfg.ShareFigures == nil || cfg.Room == nil ||
		cfg.ReadySpares == nil || cfg.Hold == nil {
		panic("capacity: the admission needs every injected fact")
	}
	if cfg.SegmentCap <= 0 {
		panic("capacity: a segment cap of nothing divides the spares by zero")
	}
	return &Capacity{
		cfg:            cfg,
		reservedTagged: make(map[core.ShareTag]int64),
		shareTally:     make(map[core.ShareTag]int64),
		seeded:         make(map[core.ShareTag]bool),
		retiredSeen:    make(map[core.ShareTag]int64),
		grantedRoom:    make(map[int]int64),
	}
}

// Grant is one admitted write's reservation: the record bytes held against
// the share's limit and the journal's maximum, the room granted on its
// stream, and the content spares held for the bytes beyond that room. It is
// not safe from two goroutines at once; the append that asked for it owns
// it, and no lock of the capacity domain is held between its calls.
//
// decision: a Grant carries no lock of its own. Every field it holds is
// moved under its Capacity's mutex, whose domain the grant's stream shares,
// so the grant's own methods take that mutex and hold no other.
type Grant struct {
	c      *Capacity
	tag    core.ShareTag
	stream int

	// bytes is the record bytes the grant reserved, the real split plus
	// the interleaving slack.
	bytes int64
	// left is what Release still has to return: it falls by each Wrote.
	left int64
	// roomPart is the room bytes of the grant covered by the stream's
	// active segment, returned to the stream only by Release when the
	// write never appended them; Wrote consumes them as the records land.
	roomPart int64
	// spares is the content spares the bytes beyond the room need, held
	// for the grant; zero when the write fit the room.
	spares int
	// hold is the pool's hold for the spares, nil when the write fit the
	// room.
	hold *segment.Hold

	// released reports the first Release, making the later ones no-ops.
	released bool
}

// Take is the one admission function: it reserves r's record bytes against
// the share's limit, the journal's maximum and the stream's room in one
// decision section, and holds — outside that section — the content spares
// the bytes beyond the room need, so no appending call ever draws a
// version, writes a byte or publishes in part what a limit would have
// refused. It runs with no file-index lock held.
//
// A refusal is a *core.NoSpaceError naming the limit that refused, in this
// order: the read-only sentinel, then the headroom threshold's slot (left
// open until that check exists — it runs first among the limits because
// only the engine's own reclaim clears it), the version limit, the index
// bound, the share's limit, the journal's maximum, and the spare pool
// last — naming its own missing amount and unit. A refusal performs no
// I/O, never waits and never evicts; each is counted by RefusedCount and
// opened as one reserve_refused span. An error the filesystem itself
// returned (ENOSPC from a spare the pool reached for) is passed through
// as itself and counts nothing.
//
// decision: Hold runs outside the critical section because the pool's lock
// is of the same capacity domain and no two locks of one domain are held
// at once (RFC 1 §8.3): the reservation is visible while Hold runs, so a
// concurrent Take may be refused by bytes a refused Hold then returns —
// the share's limit or the maximum another write briefly holds. That
// fails closed, and is accepted, because the alternative holds the
// section across the pool's lock.
//
// decision: under the section's own lock only two other layers are read,
// both allowed by the rank order of RFC 1 §8.3 (file append → capacity →
// file index): the injected CheckIndex takes the file index's lock, which
// sits below capacity in that order, and VersionsLeft is a wiring-supplied
// counter read that takes no lock of this domain — the Config says each
// injected fact must be a plain read, and the wiring that supplies a
// locking one moves it to a snapshot above, as the pool's, the set's and
// the share's are. Nothing else is read under mu: the pool's ReadySpares,
// the set's Footprint and ShareFigures are snapshotted before the section,
// Hold runs after it, and the reservation counters are this package's
// own.
//
// decision: a later reservation taken ahead over several writes — the
// ahead-call the interface offers — is the same holding this one is: one
// grant whose pieces draw on it, released whole. No separate shape is
// needed for it here.
func (c *Capacity) Take(ctx context.Context, r Request) (*Grant, error) {
	// The read-only check consults no limit, so it needs no counter and
	// runs before the section: a read-only journal answers at once,
	// reading nothing of the reservation state it refuses to move.
	if err := c.cfg.Admit(); err != nil {
		return nil, err
	}
	// A request of negative payload names no write that could exist, so it
	// is a caller bug rather than a limit's refusal: refused here,
	// consulting no limit, counted by none.
	if r.Payload < 0 {
		return nil, core.ErrInvalidExtent
	}

	// The capacity-domain facts of other layers are read before the
	// section, as snapshots: the pool's ReadySpares and the set's Footprint
	// both take locks of this same capacity domain, and no two locks of one
	// domain are held at once (RFC 1 §8.3) — the section would nest c.mu
	// over them. A snapshot is safe to decide on because the reservation of
	// uncovered segments below and the hold's own all-or-nothing refusal
	// are what keep it honest: a snapshot that overstates the ready spares
	// or understates the footprint admits an uncovered need whose hold the
	// pool then refuses whole, and both directions fail closed. The
	// deadlocked alternative — holding c.mu across the pool's or the
	// set's own lock — is what the ordering forbids.
	readySnap := c.cfg.ReadySpares()
	footSnap := c.cfg.Footprint()
	// The share's settled figure is seeded once per tag, read from the set
	// before the section — the set's lock is of this same domain, so the
	// seed never runs under mu — and every move after the seed is this
	// package's own, under mu: Wrote raises it, Retired lowers it, the
	// admission adds its ask. A landing concurrent with the seed is whole
	// the moment its own Wrote runs. The read is a pair — the tag's
	// settled bytes beside its cumulative retired bytes, under the set's
	// one lock — so every retirement the seed takes into account it takes
	// exactly once: a retirement whose set-decrement landed before the
	// read is already out of the settled figure and inside the retired
	// one, and one whose call beat this commit shows in retiredSeen above
	// the pair's own figure and is subtracted from the settled figure
	// read, so whichever side of the read a retirement falls on, the
	// seeded tally carries it once.
	//
	// decision: the share figure is owned here rather than re-read from the
	// set per take, because a record lands in two steps — the append that
	// counts it in the set and the Wrote that hands it over here — and no
	// pair of reads, one of each layer, straddles that pair exactly: a take
	// between the steps saw the record in neither and admitted past the
	// limit, and a take after both steps but on a stale first read counted
	// it twice. Owned outright, the figure moves only under mu and the
	// boundary is exact between reservations. What the ownership gives up
	// is any record a path other than WriteAt appends under the tag: it
	// lands in the set and never in the tally, and no such appender exists
	// today — records carrying no share tag are the journal's (RFC 1 §6)
	// and a repack copy is accounted to its record's share only once its
	// source segment is unlinked (RFC 1 §7.2) — so the first such appender
	// owes the capacity the same reconciliation the retirement's Retired
	// call makes, with the bytes it appended. Overturned by an appender
	// under the tag that does not pass through this admission.
	c.mu.Lock()
	if !c.seeded[r.Tag] {
		c.mu.Unlock()
		settled, retired := c.cfg.ShareFigures(r.Tag)
		c.mu.Lock()
		if !c.seeded[r.Tag] {
			c.seeded[r.Tag] = true
			// The retirements a racing Retired call already reported —
			// whose set-decrements landed after this read, so the settled
			// figure still holds their bytes — are the part of
			// retiredSeen that the pair's own retired figure has not
			// caught up with: subtracting them here accounts each once,
			// and the baseline moves to the larger of the two figures
			// so every later call's delta starts from what this seed
			// already took in.
			if over := c.retiredSeen[r.Tag] - retired; over > 0 {
				settled -= over
			}
			c.shareTally[r.Tag] = settled
			if c.retiredSeen[r.Tag] < retired {
				c.retiredSeen[r.Tag] = retired
			}
		}
	}

	// Version limit: a write with no version left to draw is refused
	// naming it before any byte is reserved.
	if c.cfg.VersionsLeft() == 0 {
		c.mu.Unlock()
		return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitVersion, Missing: 1, Unit: core.UnitVersions})
	}

	// Index bound: the pre-check's refusal is forwarded as the check
	// returned it, and counts as Take's refusal once. The index counts it
	// in its own counter too — that is its count of the refusals its bound
	// caused, and this one is Take's of the reservation it refused; neither
	// counts the other's, so the one reservation is never counted twice
	// here.
	if err := c.cfg.CheckIndex(r.File, r.Extent); err != nil {
		c.mu.Unlock()
		if ns := asNoSpace(err); ns != nil {
			return nil, c.refuse(ctx, *ns)
		}
		return nil, err
	}

	// The room grant and the spares. The room is read inside the section,
	// through the injected Room func: its read takes the stream's append
	// mutex — the segment-append domain, ranked below capacity in the
	// order of RFC 1 §8.3 — makes no I/O, and always returns a figure the
	// stream's appends still stand under, so the section's grant never
	// trusts a room a later landing has already spent.
	//
	// decision: the room is read inside the section rather than snapshotted
	// by the caller, because a concurrent write on the same stream can
	// append — and hand its Wrote in — between a caller's snapshot and the
	// take, lowering the room the snapshot still trusted, so the take would
	// grant one window to two writers whose combined pieces then meet a
	// refused roll after their versions are drawn. Inside the section the
	// read is ordered against every other take's grant and every landing's
	// Wrote, and the section's grants below are what keep two takes on one
	// stream from claiming one window.
	roomNow := c.cfg.Room(r.Stream)
	// The record bytes the write will append: the payload's split drawn
	// from the room the section just read — the first record at most the
	// room, every later one at most an empty segment's — each piece
	// carrying its own header, fixed body bytes and padding.
	//
	// decision: the sum carries one extra record's own overhead beyond
	// the split the room names. A writer that lands on the same stream
	// after this take can spend the room between the take and this
	// write's appends — the append mutex ranks below capacity, so the
	// stream cannot hold the room against it — and the appends then draw
	// one piece more than the split named: only the first cut moves, for
	// every later cut falls at an empty segment's edge wherever the first
	// one took, so at most one extra record heads the sum. The extra's
	// own overhead is charged to the limits here — its payload is charged
	// already, by the split — and Release returns it when the write never
	// drew the piece. A share limit sitting exactly at the split's bytes
	// can then refuse a write that lands without drawing the piece: the
	// room can only be spent by a write the admission admitted, never by
	// one outside it, so the refusal falls on a write whose own
	// interleaving cost it the slack, and never admits a byte past the
	// limit.
	//
	// decision: the bound this charge gives the share's limit rests on
	// the premise that a write never draws more than one piece beyond
	// the split its take sized — that one extra record's overhead is
	// then the whole of what its reservation did not name. What holds the
	// premise is the stream's own split: every cut after the first falls
	// only at an empty segment's edge, a boundary no interleaving can
	// move, so a room spent under this write's first cut can shift that
	// cut alone and add at most one record to the sum. It would be
	// overturned by a stream that cuts a payload anywhere else — a
	// seal-driven split mid-segment, a fill that must break around an
	// interleaving append — for then a moved first cut could re-cut the
	// pieces after it and the landed bytes could exceed the charge by
	// more than one record's overhead.
	bytes := recordSplit(r.Payload, roomNow, c.cfg.SegmentCap)
	if slack := slackOverhead(); bytes > math.MaxInt64-slack {
		bytes = math.MaxInt64
	} else {
		bytes += slack
	}
	roomLeft := roomNow - c.grantedRoom[r.Stream]
	var (
		roomPart int64 // the room bytes this grant covers
		sparesN  int   // the spares the bytes beyond the room need
	)
	switch {
	case bytes <= roomLeft:
		roomPart = bytes
	case roomLeft > 0:
		roomPart = roomLeft
		sparesN = sparesFor(bytes-roomLeft, c.cfg.SegmentCap)
	default:
		sparesN = sparesFor(bytes-roomLeft, c.cfg.SegmentCap)
	}

	// Share limit: the share's settled record bytes — this package's own
	// tally — plus the record bytes its open grants hold, plus this
	// write's, must fit the limit. Every figure moves only under mu, so
	// the boundary is exact between reservations: two writers at the last
	// byte resolve in the order their sections take the mutex, never both
	// granted and never both refused.
	//
	// decision: the ask the share's limit sees carries the full charge
	// the admission draws — the split the room names and the one extra
	// record's charge with it — so whatever pieces the stream actually
	// draws, the settled record bytes can never overrun a limit the
	// admission passed: a write's real landings are covered by the charge
	// its reservation held, and the undrawn part of the charge returns at
	// Release, exactly once, never driving a counter negative. The cost
	// is a neighbour's slack — a live reservation holds against the limit
	// bytes the stream may never append — which can fail-closed refuse a
	// boundary write that would have fitted on the settled figure alone;
	// that direction fails rather than exceeds, which the RFC allows.
	// Overturned by a stream that can promise the room it measured, which
	// no interleaving caller can.
	if r.ShareLimit > 0 {
		if held := c.shareTally[r.Tag] + c.reservedTagged[r.Tag] + bytes; held > r.ShareLimit {
			missing := held - r.ShareLimit
			c.mu.Unlock()
			return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitShare, Missing: missing, Unit: core.UnitBytes})
		}
	}
	// Journal maximum: the whole segments the bytes beyond the room need
	// that no ready spare covers, counted at their cap against the
	// maximum. A ready spare is already in the footprint, so it covers a
	// need without moving UsedBytes; the seats other admitted takes
	// reserve and no hold has served yet cover none of the snapshot, and
	// this take's own shortfall is the part it reserves: creating a
	// segment reserves its cap against the maximum (RFC 1 §6), and the
	// reservation is honoured from the section that admits it, never from
	// the pool's answer alone.
	var seats int
	if sparesN > 0 && c.cfg.MaxBytes > 0 {
		cover := readySnap - c.pendingSeats
		if cover < 0 {
			cover = 0
		}
		if uncovered := sparesN - readySpares(cover, sparesN); uncovered > 0 {
			seats = uncovered
			// The reserved need is k whole segments at the cap, the
			// maximum's remaining room is headroom bytes, and the
			// refusal names the shortfall of the one against the other.
			// Both are taken in the overflow-safe form: the need's bytes
			// divide first, so a need whose own bytes would wrap the sum
			// still refuses with the largest missing an int64 can name
			// rather than a wrapped negative.
			k := int64(c.pendingSeats + uncovered)
			headroom := c.cfg.MaxBytes - footSnap
			if k > headroom/c.cfg.SegmentCap {
				missing := k*c.cfg.SegmentCap - headroom
				if missing < 0 {
					missing = int64(math.MaxInt64)
				}
				c.mu.Unlock()
				return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitJournal, Missing: missing, Unit: core.UnitBytes})
			}
		}
	}

	// The reservation moves, all after the last refusal point in the
	// section: the room, the bytes, the tag, the seats. The hook is the
	// test seam between the counters' read and this move — the parking
	// spot that shows a second caller cannot pass a read the first caller
	// has not yet reserved under.
	if h := c.hook; h != nil {
		h()
	}
	c.grantedRoom[r.Stream] += roomPart
	c.reserved += bytes
	c.reservedTagged[r.Tag] += bytes
	c.pendingSeats += seats
	c.mu.Unlock()

	// The spares the bytes need, held after the section released. A granted
	// hold carries real spares in the grant: the footprint already counts
	// them, so the seats this take reserved are the hold's now — cleared
	// here, whether the hold answers by serving or by refusing, which both
	// settle the reservation. A refused hold gives the reservation back
	// first, and its answer is the fail-at-once refusal the caller needs:
	// the pool's own spare refusal counts and spans like every other — it
	// is Take's refusal too, by the pool — while an error that is not a
	// limit's own, the filesystem's ENOSPC among them, passes through as
	// itself and neither counts nor spans.
	if sparesN > 0 {
		hold, err := c.cfg.Hold(sparesN)
		c.clearSeats(seats)
		if err != nil {
			c.releaseReservation(r.Tag, r.Stream, roomPart, bytes)
			if ns := asNoSpace(err); ns != nil {
				// Counted and spanned for the caller; the error it returns
				// is this same refusal, which the line below answers with.
				_ = c.refuse(ctx, *ns)
			}
			return nil, err
		}
		return &Grant{
			c: c, tag: r.Tag, stream: r.Stream, bytes: bytes, left: bytes,
			roomPart: roomPart, spares: sparesN, hold: hold,
		}, nil
	}

	return &Grant{
		c: c, tag: r.Tag, stream: r.Stream, bytes: bytes, left: bytes,
		roomPart: roomPart,
	}, nil
}

// Retired lowers the share's figure by the retirements the tag's
// cumulative retired bytes say have run since the figure last took them
// into account. The datapath's retirement executor calls it once per
// retirement, after the set's own counters have moved, so a retry after a
// retirement finds the room it freed. The call reads the tag's figures —
// the pair the seed reads — outside the capacity mutex, and applies under
// it only the positive delta over the baseline the seed or an earlier call
// already holds: a retirement whose set-decrement the baseline predates is
// subtracted here, one the baseline already includes is not subtracted
// again, whichever side of the seed or of an earlier call its decrement
// landed on.
//
// decision: the order in which the set's own retirement steps land against
// the seed of the tag's figure is exactly the pair this function and the
// seed read — and the retired figure is read beside the settled one under
// the reporting layer's one lock, so the two answers to "is this
// retirement in the figure yet?" can never disagree. Before the pair,
// a retirement whose set-decrement preceded the seed's read was already
// out of the settled figure, and the call's late arrival subtracted its
// bytes a second time — the tally undercounted by the retired bytes, the
// share's limit failed open, admitting writes past it by up to one
// retirement per tag race. The pairing closes the window in both
// directions: a call whose decrement the baseline already saw computes a
// delta of zero, and the seed seeds against a figure already free of the
// retirement, so the only error the tally can still carry is the transient
// AddRecord-to-Wrote lag — a record counted in the settled figure whose
// grant has not handed its bytes over yet — which the grant's own
// reserved bytes cover in the admission's figure and which fails closed,
// never admitting a byte past a limit. Overturned by a reporting layer
// that names retired bytes non-monotonically, whose arithmetic this
// baseline could then roll back.
func (c *Capacity) Retired(tag core.ShareTag) {
	_, retired := c.cfg.ShareFigures(tag)
	c.mu.Lock()
	if c.seeded[tag] {
		if delta := retired - c.retiredSeen[tag]; delta > 0 {
			c.shareTally[tag] -= delta
			if c.shareTally[tag] < 0 {
				c.shareTally[tag] = 0
			}
		}
	}
	if c.retiredSeen[tag] < retired {
		c.retiredSeen[tag] = retired
	}
	c.mu.Unlock()
}

// Wrote moves n record bytes from reserved to used, after a piece's record
// is written and counted in its share's bytes: the record is the share's
// once written, so the tally takes its bytes here — the move that makes the
// share's figure whole for a record landing beside a concurrent take — and
// the reservation gives them up, which keeps ReservedBytes never covering
// a record the share already counts. n past what the grant still holds is
// clamped, so a caller that reports more than it asked for cannot drive the
// gauge negative.
func (g *Grant) Wrote(n int64) {
	if g.released || n <= 0 {
		return
	}
	if n > g.left {
		n = g.left
	}
	// Wrote hands the landed record bytes to the share's figure and
	// takes them from the reservation: the tally owns them now, and the
	// share's ledger holds only what the pieces have not landed yet —
	// the undrawn slack among it, which Release returns whole.
	g.c.mu.Lock()
	g.left -= n
	g.c.reserved -= n
	g.c.reservedTagged[g.tag] -= n
	g.c.shareTally[g.tag] += n
	// The room the written bytes consumed leaves the stream's grant: what
	// Release returns of it is only what the write never appended.
	if g.roomPart > 0 {
		eat := min64(n, g.roomPart)
		g.c.grantedRoom[g.stream] -= eat
		g.roomPart -= eat
	}
	if h := g.c.wroteHook; h != nil {
		h(g.stream, n, g.c.reserved)
	}
	g.c.mu.Unlock()
}

// Release returns what the grant still holds: the record bytes its pieces
// never wrote, the room its stream never appended and the spares the write
// never took. The share's ledger of the grant's real bytes returns with
// them. It is idempotent — a second Release does nothing.
func (g *Grant) Release() {
	if g.released {
		return
	}
	g.released = true
	g.c.releaseReservation(g.tag, g.stream, g.roomPart, g.left)
	if g.hold != nil {
		g.hold.Close()
		g.hold = nil
	}
}

// Hold returns the pool's hold the grant carries, nil when the write fit
// its stream's room.
func (g *Grant) Hold() *segment.Hold { return g.hold }

// releaseReservation returns to the counters what a grant — a failed take,
// or a released one — still holds: bytes of the share's reservation, and
// room of the stream's grant. The caller has already decided the figures;
// this runs under the mutex, and is the one place a reservation is given
// back.
func (c *Capacity) releaseReservation(tag core.ShareTag, stream int, room, bytes int64) {
	if room < 0 {
		room = 0
	}
	if bytes < 0 {
		bytes = 0
	}
	c.mu.Lock()
	c.grantedRoom[stream] -= room
	c.reserved -= bytes
	c.reservedTagged[tag] -= bytes
	c.mu.Unlock()
}

// clearSeats returns the segments an admitted take reserved against the
// journal's maximum once its hold has settled, served or refused: a served
// hold's spares are real segments the footprint already counts, and a
// refused one's seats go back with the rest of its reservation. The seats
// were added under the same mutex, exactly once per admission, so they
// clear the same way and never below zero.
func (c *Capacity) clearSeats(seats int) {
	if seats <= 0 {
		return
	}
	c.mu.Lock()
	c.pendingSeats -= seats
	if c.pendingSeats < 0 {
		c.pendingSeats = 0
	}
	c.mu.Unlock()
}

// ReservedBytes returns what open reservations hold and their records have
// not yet filled — the gauge a caller reads to see the writes in flight.
func (c *Capacity) ReservedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserved
}

// RefusedCount returns the reservation_refused counter: how many
// reservations Take refused, by every limit it names, since the Capacity
// was built.
func (c *Capacity) RefusedCount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refused
}

// refuse counts one refusal — the reservation_refused counter and one
// reserve_refused span, ended with the refusal's own error — and returns
// that error for the caller to answer with. Called with mu released: the
// count and the span are of the reservation the caller refused, not of
// any state another take is moving.
func (c *Capacity) refuse(ctx context.Context, ns core.NoSpaceError) *core.NoSpaceError {
	c.mu.Lock()
	c.refused++
	c.mu.Unlock()
	e := &ns
	if c.cfg.Tracer != nil {
		_, end := c.cfg.Tracer(ctx, core.SpanReserveRefused)
		end(e)
	}
	return e
}

// recordSplit sums the record bytes a write of payload bytes against a
// stream of cap-byte segments holding room payload bytes of its active
// segment will append: the first record takes at most the room, every
// later one at most what an empty segment holds, each carrying its own
// header, the write kind's fixed body bytes and padding — the same closed
// forms the stream's own fit and Room draw, so the admission and the
// stream can never disagree about the figure.
func recordSplit(payload, room, cap int64) int64 {
	if payload <= 0 {
		return 0
	}
	// A payload itself near the top of the range needs no sum: the pieces
	// it rolls into cost more bytes than an int64 can name, so the figure
	// is the largest one and the take's own overflow-safe refusals answer
	// with it. Inside the range below, every product the split forms has
	// headroom of half the range.
	if payload > math.MaxInt64/2 {
		return math.MaxInt64
	}
	rest := payload
	if room >= payload {
		// The whole write sits in the active segment: one record.
		return recordBytes(payload)
	}
	empty := emptyRoom(cap)
	if empty <= 0 {
		// A segment that cannot hold the first record at all leaves the
		// sum undefined; the caller's appends cannot place even the
		// first byte, so the ask is the payload alone and the very next
		// limit in the order answers the write.
		return recordBytes(payload)
	}
	if room > 0 {
		rest -= room
	}
	// The closed form of the pieces: full empty-segment records, and one
	// last record for whatever tail remains. Overhead-safe: the count is
	// taken before the bytes it multiplies, so a payload near MaxInt64
	// cannot wrap the piece count before the sum names its refusal.
	pieces := (rest-1)/empty + 1
	full := pieces - 1
	tail := rest - full*empty
	each := recordBytes(empty)
	// The sum is taken only when the product cannot wrap: a piece count
	// near MaxInt64 over its record size names a write no limit could hold,
	// so the figure returns the largest an int64 names and the take's own
	// overflow-safe refusals answer with it, never a wrapped negative an
	// admission could mistake for a fit.
	if full > 0 && full > (math.MaxInt64-2*each-2*recordBytes(payload))/each {
		return math.MaxInt64
	}
	var total int64
	if room > 0 {
		// The first record takes the room the segment still has; the
		// pieces past it are empty segments' whole records but the
		// tail's.
		total = recordBytes(min64(payload, room)) + full*each
		if tail > 0 {
			total += recordBytes(tail)
		}
	} else {
		// No room at all: every piece is an empty segment's whole
		// record but the tail's.
		total = full * each
		if tail > 0 {
			total += recordBytes(tail)
		}
	}
	return total
}

// recordBytes is what one write record of n payload bytes costs: the
// header, the write kind's fixed body bytes, the payload and the padding
// to the record's 8-byte grid — the size function the format spells and
// the stream's own fit charges.
func recordBytes(n int64) int64 {
	return format.RecordHeader{Kind: format.KindWrite, Extent: core.Extent{Len: n}}.Size()
}

// emptyRoom is the payload bytes one write record takes in an empty
// segment of cap bytes: the cap less the segment header, floored to the
// record grid, less the seal's reserve for the one record and the
// record's own overhead.
func emptyRoom(cap int64) int64 {
	room := ((cap - format.SegmentHeaderSize -
		(format.SealMarkerSize + format.CatalogEntrySize + format.TrailerSize)) &^ 7) -
		(int64(format.RecordHeaderSize) + format.RecordHeader{Kind: format.KindWrite}.BodySize())
	return max(room, 0)
}

// slackOverhead is the one extra record's own bytes the admission charges
// over the split the room names: the piece the write draws when another
// write on its stream spends the room between the take and its appends.
func slackOverhead() int64 {
	return recordBytes(0)
}

// sparesFor returns how many whole segments n bytes roll into: zero when
// they are none, and at least one whenever they are — a roll never asks
// for the zero spares that hold nothing — one more per cap they cross
// after the first. The ceiling is taken without the sum, so an n near
// MaxInt64 cannot wrap before it divides.
func sparesFor(n, cap int64) int {
	if n <= 0 {
		return 0
	}
	return int((n-1)/cap + 1)
}

// readySpares caps what ready spares can cover at the need: a pool readier
// than the write needs covers it whole, never more than it needs.
func readySpares(ready, need int) int {
	if ready > need {
		return need
	}
	return ready
}

// asNoSpace returns err as a *core.NoSpaceError, or nil when it is not
// one: the shape of a limit's own refusal, forwarded and counted as the
// refusal it is — errors.As, so a refusal a layer wrapped still counts
// and still spans.
func asNoSpace(err error) *core.NoSpaceError {
	var ns *core.NoSpaceError
	if errors.As(err, &ns) {
		return ns
	}
	return nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
