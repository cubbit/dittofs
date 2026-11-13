package recovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"runtime"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// Deps is what recovery reaches the directory with. The open fills it from
// its own config; nothing here resolves options or defaults.
type Deps struct {
	FS     storage.FS
	Clock  core.Clock
	Tracer core.Tracer
	Log    *slog.Logger

	// ExtentLimit is the index entry bound the rebuilt index carries into
	// the journal's later writes. Zero is no bound. A rebuild over the bound
	// still builds: the bound refuses later writes, never the open.
	ExtentLimit int64

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
// rebuilt into with its counters and scans. Every field a later phase would
// fill is its own phase's to grow with, and nothing outside reads a copy of
// a field attached already holds.
type State struct {
	// Attached is what the attach pass found in the directory.
	Attached Attached

	// Rebuilt is what the rebuild phase built: the index, its counters,
	// every own segment's scan and the resync count.
	Rebuilt rebuilt

	// Tails is the torn-or-corrupt classing of every own and unverified
	// segment: each one's fate, the damaged set and the corrupt drops.
	Tails tails
}

// phase is one step of the open's recovery: the span it runs under, and
// the work it does over the reader the run built, filling the state it
// rebuilt into st. A phase is one arm of the list below; a phase not yet
// built runs as a no-op whose span still opens.
type phase struct {
	span core.SpanOp
	run  func(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, floor core.Version) error
}

// String names the phase by its span.
func (p phase) String() string { return string(p.span) }

// phases is the open's recovery, each step in the order the open runs it.
// The marks phase runs twice with two jobs — raising and withholding first,
// the lost-tail check after the tail's proof — each run its own span and
// its own context check, which the list keys on the phase itself rather
// than the span, so two phases may share one.
var phases = [...]phase{
	{span: core.SpanOpenAttach, run: runAttach},
	{span: core.SpanOpenRebuild, run: runRebuild},
	{span: core.SpanOpenMarksRaise},
	{span: core.SpanOpenTailProof},
	{span: core.SpanOpenMarksLostTail},
	{span: core.SpanOpenSpares},
	{span: core.SpanOpenReappend},
	{span: core.SpanOpenAssembly},
	{span: core.SpanOpenSeal},
	{span: core.SpanOpenEmptying},
}

// runAttach is the attach pass as a phase of the run: over the reader the
// run built, so its collected ranges reach the open's refusal below.
func runAttach(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, _ core.Version) error {
	att, err := attach(ctx, deps.FS, rd, expect, deps.Log)
	st.Attached = att
	return err
}

// runRebuild is the rebuild phase: it scans the segments the attach pass
// attached, through the same reader the open refuses on. The index carries
// deps.ExtentLimit, no bound when it is unset; the builder never consults the
// bound while rebuilding, so a directory past it still opens.
func runRebuild(ctx context.Context, deps Deps, rd *reader, st *State, expect core.JournalID, floor core.Version) error {
	limit := deps.ExtentLimit
	if limit < 1 {
		limit = math.MaxInt64
	}
	// The unverified-header and zero-header segments' records verify by
	// identity, segment and offset whatever their header slot says, so they
	// scan too: a record a later sync proved synced is never lost to a slot
	// nobody wrote.
	segs := slices.Clone(st.Attached.Own)
	for _, id := range slices.Concat(st.Attached.Unverified, st.Attached.ZeroHeader) {
		segs = append(segs, SegmentInfo{ID: id, Unverified: true})
	}
	out, err := rebuildIndex(ctx, rd, expect, segs, floor, limit, rebuildReaders(deps), deps.rebuildOrder, nil, deps.rebuildForceScan)
	if err != nil {
		return err
	}
	st.Rebuilt = out
	st.Tails, err = classifyTails(rd, expect, st.Attached, out.Scans)
	return err
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
// every phase span is the open span's child. Only the attach pass is
// built; the phases after it are named, span-covered steps that change
// nothing, each checked for the context, so a cancelled open is answered
// at the boundary the cancellation reached and the directory is left for
// a later open. floor and marks are carried for the phases that consume
// them — the rebuild, the marks and the tail's proof — and are unread
// until one exists.
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
			perr = ph.run(phCtx, deps, rd, &st, expect, floor)
		}
		// decision: a phase not yet built changes nothing. Its step is a
		// named, traced no-op, and the phases around it keep this list,
		// so each one lands with its contract and its own span and
		// context check, without reordering anything. State stays zero
		// where a later phase would fill it.
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
// and gets a no-op end.
func startSpan(ctx context.Context, deps Deps, op core.SpanOp) (context.Context, func(error)) {
	if deps.Tracer == nil {
		return ctx, func(error) {}
	}
	return deps.Tracer(ctx, op)
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
