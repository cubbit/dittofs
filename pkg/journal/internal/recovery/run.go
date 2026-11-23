package recovery

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"runtime"
	"slices"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/internal/storage"
)

// Deps is what recovery reaches the directory with. The open fills it from
// its own config; nothing here resolves options or defaults.
type Deps struct {
	FS     storage.FS
	Clock  core.Clock
	Tracer core.Tracer
	Log    *slog.Logger

	// Failed is the directory's failed list at open: the sequence numbers an
	// earlier read-only journal on the same directory could not write the
	// loss record for. It is data the open hands the rebuild — the durability
	// layer's FailedFor answers it before any layer of the open is built —
	// not a call the rebuild makes back into that layer; nil is no failed
	// list.
	Failed []durability.Failed

	// ExtentLimit is the index entry bound the rebuilt index carries into
	// the journal's later writes. Zero is no bound. A rebuild over the bound
	// still builds: the bound refuses later writes, never the open.
	ExtentLimit int64

	// SegmentCap is the size the journal's own segments are made and
	// accounted at, the one figure two phases read: the spares phase sizes
	// a re-adopted candidate against it — a file not holding exactly the
	// cap is unlinked and left for the pool to make again — and the
	// assembly seeds the rebuilt segment ledger at it, the cap an active
	// or ended segment is accounted under and the post-seal size of a
	// sealed one measured against. The caller fills it from the journal's
	// settings; a cap that is not positive names no storage — the spares
	// phase refuses it naming it, and the seed the assembly makes refuses
	// it by panic, the set's own maker's answer to a caller bug.
	SegmentCap int64

	// SpareTargets is how many spares each of the pool's classes keeps, in
	// segments: it decides the class each adopted spare is recorded under,
	// in the adoption order — the repack reserve first, then the headroom,
	// then the content class, with every spare past every target taking the
	// content class, for none is ever orphaned (segment.AdoptClasses says
	// why the reserve goes before the headroom, which the refill does not).
	// The open fills it with the pool's own targets.
	SpareTargets segment.Targets

	// Liveness is the segment liveness tracker the journal the open
	// assembled registered its rebuilt records with, the one answer the
	// emptying asks of an attached segment: held or not. The open fills the
	// field from the journal it built once the loss pass has landed, so the
	// tracker's answer names the loss records it appended too. Nil is no
	// emptying: the phase answers the refusal below rather than deciding
	// liveness on its own, for a caller that hands no tracker answers
	// nothing this phase may trust.
	Liveness *reclaim.Tracker

	// Retire is the retirement path the assembled journal runs — recycle or
	// unlink, decided at the retirement itself, the pool's refill after each
	// included — the one path a running journal's own reclamation takes,
	// which the emptying reuses for every segment it retires. Nil is no
	// emptying, the same refusal as Liveness's.
	Retire func(id core.SegmentID, reason segment.Reason) (segment.Retirement, error)

	// DrawReserve hands back one spare of the pool's repack reserve whose id
	// is above floor, the one class an open's copies draw on — a ready one
	// that clears the floor, else a low one renamed above every id the
	// directory held, which needs no new space: the journal the open built
	// answers it with its pool's own take of that class, so the emptying
	// draws on the pool the assembled journal runs rather than a list
	// recovery keeps for itself. Its error — the pool's refusal when no
	// spare of the class is ready and none can be made, or the wait's word
	// for why the refill stopped — is the open's when it answers a draw. Nil
	// is no emptying, the same refusal as Liveness's.
	DrawReserve func(floor core.SegmentID) (core.SegmentID, error)

	// EndDeferral ends one deferred loss whose record the emptying copied
	// into its target: the durability layer the open built holds the
	// registry entry — a loss whose record the copy that retires its
	// segment owes — and gives it up here, once the copy holds the record
	// durable. Nil ends nothing: the registry keeps the entry and the next
	// open re-reads it, safe.
	EndDeferral func(seg core.SegmentID, l durability.Loss) bool

	// Dir is the journal's own directory, the one the per-directory
	// deferred-loss registry is keyed by: the retire step reads it the way
	// the open reads the failed list, and never retires a segment the
	// registry answers as holding a deferred loss's dropped record.
	Dir string

	// rebuildOrder reorders the segments the rebuild dispatches before it
	// starts a reader; nil keeps the listing's order, the one the open runs
	// in. It is unexported and set only by tests that need an order the
	// directory would not hand over.
	rebuildOrder func(own []SegmentInfo)

	// rebuildReaders bounds the segment readers the rebuild runs at once; 0
	// means GOMAXPROCS, what the open runs with. It is unexported and set
	// only by tests that need a bound the machine would not give.
	rebuildReaders int

	// rebuildForceScan makes the rebuild scan every segment rather than
	// read the footer a verified successor names, the test-only switch
	// that lets a check compare the two sources' answers.
	rebuildForceScan bool
}

