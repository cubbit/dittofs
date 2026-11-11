package recovery

import (
	"context"
	"errors"
	"io"
	"log/slog"

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
}

// State is what recovery rebuilt of the journal's state. Today it carries
// the attach pass's answer, Attached — the id high-water mark and the
// rejected count included there; every field a later phase would fill is
// attached's to grow with its phase, and nothing outside reads a copy of a
// field attached already holds.
type State struct {
	// Attached is what the attach pass found in the directory.
	Attached Attached
}

// phases is the open's recovery, each step's span in the order the open
// runs it. The marks phase runs twice with two jobs — raising and
// withholding first, the lost-tail check after the tail's proof — each run
// its own span and its own context check.
var phases = [...]core.SpanOp{
	core.SpanOpenAttach, core.SpanOpenRebuild, core.SpanOpenMarksRaise, core.SpanOpenTailProof, core.SpanOpenMarksLostTail,
	core.SpanOpenSpares, core.SpanOpenReappend, core.SpanOpenAssembly, core.SpanOpenSeal, core.SpanOpenEmptying,
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
	for _, span := range phases {
		if err := ctx.Err(); err != nil {
			return State{}, err
		}
		phCtx, end := startSpan(ctx, deps, span)
		switch span {
		case core.SpanOpenAttach:
			att, aerr := attach(phCtx, deps.FS, rd, expect, deps.Log)
			end(aerr)
			if aerr != nil {
				return State{}, aerr
			}
			st = State{Attached: att}
		default:
			// decision: a phase not yet built changes nothing. Its step is a
			// named, traced no-op, and the phases around it keep this list,
			// so each one lands with its contract and its own span and
			// context check, without reordering anything. State stays zero
			// where a later phase would fill it.
			end(nil)
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
	ranges, _ := rd.Unreadable()
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
	some := false
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			// The cancellation stops reading more headers; the identity
			// the ones before it agreed on still names what the
			// directory holds, and the refusal below carries it.
			return agreeOrNone(agree, some)
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
		if some && h.Identity != agree {
			return core.JournalID{} // two headers name different journals: none agrees
		}
		agree = h.Identity
		some = true
	}
	return agreeOrNone(agree, some)
}

// agreeOrNone returns agree when some header verified, zero otherwise. A
// cancellation is the one case an agreeing identity is named without the
// whole directory read, so the rule lives once, beside the normal end.
func agreeOrNone(agree core.JournalID, some bool) core.JournalID {
	if !some {
		return core.JournalID{}
	}
	return agree
}
