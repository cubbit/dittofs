package core

import (
	"context"
	"log/slog"
)

// Tracer starts a span for op, with attrs, as a child of the span ctx carries,
// and returns the context carrying the new span with the function that ends it
// with the operation's error, nil when it succeeded. The journal runs the
// operation's nested work under the returned context, so a span the journal
// opens inside it is that span's child. A journal opened without a tracer
// traces nothing. A tracer that records nothing returns ctx and a nil end,
// which the journal does not call.
//
// The journal calls a tracer, and the end function it returns, with no
// journal lock held. Neither may block, and neither may call back into the
// journal.
//
// decision: a journal span is a child of the caller's span, carried by the
// context of the call that started the work. Work the journal starts on its
// own, with no caller, runs under a background context, so its span is a root,
// as RFC 25 §6.1 makes background work.
type Tracer func(ctx context.Context, op SpanOp, attrs ...slog.Attr) (context.Context, func(err error))

// SpanOp names the operation a span covers. Its values are the span names
// operators see, so none carries the journal's version. Each is
// journal.<Name>, the journal's prefix from RFC 25's prefix table, as RFC 25
// §6.1 requires of every span name.
//
// decision: no span op is declared ahead of the code that emits it, and each
// new emitter lands with the constant for its own op. A span name is what
// traces and dashboards key on once a release emits it, so a phase the open
// runs as a named no-op counts as having its emitter all the same — its op
// is one a trace can already name — while an op nothing emits is the dead
// weight a name declared ahead of its emitter would be.
type SpanOp string

// SpanSync covers one sync the journal issues, whether a caller asked for it
// or the sync bound did. It ends with the sync's own error.
const SpanSync SpanOp = "sync"

// SpanReserveRefused covers one reservation the journal refused by a named
// limit before any byte was written. It ends with the refusal's own error.
const SpanReserveRefused SpanOp = "reserve_refused"

// SpanFill covers one fill the journal runs: the parts of an extent nothing
// held, appended and held offloaded. It ends with the fill's own error.
const SpanFill SpanOp = "fill"

// SpanOpen covers the open's recovery, Run: the phases from the attach pass
// on, the reads they made and the refusal Run ended with. The steps Open
// runs before it — the platform checks, the format read, the identity
// check — are outside. It ends with the error that failed the recovery,
// nil when every phase ran clean.
const SpanOpen SpanOp = "open"

// SpanOpenAttach covers the attach pass: the one listing, the .spare.tmp
// unlinks and the classification of every segment header. It ends with the
// pass's own error — a cancelled context, a version the binary does not
// read — nil when every name was classed.
const SpanOpenAttach SpanOp = "open.attach"

// SpanOpenRebuild covers the rebuild of the placement index from the
// segments the attach pass classed. It ends with the error that failed the
// rebuild, nil when the index is whole. Not built: it ends nil today.
const SpanOpenRebuild SpanOp = "open.rebuild"

// SpanOpenMarksRaise covers the sync marks' first job, raising the counters
// they name and withholding what they cover. It ends with the error of that
// step, nil when every mark was applied. Not built: it ends nil today.
// SpanOpenMarksRaise and SpanOpenMarksLostTail share the value "open.marks":
// the marks phase runs twice with two jobs, and each run is its own span of
// the one op the phase has.
const SpanOpenMarksRaise SpanOp = "open.marks"

// SpanOpenTailProof covers the unproven tail's verification on the device:
// the first read, the sync and the direct re-read. It ends with the error
// that failed the proof, nil when the tail stands. Not built: it ends nil
// today.
const SpanOpenTailProof SpanOp = "open.tail"

// SpanOpenMarksLostTail covers the sync marks' second job, the lost-tail
// check once every unproven tail is proven. It ends with the refusal of a
// mark whose tail was lost, nil when every mark's tail is proven or cut.
// Not built: it ends nil today. The value it shares with
// SpanOpenMarksRaise is the one op of the phase that runs twice; see there.
const SpanOpenMarksLostTail SpanOp = "open.marks"

// SpanOpenSpares covers the spare pool's part of the recovery: the
// re-adoption of the segments that pass as spares and the directory sync
// before one is taken. It ends with the error of a re-adoption or of that
// sync, nil when the pool is set. Not built: it ends nil today.
const SpanOpenSpares SpanOp = "open.spares"

// SpanOpenReappend covers the re-appending of the tail the device lost and
// of any record the phases before it ended a segment of. It ends with the
// error of a re-append, nil when every record is re-appended whole. Not
// built: it ends nil today.
const SpanOpenReappend SpanOp = "open.reappend"

// SpanOpenAssembly covers the assembly of the rebuilt state into the
// running journal. It ends with the error that failed the assembly, nil
// when the journal is built. Not built: it ends nil today.
const SpanOpenAssembly SpanOp = "open.assembly"

// SpanOpenSeal covers the cut and seal: truncating every torn tail,
// sealing what the open ends and appending the open's loss records. It
// ends with the error of a truncation or a seal, nil when every segment
// the open ends is sealed. Not built: it ends nil today.
const SpanOpenSeal SpanOp = "open.seal"

// SpanOpenEmptying covers the emptying of the segments the open ended:
// their records re-appended whole, the segment unlinked and the directory
// synced. It ends with the error of the emptying, nil when every ended
// segment is emptied. Not built: it ends nil today.
const SpanOpenEmptying SpanOp = "open.emptying"