// State is what recovery rebuilt of the journal's state: the attach pass's
// answer, Attached — the id high-water mark and the rejected count included
// there — and the rebuild phase's, Rebuilt, the index the records on disk
// rebuilt into with its counters and scans — replaced whole, index, counters
// and kinds alike, by the tail proof's second pass where that pass found a
// torn record to exclude. Every field a later phase would fill is its own
// phase's to grow with, and nothing outside reads a copy of a field attached
// already holds.
type State struct {
	// Attached is what the attach pass found in the directory.
	Attached Attached

	// Rebuilt is what the rebuild phase built: the index, its counters,
	// every own segment's scan and the resync count.
	Rebuilt rebuilt

	// Kinds is the decoded state of the aside records the rebuild set
	// aside: the loss and forget records it collected and the read-only
	// drops the feed will find. The kinds pass fills it.
	Kinds kindResults

	// Tails is the torn-or-corrupt classing of every own and unverified
	// segment: each one's fate, the damaged set and the corrupt drops.
	Tails tails

	// Proof is the tail proof's answer, the records the re-append phase
	// takes and the segments whose proof sync failed; the marks phase
	// reads Reappends for its lost-tail check.
	Proof proof

	// Marks is the marks raise phase's answer: the marks themselves, the
	// predecessor verdicts the spares phase holds takes by, and the
	// segments withheld from the spare test. The raise phase fills it.
	Marks marksResults

	// Spares is what the spares phase did with the segments it re-adopted:
	// each adoption, with the id the allocator owns again, and each id the
	// phase unlinked.
	Spares Spares

	// Assembled is what the assembly phase built over the rebuilt state:
	// the seeded segment ledger and the open's liveness registrations, the
	// state the open continues from. The assembly phase fills it.
	Assembled Assembled
}

// Reporting is the open's own figures about its recovery: the segments read
// by their catalog and by scan, the segments the attach pass rejected, the
// tails the cut-and-seal phase cut and sealed, and the scans that
// resynchronised past a bad range. The journal's Stats serves them as the
// open found them; the figures the running journal raises past its open are
// the core's, not these.
type Reporting struct {
	CatalogSegments  int
	ScannedSegments  int
	RejectedSegments int
	TornTails        int
	ScanResyncs      int

	// ReappendedBytes is the payload bytes the re-append phase wrote back
	// into its sealed target: the records a failed sync left unproven on
	// the device, copied out of whatever the tail's proof read whole. The
	// running journal's syncs count their own beside them.
	ReappendedBytes int64
}

// Reporting collects the phases' counting into the figures the opened
// journal serves. Every segment the rebuild read counts once, by whichever
// source built its scan — a verified footer named by a successor the catalog
// route, everything else the walk.
func (st State) Reporting() Reporting {
	return Reporting{
		CatalogSegments:  st.Rebuilt.ByCatalog,
		ScannedSegments:  len(st.Rebuilt.Scans) - st.Rebuilt.ByCatalog,
		RejectedSegments: st.Attached.Rejected,
		TornTails:        st.Tails.TornTails,
		ScanResyncs:      st.Rebuilt.ScanResyncs,
		ReappendedBytes:  st.Proof.ReappendedBytes,
	}
}

