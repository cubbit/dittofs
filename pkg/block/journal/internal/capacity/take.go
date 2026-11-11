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
// headroom's BelowHalf, the set's Footprint and ShareFigures) are called
// as snapshots outside the decision section, never under it.
type Config struct {
	// Admit is the read-only check: a non-nil error is the sentinel that
	// journal-wide state carries, and a write is refused with it at once,
	// consulting no limit and reserving nothing.
	Admit func() error

	// CheckIndex predicts the index entries a write would add and its
	// refusal against the index's bound, without reserving: its error is
	// returned untouched as the check made it. Take reads the count it
	// returns to refuse a write whose own entries would eat the ones open
	// reservations hold.
	CheckIndex func(file core.FileID, e core.Extent) (int64, error)

	// CheckFillIndex predicts the index entries a fill of an extent would add
	// and its refusal against the index's bound, without reserving: its error
	// is returned untouched, and its count is read as CheckIndex's is. Take
	// runs it in CheckIndex's place for a request whose Fill flag is set.
	CheckFillIndex func(file core.FileID, e core.Extent) (int64, error)

	// VersionsLeft reports how many versions remain below the version
	// limit; zero means the next draw is refused.
	VersionsLeft func() uint64

	// DrawVersion issues the version one write carries, or refuses with the
	// counter's named NoSpaceError. The wiring accepts the counters' own
	// Draw. It takes no lock of this domain — the counter is one atomic
	// swap — so the admission calls it inside its section, where the version
	// check and the draw are one step: a plain write cannot spend a version a
	// reservation holds between the check and the draw.
	DrawVersion func() (core.Version, error)

	// EntriesLeft reports how many placement index entries remain below the
	// index's bound; zero means the next entry is refused. It takes no lock:
	// the index reads its count and limit as two atomic loads.
	EntriesLeft func() int64

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

	// BelowHalf reports whether the headroom's drawing side — its size
	// without the open reserve — has fallen below half while the journal
	// holds its limit: the content-write throttle. It takes the headroom's
	// own mutex, never held across a pool or set read, and Take reads it as
	// a snapshot outside the decision section, so it never runs under
	// Take's mutex.
	BelowHalf func() bool

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
	// Fill marks the request a fill's admission, not a client write's: the
	// version limit does not gate it and CheckFillIndex runs in CheckIndex's
	// place, every other limit unchanged. It is set by the journal's own
	// fill path alone, for the public Fill carries no such parameter.
	Fill bool
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

	// heldEntries is the placement index entries open reservations hold and
	// no write of theirs has drawn yet: Take and Reserve size their own asks
	// against what the index has left after these.
	heldEntries int64

	// heldVersions is the versions open reservations hold and no write of
	// theirs has drawn yet, counted beside VersionsLeft the same way.
	heldVersions uint64

	// reservedAsk is the journal-side bytes open reservations hold — their
	// payload bytes and the footer entry per piece — charged against the
	// journal's maximum beside the footprint, so two reservations cannot
	// claim one room. It is separate from reserved, whose grants' bytes sit
	// inside allocated segments and so already in the footprint.
	reservedAsk int64

	// pendingSeats is the whole segments open admissions have reserved
	// against the journal's maximum whose spares no hold has served yet.
	// A take whose coverage falls short of its need reserves the
	// difference here inside the decision section, so a second take in the
	// window before the first's hold answers cannot claim the same room
	// (RFC 1 §6: the journal must not accept bytes that exceed the
	// maximum, however its callers interleave). Not every hold-less take
	// before the first's hold answers is covered — a refused hold's seats
	// return only once its own take has heard the pool's answer, and an
	// admission the journal's maximum refused never seats anything — so
	// the figure is a conservative window, not a ledger of every admission
	// in flight. The hold's answer settles it: a served hold's spares are
	// real segments in the footprint, and a refused one returns its
	// reservation whole. It can never go negative: it moves only under mu,
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
	if cfg.Admit == nil || cfg.CheckIndex == nil || cfg.CheckFillIndex == nil ||
		cfg.VersionsLeft == nil || cfg.DrawVersion == nil || cfg.EntriesLeft == nil ||
		cfg.Footprint == nil || cfg.ShareFigures == nil || cfg.Room == nil ||
		cfg.ReadySpares == nil || cfg.Hold == nil || cfg.BelowHalf == nil {
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

	// bytes is the record bytes the grant reserved: the split's.
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

	// version is the version the write carries: the one the admission drew in
	// its section, or the one a reservation drew for its write.
	version core.Version

	// reservation is the reservation the grant was drawn from, nil for an
	// admitted write's grant: a drawn grant borrows its reservation's hold and
	// returns its unlanded bytes to the reservation rather than to the pool.
	reservation *Reservation

	// heldEntries is the index entries a drawn grant still has to hand over:
	// the reservation's held figure keeps them until the publish claims them,
	// so a plain write cannot pass the bound's check and eat them in the
	// window between the draw and the publish.
	heldEntries int64

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
// order: the read-only sentinel, then the headroom threshold — a content
// write below half the headroom's drawing side, first among the limits
// because only the engine's own reclaim clears it — then the version
// limit, the index bound, the share's limit, the journal's maximum, and
// the spare pool last — naming its own missing amount and unit. A refusal
// performs no I/O, never waits and never evicts; each is counted by
// RefusedCount and opened as one reserve_refused span. A fill request (Request.Fill) is
// gated by the same limits in the same order, with two differences: a fill
// draws no version, so the version limit passes it, and CheckFillIndex runs
// in CheckIndex's place. An error the
// filesystem itself returned (ENOSPC from a spare the pool reached for) is
// passed through as itself and counts nothing.
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
	// The headroom's gate reads the set's footprint under its lock, so it
	// joins the snapshots: never under the section.
	belowHalf := c.cfg.BelowHalf()
	// The headroom threshold: a content write is refused while the journal
	// holds its limit and the headroom's drawing side has fallen below
	// half — one entry short, nothing held, the one limit only the
	// engine's own reclaim clears.
	if belowHalf {
		return nil, c.refuse(ctx, *refuseHeadroom(1))
	}
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
	// is any record a path other than WriteAt appends under the tag, and
	// two exist: the durability layer's re-append of a failed window's
	// records, which carries the record's original tag, and its loss
	// records, which carry the dropped record's share — both land in the
	// set's settled figure and never in the tally, for no grant of theirs
	// ever ran a Wrote. While their bytes are live the tally undercounts:
	// the share's limit fails OPEN by exactly them, and it takes a window
	// that failed a sync, or a record the layer dropped, to append them —
	// the re-append of no record runs on a journal whose syncs all
	// complete. Their segment's retirement then fails open further still:
	// the retirement moves their bytes from the settled figure into the
	// retired one, and the Retired call below subtracts a delta the tally
	// never held, driving it lower by up to the re-appended and loss
	// record bytes the segment carried. The marker names the bound: writes
	// the share's limit admits past it sum to at most the re-appended and
	// loss record bytes retired under the tag, on a journal that has
	// already failed a sync or dropped a record. Closed by feeding those
	// appends into the tally — a per-tag cumulative added counter in the
	// set, seeded and reconciled beside the retired pair — or by driving
	// them through this admission. Overturned by either.
	c.mu.Lock()
	c.seedFor(r.Tag)

	// Version limit: a write with no version left to draw is refused
	// naming it before any byte is reserved. The versions open reservations
	// hold are not the write's to spend, so the check sees them beside the
	// counter.
	//
	// decision: a fill skips this check because it draws no version — its
	// content lands under the version the caller read at — so the version
	// limit does not gate it. Withdraw the skip if a fill ever draws one.
	if !r.Fill {
		if left := c.cfg.VersionsLeft(); left < c.heldVersions+1 {
			c.mu.Unlock()
			return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitVersion, Missing: 1, Unit: core.UnitVersions})
		}
	}

	// Index bound: the pre-check's refusal is forwarded as the check
	// returned it, and counts as Take's refusal once. The index counts it
	// in its own counter too — that is its count of the refusals its bound
	// caused, and this one is Take's of the reservation it refused; neither
	// counts the other's, so the one reservation is never counted twice
	// here. A fill's request runs the fill pre-check here, in the write's
	// place.
	check := c.cfg.CheckIndex
	if r.Fill {
		check = c.cfg.CheckFillIndex
	}
	adds, err := check(r.File, r.Extent)
	if err != nil {
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
	// decision: the sum is the split's record bytes alone, with no slack
	// for a piece the split does not know of. The room between this take
	// and this write's appends can be spent by more than another admitted
	// writer: the idle sealer seals the active segment and leaves the
	// stream none; the durability layer appends re-appended records and
	// its loss records under their tags with no reservation of their own;
	// and a same-stream writer whose grant holds spares reserves for its
	// first piece whatever room it finds. Whatever spent the room, only
	// the write's first cut moves (every later cut falls at an empty
	// segment's edge), so such a write draws at most one piece beyond the
	// split. That piece's header is outside the charge: its share-limit
	// overshoot is at most one record header per write whose room was
	// spent by something outside the admission, in the fail-open
	// direction, bounded, and closes with the reserved-roll replacement
	// (#3950) that also closes the unreserved roll such a piece makes.
	// The premise holding the bound is the stream's own split: a stream
	// that cut a payload anywhere but an empty segment's edge could draw
	// more than one extra piece, and the overshoot would exceed one
	// header.
	bytes := recordSplit(r.Payload, roomNow, c.cfg.SegmentCap)
	// The room this grant can claim is what the stream names less what
	// the grants still open on it already hold. The split a write's
	// appends can draw is bounded by that claim twice over: the bytes the
	// charge sums are the split the room the section read names, and the
	// spares below are sized by the pieces the stream's own split draws
	// past the claimable room — an earlier grant lands at most what it
	// claimed, record bytes against a payload figure, so the room this
	// write's appends find never falls below roomLeft and its rolls never
	// exceed the pieces sized here.
	roomLeft := roomNow - c.grantedRoom[r.Stream]
	var (
		roomPart int64 // the room bytes this grant covers
		sparesN  int   // the spares the pieces beyond the room need
	)
	switch {
	case r.Payload <= roomLeft:
		// The whole write sits in the room this grant can claim: one
		// record, no spare. decision: the claim is the record's bytes
		// plus the footer entry its landing also takes from Room — exactly
		// what the landing lowers the stream's room by, no slack — so two
		// writes that together fit the segment are both granted from the
		// room, and two takes can never grant one window; clamped at the
		// room left, the ledger claims no window an earlier grant holds.
		// The spare count and the journal-maximum seats below keep no
		// slack either: the piece a displaced write draws beyond them is
		// the unreserved roll of the window above (#3950).
		roomPart = min64(bytes+format.CatalogEntrySize, max(roomLeft, 0))
	case roomLeft > 0:
		roomPart = roomLeft
		sparesN = sparesBeyondRoom(r.Payload, roomLeft, c.cfg.SegmentCap)
	default:
		sparesN = sparesBeyondRoom(r.Payload, 0, c.cfg.SegmentCap)
	}

	// The pieces the write will publish: one record per segment its split can
	// reach. A write that rolls publishes one index entry per piece, never one
	// for the whole extent — pieces never join at run time — so a plain
	// write's real adds are the pre-check's count plus one per extra piece,
	// and each of those can eat a held entry.
	//
	// decision: the extra pieces are counted only while a reservation holds
	// entries (heldEntries > 0), so a journal with no reservation keeps the
	// pre-check's own figure exactly — a zero-gap fill or a net-zero write at
	// a full index stays admitted as the pre-check admits it. A write that
	// adds nothing publishes no entry and adds no piece. The figure fails
	// closed: the pieces are the split the room the section read names, and a
	// write whose room something else spent draws one piece beyond it, refused
	// at its publish rather than admitted past the bound here.
	if c.heldEntries > 0 && adds > 0 {
		// The publish count mirrors the three ways the split above names: a
		// write the room covers publishes one piece; one the room only partly
		// covers publishes its head there and one more per segment past it; a
		// roomless one publishes one per spare, the first included. Only the
		// partly covered case adds the room's own piece back —
		// sparesBeyondRoom leaves it out there, and already counts it when the
		// stream has no room at all.
		var published int64
		switch {
		case r.Payload <= max(roomLeft, 0):
			published = 1
		case roomLeft > 0:
			published = 1 + int64(sparesBeyondRoom(r.Payload, roomLeft, c.cfg.SegmentCap))
		default:
			published = int64(max(sparesBeyondRoom(r.Payload, 0, c.cfg.SegmentCap), 1))
		}
		adds += published - 1
		// The entries open reservations hold are not the write's to spend: the
		// pre-check sized the write's own entries against the index's count,
		// and this names them, the extra pieces included, against the room the
		// held entries leave. A write that covers a held piece — an overwrite
		// that supersedes and splits, up to two entries where a plain write
		// names one — is refused where it alone would have fitted and would
		// have spent entries a reservation holds.
		//
		// decision: the held figure is left to the pre-check's count and adds
		// rather than tested alone, because a check that only compared the
		// entries left against the held ones refused a zero-gap fill at a full
		// index where the pre-check admitted it. The bound is checked twice —
		// the pre-check's own test names the write's entries against the whole
		// count, and this names them against the room the held entries leave —
		// and the stricter refusal answers.
		if left := c.cfg.EntriesLeft(); left-c.heldEntries < adds {
			missing := adds - (left - c.heldEntries)
			c.mu.Unlock()
			return nil, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitIndex, Missing: missing, Unit: core.UnitEntries})
		}
	}

	// Share limit: the share's settled record bytes — this package's own
	// tally — plus the record bytes its open grants hold, plus this
	// write's, must fit the limit. Every figure moves only under mu, so
	// the boundary is exact between reservations: two writers at the last
	// byte resolve in the order their sections take the mutex, never both
	// granted and never both refused.
	//
	// decision: the ask the share's limit sees is the split's record
	// bytes and no more, so a reservation of exactly the remaining space
	// is granted (RFC 1 §6, §9.2). The limit can be passed by at most one
	// record header per write whose room was spent by something outside
	// the admission and which therefore drew a piece beyond its split:
	// fail-open, bounded, and closed with the reserved-roll replacement
	// (#3950). Overturned by a stream that can promise the room it
	// measured, which no interleaving caller can.
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
	// the pool's answer alone. The bytes open reservations hold are not
	// the write's to spend either: their ask rides beside the footprint,
	// for it names room in segments not yet allocated.
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
			headroom := c.cfg.MaxBytes - footSnap - c.reservedAsk
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

	// The version is drawn here, inside the section and after every refusal,
	// so its check above and the draw are one step: a plain write cannot pass
	// Take's version check and have a reservation's own draw or another plain
	// write's draw take the version it was admitted on. A fill draws no
	// version — its content lands under the version the caller read at — so it
	// skips the draw and leaves the zero version, as before.
	//
	// decision: a take the spare pool refuses after this spends the version it
	// drew, for the hold runs outside the section and cannot be ordered before
	// the draw without holding the section across the pool's lock. A spent
	// version is a gap in the counters, which every reader tolerates: a version
	// is issued once and never reused, and the floor and limit arithmetic work
	// on it. Withdrawn if a caller needs the counter gapless.
	var v core.Version
	if !r.Fill {
		var err error
		if v, err = c.cfg.DrawVersion(); err != nil {
			c.mu.Unlock()
			if ns := asNoSpace(err); ns != nil {
				// Counted and spanned for the caller; the error returned is
				// this same refusal.
				return nil, c.refuse(ctx, *ns)
			}
			return nil, err
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
			roomPart: roomPart, spares: sparesN, hold: hold, version: v,
		}, nil
	}

	return &Grant{
		c: c, tag: r.Tag, stream: r.Stream, bytes: bytes, left: bytes,
		roomPart: roomPart, version: v,
	}, nil
}