// phase is one step of the open's recovery: the span it runs under, and
// the work it does over the reader the run built, filling the state it
// rebuilt into st. A phase is one arm of the list below; a phase not yet
// built runs as a no-op whose span still opens.
type phase struct {
	span core.SpanOp
	run  func(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, floor core.Version, marks []core.SyncMark) error
}

// String names the phase by its span.
func (p phase) String() string { return string(p.span) }

// phases is the open's recovery, each step in the order the open runs it.
// The marks phase runs twice with two jobs — raising and withholding first,
// the lost-tail check after the tail's proof — each run its own span and
// its own context check, which the list keys on the phase itself rather
// than the span, so two phases may share one. The tail proof's second
// rebuild pass replaces the rebuilt index and counters before any later
// phase reads them — the spares and assembly phases and every phase after.
// The marks-raise phase is the exception its slot pays: it runs between
// the rebuild and the proof, so the second pass a torn tail made replays
// its answers over the fresh state — the marks' presence map re-raised
// over the fresh feed, their sequence numbers re-observed over the fresh
// counters, and the decrements the proof settled replayed over the fresh
// map.
var phases = [...]phase{
	{span: core.SpanOpenAttach, run: runAttach},
	{span: core.SpanOpenRebuild, run: runRebuild},
	{span: core.SpanOpenMarksRaise, run: runMarksRaise},
	{span: core.SpanOpenTailProof, run: runTailProof},
	{span: core.SpanOpenMarksLostTail, run: runMarksLostTail},
	{span: core.SpanOpenSpares, run: runSpares},
	{span: core.SpanOpenReappend, run: runReappend},
	{span: core.SpanOpenAssembly, run: runAssemble},
	{span: core.SpanOpenSeal, run: runSeal},
	// The emptying's entry keeps the list's order whole while the phase's
	// work runs past it: the entry opens this loop's span as the recovery's
	// own end, and Emptying — called by the open, once the loss records the
	// work gates on are durable — opens the same op over the work itself,
	// the retire step first and the emptying step after. Both steps stay in
	// empty.go, under this one span; the list never gains a second entry.
	{span: core.SpanOpenEmptying},
}

// runAttach is the attach pass as a phase of the run: over the reader the
// run built, so its collected ranges reach the open's refusal below.
func runAttach(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, _ core.Version, _ []core.SyncMark) error {
	att, err := attach(ctx, deps.FS, rd, expect, deps.Log)
	st.Attached = att
	return err
}

// runRebuild is the rebuild phase: it scans the segments the attach pass
// attached, through the same reader the open refuses on. The scans the
// workers buffer are decoded, classed and fed inside the rebuild, in that
// order — the kinds pass and the classing run before a single record is fed,
// over the same scans — and the phase's answer fills the State's rebuilt,
// kinds and tails fields. The index carries deps.ExtentLimit, no bound when
// it is unset; the builder never consults the bound while rebuilding, so a
// directory past it still opens. The marks name the sequence numbers whose
// on-disk presence the feed raises into the kinds' answer, the one question
// the lost-tail check asks of the records the walks attached.
func runRebuild(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, floor core.Version, marks []core.SyncMark) error {
	out, kinds, tl, err := rebuildOver(ctx, deps, rd, st.Attached, expect, floor, nil, nil, marks)
	if err != nil {
		return err
	}
	st.Rebuilt = out
	st.Kinds = kinds
	st.Tails = tl
	return nil
}