// seedFor seeds tag's own record-byte figure from the set's pair once per
// tag, and takes a racing Retired call's already-reported bytes into account
// on the way in. It is called with mu held and holds it across the set's
// read by releasing it for that read and retaking it: the set's lock is of
// this same domain, and no two locks of one domain are held at once.
func (c *Capacity) seedFor(tag core.ShareTag) {
	if c.seeded[tag] {
		return
	}
	c.mu.Unlock()
	settled, retired := c.cfg.ShareFigures(tag)
	c.mu.Lock()
	if c.seeded[tag] {
		return
	}
	c.seeded[tag] = true
	// The retirements a racing Retired call already reported — whose
	// set-decrements landed after this read, so the settled figure still
	// holds their bytes — are the part of retiredSeen that the pair's own
	// retired figure has not caught up with: subtracting them here accounts
	// each once, and the baseline moves to the larger of the two figures so
	// every later call's delta starts from what this seed already took in.
	if over := c.retiredSeen[tag] - retired; over > 0 {
		settled -= over
	}
	c.shareTally[tag] = settled
	if c.retiredSeen[tag] < retired {
		c.retiredSeen[tag] = retired
	}
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

// RetiredEverySeeded tells Retired to every tag this admission holds a
// figure for, whether or not a share of it is registered at the moment: the
// retirement executor's hook runs per segment, with no tag of its own, and
// a share closed while its segment was waiting on the retirer would
// otherwise keep a stale-high figure until some unrelated retirement fired
// while a share of it was registered again — for the seed runs once per
// tag, and the reattached share's figure is the one the hook lowers, not
// the pair a fresh seed would read. A tag whose segment was not this one
// computes a delta of zero, exactly as Retired does, so the call costs one
// figure read per seeded tag and changes nothing it should not. A tag
// seeded for the first time after this call reads the pair, whose figures
// the retirement already moved, so no tag is missed by the snapshot the
// iteration takes.
func (c *Capacity) RetiredEverySeeded() {
	c.mu.Lock()
	tags := make([]core.ShareTag, 0, len(c.seeded))
	for tag := range c.seeded {
		tags = append(tags, tag)
	}
	c.mu.Unlock()
	for _, tag := range tags {
		c.Retired(tag)
	}
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
	// share's ledger holds only what the pieces have not landed yet,
	// which Release returns whole.
	g.c.mu.Lock()
	g.left -= n
	g.c.reserved -= n
	g.c.reservedTagged[g.tag] -= n
	g.c.shareTally[g.tag] += n
	// The room the landing consumed — the record's bytes and the footer
	// entry it also takes from Room, the two the claim covers — leaves the
	// stream's grant, so a take beside this landing sees the room the stream
	// has; what Release returns of it is only what the write never appended.
	// The room part bounds it, so a grant never gives back more than it
	// claimed.
	if g.roomPart > 0 {
		eat := min64(n+format.CatalogEntrySize, g.roomPart)
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
// them. A grant drawn from a reservation returns its unlanded bytes to the
// reservation instead, and leaves the reservation's hold open: the spares are
// the reservation's, and its later writes draw on them. It is idempotent — a
// second Release does nothing.
func (g *Grant) Release() {
	if g.released {
		return
	}
	g.released = true
	if g.reservation != nil {
		g.releaseDrawn()
		return
	}
	g.c.releaseReservation(g.tag, g.stream, g.roomPart, g.left)
	if g.hold != nil {
		g.hold.Close()
		g.hold = nil
	}
}

// releaseDrawn gives a drawn grant's unlanded bytes back to its reservation, so
// a later write of the reservation may draw them; the entries are handed back
// by releaseEntries beside it. They stay in the share's reserved figure
// throughout — a byte the write never wrote is still a byte the reservation
// holds — and the reservation's Release returns them. The journal's maximum
// keeps the reservation's whole ask until then, which fails closed: the room is
// in the spares the reservation holds, and no limit is consulted again while it
// holds.
func (g *Grant) releaseDrawn() {
	if g.reservation == nil {
		return
	}
	g.c.mu.Lock()
	// The bytes go back to the reservation's figure while it is open — its
	// Release returns them — or straight to the counters once it has released,
	// which returned only what its writes had not drawn and takes no more: a
	// grant released after its reservation must not leak its own charge.
	if back := g.left; back > 0 {
		g.left = 0
		if g.reservation.released {
			g.c.reserved -= back
			g.c.reservedTagged[g.tag] -= back
		} else {
			g.reservation.leftBytes += back
		}
	}
	g.c.mu.Unlock()
	// The entries the write never published go back after the bytes: a write
	// that failed part-way handed over the entries it did publish through
	// Claimed, so what is left here is the pieces it never appended.
	g.releaseEntries()
}

// Version returns the version drawn for the write when a reservation drew it,
// and the zero version for an admitted write's grant, whose write draws its
// own.
func (g *Grant) Version() core.Version { return g.version }

// Claimed hands over the index entries one piece of the write's split has just
// reserved in the index, releasing them from the reservation's held figure so a
// plain write beside it sees them spent only once the bound holds the piece. It
// is a no-op for an admitted write's grant, whose entries the admission already
// left to the index.
//
// It reads and moves the grant's own heldEntries with no lock of the grant's,
// so one goroutine at a time must call it — the write that holds the grant,
// which the append loop does one piece per call.
func (g *Grant) Claimed(entries int64) {
	if entries <= 0 || g.heldEntries <= 0 {
		return
	}
	if entries > g.heldEntries {
		entries = g.heldEntries
	}
	g.c.mu.Lock()
	g.heldEntries -= entries
	g.c.heldEntries -= entries
	g.c.mu.Unlock()
}

// releaseEntries hands a drawn grant's unclaimed entries back to its
// reservation, so a later write of the reservation may draw them: a write that
// never published its pieces never reserved them in the index either. A write
// that failed part-way calls Claimed for the pieces it did publish, so what it
// hands back here is only what it never published.
func (g *Grant) releaseEntries() {
	if g.heldEntries <= 0 || g.reservation == nil {
		return
	}
	g.c.mu.Lock()
	back := g.heldEntries
	g.heldEntries = 0
	// The held figure counts what the reservation may still spend — its own
	// leftEntries beside every drawn grant's unclaimed heldEntries — so an
	// entry handed back to an open reservation stays held and only moves to
	// leftEntries; the reservation's Release returns it. A reservation already
	// released returned what its writes had not drawn and took no more, so the
	// entry goes straight back to the bound here.
	if g.reservation.released {
		g.c.heldEntries -= back
	} else {
		g.reservation.leftEntries += back
	}
	g.c.mu.Unlock()
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

// DrawVersion issues the version a caller outside a reservation carries — a
// change stored elsewhere, a truncate's own record — and refuses with a
// NoSpaceError naming the version limit when nothing is left beside the
// versions open reservations hold: those are not the caller's to spend, as
// Take's own version check leaves them. The check and the draw are one
// section, so a reservation's draw racing it cannot pass between them.
func (c *Capacity) DrawVersion(ctx context.Context) (core.Version, error) {
	c.mu.Lock()
	if left := c.cfg.VersionsLeft(); left <= c.heldVersions {
		c.mu.Unlock()
		return core.Version{}, c.refuse(ctx, core.NoSpaceError{Limit: core.LimitVersion, Missing: 1, Unit: core.UnitVersions})
	}
	v, err := c.cfg.DrawVersion()
	c.mu.Unlock()
	return v, err
}

// HeldEntries returns the placement index entries open reservations hold and no
// publish has claimed yet: the figure Take leaves unspent for them.
func (c *Capacity) HeldEntries() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.heldEntries
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

// sparesBeyondRoom returns how many whole segments a write of payload bytes
// appends past the room its stream grants it: the pieces the stream's own
// split draws beyond the first — full empty-segment records and the tail's —
// each landing in a segment no roll of the write already holds. A write the
// room covers whole needs none. The pieces, not the bytes over the cap, are
// the figure: an empty segment takes segment.EmptyRoom(cap) payload bytes — the cap
// less the segment header, the seal's reserve and the record's own overhead —
// so a payload of two full segments and a tail sits in three pieces where its
// bytes over the cap name two, and a hold sized by bytes admits a write whose
// next-to-last roll is refused.
func sparesBeyondRoom(payload, room, cap int64) int {
	if payload <= 0 || payload <= room {
		return 0
	}
	empty := segment.EmptyRoom(cap)
	if empty <= 0 {
		// A segment that cannot hold the first record at all leaves the
		// pieces undefined — the stream will refuse the append itself —
		// so the best figure the bytes can name is the payload over the
		// cap, one segment per cap and a whole one for any tail.
		return int((payload-1)/max(cap, 1) + 1)
	}
	// The pieces of the rest: full empty-segment records and the tail's,
	// each in a segment of its own — the first piece, the one the room
	// holds, is not among them, and a roomless stream rolls even its
	// first record onto a spare.
	rest := payload - max(room, 0)
	return int((rest-1)/empty + 1)
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
		return segment.RecordBytes(payload)
	}
	empty := segment.EmptyRoom(cap)
	if empty <= 0 {
		// A segment that cannot hold the first record at all leaves the
		// sum undefined; the caller's appends cannot place even the
		// first byte, so the ask is the payload alone and the very next
		// limit in the order answers the write.
		return segment.RecordBytes(payload)
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
	each := segment.RecordBytes(empty)
	// The sum is taken only when the product cannot wrap: a piece count
	// near MaxInt64 over its record size names a write no limit could hold,
	// so the figure returns the largest an int64 names and the take's own
	// overflow-safe refusals answer with it, never a wrapped negative an
	// admission could mistake for a fit.
	if full > 0 && full > (math.MaxInt64-2*each-2*segment.RecordBytes(payload))/each {
		return math.MaxInt64
	}
	var total int64
	if room > 0 {
		// The first record takes the room the segment still has; the
		// pieces past it are empty segments' whole records but the
		// tail's.
		total = segment.RecordBytes(min64(payload, room)) + full*each
		if tail > 0 {
			total += segment.RecordBytes(tail)
		}
	} else {
		// No room at all: every piece is an empty segment's whole
		// record but the tail's.
		total = full * each
		if tail > 0 {
			total += segment.RecordBytes(tail)
		}
	}
	return total
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