// rebuildOver dispatches the rebuild over the segments the attach pass
// answer names, the whole of what runRebuild and the tail proof's second
// pass read, so the two can never scan one segment two ways: skip holds the
// placements the second pass excludes from the feed — a record the proof
// judged torn, treated as never written — and settled, where not nil, is
// the tail classing whose fates the repoint reads in the second pass's
// place: the fresh scans' classing answers the pre-proof verdicts, while
// the fates the proof settled are the homes the open really keeps.
func rebuildOver(ctx context.Context, deps Deps, rd *reader, att Attached, expect core.JournalID, floor core.Version, skip map[index.RecordAt]bool, settled *tails, marks []core.SyncMark) (rebuilt, kindResults, tails, error) {
	limit := deps.ExtentLimit
	if limit < 1 {
		limit = math.MaxInt64
	}
	// The unverified-header segments' records verify by identity, segment
	// and offset whatever their header slot says, so they scan too, the
	// zero-header ones named or not: a record a later sync proved synced is
	// never lost to a slot nobody wrote, and a segment is a spare only where
	// no record header verifies anywhere in it, which only a scan shows.
	// The cost is that an all-zero spare is read to its end by the walk's
	// forward search, up to the whole pool's size at every open; that is
	// accepted, the search bounded by the largest in-flight reservation
	// being the upgrade that would withdraw it.
	named := successorSet(att.Own)
	segs := slices.Clone(att.Own)
	for _, id := range slices.Concat(att.Unverified, att.ZeroHeader) {
		segs = append(segs, SegmentInfo{ID: id, Unverified: true})
	}
	return rebuildIndex(ctx, rd, expect, segs, named, floor, limit, rebuildReaders(deps), deps.rebuildOrder, nil, deps.rebuildForceScan, deps.Failed, att, skip, settled, marks)
}

// rebuildReaders is the reader bound the rebuild runs under.
func rebuildReaders(deps Deps) int {
	if deps.rebuildReaders > 0 {
		return deps.rebuildReaders
	}
	return runtime.GOMAXPROCS(0)
}

// Run rebuilds the journal's state from its directory, running the open's
// recovery phases in order under one open span, checking ctx before each,
// and running each phase under the context its parent span returned, so
// every phase span is the open span's child. The attach pass, the rebuild,
// the marks raise, the tail's proof, the lost-tail check, the spares
// phase — the open's first writing one, which re-adopts the spare
// candidates and zeroes their dirty slots after refusing on every range no
// bounded read completed — and the re-append — which writes the tail the
// device lost into a sealed spare of the repack reserve — and the assembly
// and the cut-and-seal — which truncates, syncs and seals every torn-tail
// fate the earlier phases answered and truncates every ended one — are
// built; the emptying after it is a named, span-covered step whose work Run
// cannot run: it gates on the open's loss records, which the loss pass
// appends and syncs only past the recovery, over the journal the recovery
// assembled, so the phases entry covers the span alone and Emptying runs
// the work — the retire step first, the ended segments' emptying after — at
// the open, once the records are durable. A cancelled open answers the
// context's
// error at the boundary it reached, the directory left as it began for a
// later open — except for the open's writing phases, the spares phase, the
// re-append and the cut-and-seal:
// cancelling under the span of one stops it at its own context check or
// at the doorstep of its next write — a write already under way finishes
// and the next open settles what it left idempotently, but no write begins
// past the cancellation — and a cancellation after one keeps that phase's
// unlinks, zeroed slots, sealed re-appended copies and durable cuts and
// seals, which a later open re-reads and re-decides.
// floor and marks are carried for the phases that consume them — floor
// for the rebuild, and marks for the marks raise phase, which reads them
// into the state the spares phase holds its takes by, for the spares
// phase, which withholds every candidate a mark names, and for the
// lost-tail check, which refuses the open over a mark whose tail the
// device lost and no copy anywhere answers for.
func Run(ctx context.Context, deps Deps, floor core.Version, expect core.JournalID, marks []core.SyncMark) (st State, err error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	ctx, endOpen := startSpan(ctx, deps, core.SpanOpen)
	defer func() { endOpen(err) }()

	rd := newReader(ctx, deps.FS, deps.Clock, deps.Log)
	for _, ph := range phases {
		if err := ctx.Err(); err != nil {
			return State{}, err
		}
		phCtx, end := startSpan(ctx, deps, ph.span)
		perr := error(nil)
		if ph.run != nil {
			perr = ph.run(phCtx, deps, rd, &st, expect, floor, marks)
		}
		// decision: a phase whose step this loop does not run — the
		// emptying, whose work gates on the loss records — changes nothing
		// here. Its step is a named, traced no-op, and the phases around it
		// keep this list, so the phase lands with its contract, its own span
		// and context check, without reordering anything: the emptying's
		// SpanOpenEmptying opens here as the recovery's end and again under
		// Emptying over the work itself. State stays zero where a phase that
		// runs outside this loop would fill it.
		end(perr)
		if perr != nil {
			return State{}, perr
		}
	}

	// decision: any read the phases could not complete refuses the open
	// before it returns State, rather than opening a journal with holes it
	// would have to serve. The refusal names every range no bounded read
	// completed; it is withdrawn when an unread record can be held, as the
	// later unread set plans.
	//
	// Identity stays zero here: its rule fills it only when the caller
	// expected no journal in particular, from the one identity every
	// verifying header names — and an open that reached this run always
	// named one: the identity check the open runs before recovery refuses
	// the expect-zero caller, or one whose identity the format file does
	// not name, before any phase runs. So the only unreadable-format
	// refusal that names an identity is the one ReadFormat answers, for
	// the caller that expected none.
	ranges := rd.Unreadable()
	if len(ranges) > 0 {
		return State{}, &core.UnreadableRangesError{Ranges: ranges}
	}
	return st, nil
}

// startSpan starts a span for op through the deps's tracer, as a child of
// the span ctx carries, when one is given, and returns the context carrying
// the new span with the function that ends it with the operation's error,
// nil when it succeeded. A journal opened without a tracer keeps its ctx
// and gets a no-op end. attrs ride the span from its start: a Tracer takes
// attributes only when it opens a span.
func startSpan(ctx context.Context, deps Deps, op core.SpanOp, attrs ...slog.Attr) (context.Context, func(error)) {
	if deps.Tracer == nil {
		return ctx, func(error) {}
	}
	return deps.Tracer(ctx, op, attrs...)
}

// ReadFormat reads and decodes the journal's format file through the open's
// retry bound: every error of a read — a device fault on the read itself
// and a failed open, a file vanished after the listing that showed it — is
// retried the same three times, never judged on the bytes the attempt did
// not return. A file shorter than a whole format file is handed to the
// decoder — its length is the format's own rule to judge, not the storage's
// — and the decoder's error is returned as is. A read no bounded attempt
// completed is answered with an UnreadableRangesError whose Format is set:
// a format file no read names names no identity, so it is the one
// unreadable file the ranges' error names by flag rather than by range.
//
// With expect zero the refusal still names the journal, where the verifying
// segment headers agree: the caller expected none in particular, and the
// one identity every header verifies under is what the directory holds for
// it. The agreeing identity is read through the same retry bound, from
// headers alone, writing nothing — and where no header verifies, or two
// name different journals, the refusal's Identity stays zero: the answer
// says the format is unreadable and names nothing, never guessing from
// headers that do not agree.
func ReadFormat(ctx context.Context, deps Deps, expect core.JournalID) (format.FormatFile, error) {
	rd := newReader(ctx, deps.FS, deps.Clock, deps.Log)
	// One byte past the format's size is read as well, so a file holding a
	// whole format file and anything after it is handed to the decoder
	// whole: the decoder judges length strictly, and a read that stopped at
	// the format's own size would take a longer file as the valid prefix it
	// holds and cast the trailing bytes away.
	b := make([]byte, format.FormatFileSize+1)
	n, rerr := rd.ReadAt(format.FormatFileName, b, 0)
	// A cancellation says nothing about the file: it is the caller's error,
	// answered as itself, never the damaged verdict a decode of the bytes
	// the bound never returned would give.
	if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
		return format.FormatFile{}, rerr
	}
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		// decision: a format file no bounded read completed names no
		// identity of its own, so the refusal is by flag; the open fails
		// before it writes a byte and the directory is left as it stands.
		// Withdrawn when an unread record, the suspect segment and the
		// unread file can be held until the device reads again, as the
		// later unread set plans.
		//
		// Identity is filled only for the caller that expected no journal
		// in particular, from the identities the verifying segment headers
		// agree on; a caller that named an identity keeps the refusal
		// naming none, its own identity already known to it.
		var id core.JournalID
		if expect == (core.JournalID{}) {
			id = agreeingIdentity(ctx, deps, rd)
		}
		return format.FormatFile{}, &core.UnreadableRangesError{
			Format:   true,
			Identity: id,
		}
	}
	return format.DecodeFormatFile(b[:n])
}

// agreeingIdentity returns the one identity every verifying segment header
// in the directory names, zero where none verifies or where two name
// different journals. Only ReadFormat reaches it, and only for the caller
// whose expect is the zero identity — the one having no journal in
// particular — so the headers are read for what they say, never judged
// against one. The decode is against the zero identity, so a header that
// verifies by its checksum names its own — ErrForeign under none — and
// its bytes are never judged beyond the checksum the format's rules put
// on them.
//
// Each header read goes through the retry bound the open's other reads
// take; a header no bound completed, like a header that does not verify,
// is one the answer does not count. The one listing the pass starts from
// is the storage's own, outside the bound: it reads what the directory
// holds, faults included, and a listing that itself fails answers zero
// rather than refusing — the format file's refusal, already decided,
// carries the answer. A cancellation between headers ends the pass with
// the identity agreed so far, never an answer that waits: the refusal is
// the format file's either way, and the identity it names is only what the
// headers it did read agreed on.
func agreeingIdentity(ctx context.Context, deps Deps, rd *reader) core.JournalID {
	names, err := deps.FS.List()
	if err != nil {
		return core.JournalID{}
	}

	none := core.JournalID{}
	agree := none
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			// The cancellation stops reading more headers; the identity
			// the ones before it agreed on still names what the
			// directory holds, and the refusal below carries it.
			return agree
		}
		if kind, _ := format.ClassifyName(name); kind != format.NameSegment {
			continue
		}
		// Only the bytes the read returned are judged: a short file is its
		// own length, never the zeros a full-size buffer pads it with.
		b := make([]byte, format.SegmentHeaderSize)
		n, rerr := rd.ReadAt(name, b, 0)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			continue // no bound completed, or the context ended: nothing names an identity
		}
		b = b[:n]
		if !anyHeaderByte(b) {
			continue // a spare nobody took: an all-zero, short or empty slot
		}
		h, derr := format.DecodeSegmentHeader(b, none)
		switch {
		case derr == nil:
		case errors.Is(derr, format.ErrForeign):
			// It verifies, under the identity its header names: ErrForeign
			// under none only says it is not the zero journal's, so the
			// identity it names counts like any other verifying header's.
		default:
			continue // damaged, or a version this binary does not read
		}
		if agree != none && h.Identity != agree {
			return core.JournalID{} // two headers name different journals: none agrees
		}
		agree = h.Identity
	}
	return agree
}

// TakenSegment reports whether at least one segment file in names holds the
// header a take wrote. A finished spare answers false: it lives as its own
// <id>.seg file, its name no different from a taken segment's, and its
// header slot is all zero. Each slot is read through the open's retry
// bound like every other read, and only the bytes the read returned are
// judged: a short, empty, garbage or zero slot is nobody's take. A read
// the bound could not finish answers taken, for unread is not proven
// empty, and a caller that sees no taken segment gets to record the
// identity and open without an operator (RFC 28 section 3.4). The decode
// is against the zero identity, so any header that verifies counts,
// whichever journal it names.
func TakenSegment(ctx context.Context, deps Deps, names []string) bool {
	rd := newReader(ctx, deps.FS, deps.Clock, deps.Log)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return false
		}
		if kind, _ := format.ClassifyName(name); kind != format.NameSegment {
			continue
		}
		b := make([]byte, format.SegmentHeaderSize)
		n, rerr := rd.ReadAt(name, b, 0)
		if errors.Is(rerr, fs.ErrNotExist) {
			continue // no file at the name: nothing to read, nobody's take
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return true // unread is not proven empty: count the slot as taken
		}
		b = b[:n]
		if !anyHeaderByte(b) {
			continue // a spare nobody took: an all-zero, short or empty slot
		}
		if _, derr := format.DecodeSegmentHeader(b, core.JournalID{}); derr == nil || errors.Is(derr, format.ErrForeign) {
			return true
		}
		// damaged, or a version this binary does not read: nobody's take
	}
	return false
}
