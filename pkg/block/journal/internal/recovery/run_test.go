package recovery

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/raw"
)

// runDeps is the deps Run takes over the rig's seam, with nil tracer and
// logger, the defaults an Open without options gives them.
func (r *rig) runDeps() Deps {
	r.t.Helper()
	return Deps{FS: r.seam.FS(), Clock: r.clk}
}

// runDepsWithTracer is the rig's deps with a tracer recorded.
func (r *rig) runDepsWithTracer(tracer core.Tracer) Deps {
	d := r.runDeps()
	d.Tracer = tracer
	return d
}

// TestReadFormat_CancelKeepsTheIdentityAgreedSoFar cancels the header walk
// under a held read with a segment still to come: the headers before it
// agreed on one journal, the cancelled read counts nothing, and the next
// iteration's context check — not the walk's end — is where the answer
// settles. The refusal still names the journal the walk agreed on so far;
// an arm that drops the agreement on a cancellation answers zero and is
// what this fails.
func TestReadFormat_CancelKeepsTheIdentityAgreedSoFar(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(3, journalID)
	r.writeSeg(9, journalID)
	r.writeSeg(15, journalID)
	r.failReads(format.FormatFileName, 0)

	ctx, cancel := context.WithCancel(context.Background())
	held := r.seam.HoldFirst(t, kit.OpRead, format.SegmentName(9))
	done := make(chan error, 1)
	go func() {
		_, err := ReadFormat(ctx, r.runDeps(), core.JournalID{})
		done <- err
	}()

	// Segment 3's header is read and agrees; 9's read is held under it,
	// and 15 is still to come: the cancellation lands between them, so the
	// iteration for 15 is the one whose context check the walk answers on.
	<-held.Reached()
	cancel()
	held.Release()

	select {
	case err := <-done:
		var ure *core.UnreadableRangesError
		if !errors.As(err, &ure) {
			t.Fatalf("ReadFormat: a cancelled walk got err %v, want an UnreadableRangesError", err)
		}
		if !ure.Format {
			t.Errorf("ReadFormat: the refusal's Format flag is not set; the format file's read alone failed")
		}
		if ure.Identity != journalID {
			t.Errorf("ReadFormat: a walk cancelled between headers names identity %x, want %x: the one it agreed on so far", ure.Identity, journalID)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("ReadFormat: a walk cancelled between headers did not return")
	}
}

// TestReaderWarnsOfAnUncompletedRead reads a header that no attempt
// completes with a logger recording: before the range is collected the last
// error the bound saw is warned of, naming the file, so an operator steering
// a repair by the open's own logs has the fault that refused it. The
// policy is unchanged — everything unreadable is collected, never judged —
// and only the warning is what this checks.
func TestReaderWarnsOfAnUncompletedRead(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(12, journalID)
	r.failReads(format.SegmentName(12), 0)

	var buf bytes.Buffer
	rd := newReader(context.Background(), r.seam.FS(), r.clk, slog.New(slog.NewTextHandler(&buf, nil)))
	_, err := rd.ReadAt(format.SegmentName(12), make([]byte, format.SegmentHeaderSize), 0)
	if err == nil {
		t.Fatalf("reader: a header read that never completed returned nil")
	}
	logged := buf.String()
	if !strings.Contains(logged, "WARN") || !strings.Contains(logged, format.SegmentName(12)) ||
		!strings.Contains(logged, "input/output error") {
		t.Errorf("reader: the failed read was not warned of with its file and error; log holds %q", logged)
	}
}

// TestReadFormat_ExpectZeroNamesTheAgreeingIdentity reads a directory whose
// format file no read completes and whose segments name one journal: with
// expect zero the refusal still names that journal — the one identity every
// verifying header agrees on — and writes nothing. With no header
// verifying, and with two headers naming different journals, the answer
// names no identity at all: it never guesses from headers that do not
// agree. A caller that named an identity gets a refusal naming none, its
// own already known to it — agreeing headers are read only for the caller
// that expected no journal in particular.
func TestReadFormat_ExpectZeroNamesTheAgreeingIdentity(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(3, journalID)
	r.writeSeg(9, journalID)
	before := map[string][]byte{}
	for _, name := range r.listing() {
		before[name] = r.fileBytes(name)
	}
	r.failReads(format.FormatFileName, 0)

	_, err := ReadFormat(context.Background(), r.runDeps(), core.JournalID{})
	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("ReadFormat: a format file no read completed got err %v, want an UnreadableRangesError", err)
	}
	if !ure.Format {
		t.Errorf("ReadFormat: the refusal's Format flag is not set; the format file's read alone failed")
	}
	if ure.Identity != journalID {
		t.Errorf("ReadFormat: the refusal names identity %x, want %x: every verifying header in the directory agrees on it", ure.Identity, journalID)
	}
	for _, name := range r.listing() {
		if !slices.Equal(before[name], r.fileBytes(name)) {
			t.Errorf("ReadFormat: the refusal changed %s", name)
		}
	}

	// The same directory, expected as the journal the headers name: the
	// refusal names no identity, the caller's own being one it knows.
	r.failReads(format.FormatFileName, 0)
	_, err = ReadFormat(context.Background(), r.runDeps(), journalID)
	if !errors.As(err, &ure) {
		t.Fatalf("ReadFormat: a format file no read completed under a named expect got err %v, want an UnreadableRangesError", err)
	}
	if ure.Identity != (core.JournalID{}) {
		t.Errorf("ReadFormat: the refusal under a named expect names identity %x, want none: an identity the caller knows is never filled in", ure.Identity)
	}

	// A directory whose headers name two journals: no identity agrees, and
	// the refusal names none rather than one of them.
	other := core.JournalID{'d', 'i', 's', 'a', 'g', 'r', 'e', 'e'}
	r.writeSeg(21, other)
	r.failReads(format.FormatFileName, 0)

	_, err = ReadFormat(context.Background(), r.runDeps(), core.JournalID{})
	if !errors.As(err, &ure) {
		t.Fatalf("ReadFormat: a format file no read completed got err %v, want an UnreadableRangesError", err)
	}
	if ure.Identity != (core.JournalID{}) {
		t.Errorf("ReadFormat: the refusal names identity %x, want none: two headers in the directory name different journals", ure.Identity)
	}

	// A directory whose only segment holds no header to verify: nothing
	// agrees, the refusal names none.
	q := newRig(t)
	q.writeFormat()
	q.write(format.SegmentName(4), nil)
	q.failReads(format.FormatFileName, 0)
	_, err = ReadFormat(context.Background(), q.runDeps(), core.JournalID{})
	if !errors.As(err, &ure) {
		t.Fatalf("ReadFormat: a format file no read completed got err %v, want an UnreadableRangesError", err)
	}
	if ure.Identity != (core.JournalID{}) {
		t.Errorf("ReadFormat: the refusal names identity %x, want none: no verifying header is in the directory", ure.Identity)
	}
}

// rewrite replaces the file name's whole content with bytes b through the
// seam, as a whole-file restamp would have left it.
func (r *rig) rewrite(name string, b []byte) {
	r.t.Helper()
	if err := r.seam.Remove(name); err != nil {
		r.t.Fatalf("fixture: remove %s: %v", name, err)
	}
	r.write(name, b)
}

// spanKey is the value the recording tracer of this file carries in the
// context, naming the span the ctx it returns belongs to.
type spanKey struct{}

// openedSpan is one span the recording tracer started: its op, the op of
// the span the ctx it was started under carried — its parent, an empty op
// for the root — and the error it ended with.
type openedSpan struct {
	op, parent core.SpanOp
	err        error
}

// recordingTracer returns a tracer that records each span's op, its parent
// (the op carried in the ctx it was started under, so phase spans must be
// children of open for them to say so) and the error each end was called
// with, in order, under mu.
func recordingTracer(mu *sync.Mutex) (core.Tracer, func() []openedSpan) {
	mu.Lock()
	spans := []openedSpan{}
	mu.Unlock()
	tracer := func(ctx context.Context, op core.SpanOp, _ ...slog.Attr) (context.Context, func(error)) {
		parent, _ := ctx.Value(spanKey{}).(core.SpanOp)
		mu.Lock()
		spans = append(spans, openedSpan{op: op, parent: parent})
		i := len(spans) - 1
		mu.Unlock()
		return context.WithValue(ctx, spanKey{}, op), func(err error) {
			mu.Lock()
			spans[i].err = err
			mu.Unlock()
		}
	}
	return tracer, func() []openedSpan {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(spans)
	}
}

// TestRun_ThePhasesAndTheirSpans pins the phase list's order and the span
// tree it opens: one open span, one child per phase under it, marks twice,
// each child's parent the open span, and each ended with the error its own
// work returned — nil while the phase is a no-op. A phase list reordered,
// a check dropped or a phase run outside the open span's context is what
// this names, so the phases that land later do not move or detach the ones
// around them.
func TestRun_ThePhasesAndTheirSpans(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(7, journalID)

	var mu sync.Mutex
	tracer, spansOf := recordingTracer(&mu)

	st, err := Run(context.Background(), r.runDepsWithTracer(tracer), core.Version{}, journalID, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []core.SpanOp{
		core.SpanOpen,
		core.SpanOpenAttach, core.SpanOpenRebuild, core.SpanOpenMarksRaise, core.SpanOpenTailProof, core.SpanOpenMarksLostTail,
		core.SpanOpenSpares, core.SpanOpenReappend, core.SpanOpenAssembly, core.SpanOpenSeal, core.SpanOpenEmptying,
	}
	got := spansOf()
	ops := make([]core.SpanOp, len(got))
	for i, sp := range got {
		ops[i] = sp.op
	}
	if !slices.Equal(ops, want) {
		t.Errorf("Run: spans opened %v, want %v", ops, want)
	}
	for _, sp := range got {
		if sp.op != core.SpanOpen && sp.parent != core.SpanOpen {
			t.Errorf("Run: the %s span's parent is %q, want the open span: a phase run outside the open span's context", sp.op, sp.parent)
		}
		if sp.err != nil {
			t.Errorf("Run: span %s ended with %v, want nil for the no-op it covers", sp.op, sp.err)
		}
	}
	if want := []core.SegmentID{7}; !slices.Equal(ids(st.Attached.Own), want) {
		t.Errorf("Run: State.Attached.Own got %v, want %v", ids(st.Attached.Own), want)
	}
}

// TestRun_TheSpansEndWithTheirErrors pins that a refusal ends the open
// span with the error that failed the open — the attach pass itself ends
// nil, for it classed every name it read and the collected range is the
// refusal the whole run answers — and that a run cancelled as the open
// span starts ends that span with the context's error, never a span ended
// nil over failed work.
func TestRun_TheSpansEndWithTheirErrors(t *testing.T) {
	var mu sync.Mutex
	tracer, spansOf := recordingTracer(&mu)

	r := newRig(t)
	r.writeFormat()
	r.writeSeg(12, journalID)
	r.failReads(format.SegmentName(12), 0)

	_, err := Run(context.Background(), r.runDepsWithTracer(tracer), core.Version{}, journalID, nil)
	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("Run: a header read that never completed got err %v, want an UnreadableRangesError", err)
	}
	got := spansOf()
	for _, sp := range got {
		wantErr := error(nil)
		if sp.op == core.SpanOpen {
			wantErr = err // the refusal the whole run answers
		}
		if sp.err != wantErr {
			t.Errorf("Run: the %s span ended with %v, want %v", sp.op, sp.err, wantErr)
		}
	}

	// A run cancelled as the open span starts: no phase has run yet, the
	// context check before the first phase answers the cancellation, and
	// the open span — started before the cancellation — is the one span
	// recorded, ended with the context's error.
	mu2 := sync.Mutex{}
	tracer2, spans2Of := recordingTracer(&mu2)
	r2 := newRig(t)
	r2.writeFormat()
	r2.writeSeg(3, journalID)
	ctx, cancel := context.WithCancel(context.Background())
	rec := func(c context.Context, op core.SpanOp, a ...slog.Attr) (context.Context, func(error)) {
		if op == core.SpanOpen {
			cancel()
		}
		return tracer2(c, op, a...)
	}
	_, cerr := Run(ctx, r2.runDepsWithTracer(rec), core.Version{}, journalID, nil)
	if !errors.Is(cerr, context.Canceled) {
		t.Fatalf("Run: a run cancelled as the open span started got err %v, want context.Canceled", cerr)
	}
	spans2 := spans2Of()
	if len(spans2) != 1 {
		t.Fatalf("Run: a run cancelled as the open span started recorded %d spans, want 1: the open's alone", len(spans2))
	}
	if spans2[0].op != core.SpanOpen {
		t.Errorf("Run: the one span of a run cancelled at the open span is %s, want the open's", spans2[0].op)
	}
	if !errors.Is(spans2[0].err, context.Canceled) {
		t.Errorf("Run: the open span ended with %v, want context.Canceled", spans2[0].err)
	}
}

// TestRun_CancelledContextAnswersTheContextsError cancels the context Run
// runs under before any phase starts: the context's error answers, and the
// directory is untouched — the segment and the spare temporary both keep
// their names, since even the .spare.tmp unlinks belong to the attach pass
// no phase of which has run.
func TestRun_CancelledContextAnswersTheContextsError(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(3, journalID)
	r.write(format.SpareName(9), nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Run(ctx, r.runDeps(), core.Version{}, journalID, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run: a cancelled context got err %v, want context.Canceled", err)
	}
	if !slices.Contains(r.listing(), format.SpareName(9)) {
		t.Errorf("Run: the .spare.tmp is gone: no phase ran on a context cancelled before them; directory holds %v", r.listing())
	}
}

// TestRun_CancelledBetweenPhasesKeepsOnlySpareUnlinks cancels the run at
// each phase boundary — the moment a phase's span opens is the boundary the
// next phase's context check is the first thing after — and the run must
// answer the context's error there, leaving the directory as the leg began
// it but for the .spare.tmp unlinks the attach pass already made before the
// cancellation landed. A phase whose boundary check is skipped, or a run that
// answers a partial state, is what this fails.
//
// The boundary into each phase is reached by cancelling when the span of the
// phase before it opens (the open's own span for the attach pass): the phase
// the span belongs to has already passed its check, so the cancellation is
// answered by the next one. A cancel under the last phase's own span is not
// a boundary — no check follows it — and the run completes; that completing
// leg is not one of these.
func TestRun_CancelledBetweenPhasesKeepsOnlySpareUnlinks(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.writeSeg(3, journalID)
	r.write("notes.txt", []byte("bytes of a file the journal did not write"))
	keep := []string{format.SegmentName(3), "notes.txt", format.FormatFileName}

	for k, into := range phases {
		// The spare's temporary name is re-made for each leg: a leg whose
		// attach pass ran whole before the cancellation landed dropped it —
		// the one change a cancelled open is allowed — and a leg cancelled
		// before the pass leaves it, so each leg starts with one to name.
		if slices.Contains(r.listing(), format.SpareName(4)) {
			if err := r.seam.Remove(format.SpareName(4)); err != nil {
				t.Fatalf("remove the stale .spare.tmp: %v", err)
			}
		}
		r.write(format.SpareName(4), nil)

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel at the k'th span the tracer opens, and only there: the
		// value is not compared, because the marks phase's two spans share
		// one op, and the first of them — not any span whose op happens to
		// match — is the boundary every leg below the second marks is
		// meant to reach. Span k+1 is phase k's own (span 0 being the
		// open's), so phase k never runs and its check below answers.
		occurrence := k + 1
		opened := 0
		tracer := func(tctx context.Context, _ core.SpanOp, _ ...slog.Attr) (context.Context, func(error)) {
			opened++
			if opened == occurrence {
				cancel()
			}
			return tctx, func(error) {}
		}
		_, err := Run(ctx, r.runDepsWithTracer(tracer), core.Version{}, journalID, nil)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run: the boundary into %s, cancelled under the phase before it, got err %v, want context.Canceled",
				into, err)
		}
		got := r.listing()
		for _, name := range keep {
			if !slices.Contains(got, name) {
				t.Errorf("Run: a run cancelled at the boundary into %s removed %s; directory holds %v",
					into, name, got)
			}
		}
		if extra := slices.DeleteFunc(slices.Clone(got), func(n string) bool {
			return slices.Contains(keep, n) || n == format.SpareName(4)
		}); len(extra) != 0 {
			t.Errorf("Run: a run cancelled at the boundary into %s changed the directory more than .spare.tmp unlinks: %v",
				into, extra)
		}
	}
}

// TestReadFormat_UnreadableFormatRefusesByFlag makes every read of a good
// format file fail with EIO: the answer is an UnreadableRangesError whose
// Format is set and whose Identity is zero — no read named one — and the
// file is unchanged on disk, read three times and never judged by a byte it
// did not return.
func TestReadFormat_UnreadableFormatRefusesByFlag(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	before := r.fileBytes(format.FormatFileName)
	r.failReads(format.FormatFileName, 0)

	_, err := ReadFormat(context.Background(), r.runDeps(), core.JournalID{})
	var ure *core.UnreadableRangesError
	if !errors.As(err, &ure) {
		t.Fatalf("ReadFormat: a format file no read completed got err %v, want an UnreadableRangesError", err)
	}
	if !ure.Format {
		t.Errorf("ReadFormat: the refusal's Format flag is not set; only the format file's read failed")
	}
	if len(ure.Ranges) != 0 {
		t.Errorf("ReadFormat: the refusal names segment ranges %v, want none", ure.Ranges)
	}
	if ure.Identity != (core.JournalID{}) {
		t.Errorf("ReadFormat: the refusal names identity %x, want none: no read named one", ure.Identity)
	}
	if calls := r.seam.CallsFor(kit.OpRead, format.FormatFileName); calls != 3 {
		t.Errorf("ReadFormat: format reads got %d, want 3, the readretry bound", calls)
	}
	if after := r.fileBytes(format.FormatFileName); !slices.Equal(before, after) {
		t.Errorf("ReadFormat: the refusal changed the format file: got %x, want the bytes it held", after)
	}
}

// TestReadFormat_CancelledContextAnswersTheContextsError cancels the format
// read at its second attempt: the answer is the context's error, never the
// damaged verdict a decode of the bytes no read returned would give — a
// cancelled open is answered at its deadline by the caller's own error, not
// reinterpreted as corruption.
func TestReadFormat_CancelledContextAnswersTheContextsError(t *testing.T) {
	r := newRig(t)
	r.writeFormat()
	r.failReads(format.FormatFileName, 1) // the first read attempt faults

	ctx, cancel := context.WithCancel(context.Background())
	held := r.seam.HoldFirst(t, kit.OpRead, format.FormatFileName)
	done := make(chan error, 1)
	go func() {
		_, err := ReadFormat(ctx, r.runDeps(), core.JournalID{})
		done <- err
	}()

	<-held.Reached() // the second read attempt is open and held under it
	cancel()
	held.Release()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("ReadFormat: a read the cancellation cut got err %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("ReadFormat: a read the cancellation cut did not return")
	}
	if calls := r.seam.CallsFor(kit.OpRead, format.FormatFileName); calls != 1 {
		t.Errorf("ReadFormat: the cancelled read made %d attempts, want the one the fault ended", calls)
	}
}

// TestReadFormat_DecodesTheFileAndPassesDamageThrough reads a good format
// file, then each of its refusals: a 55-byte file, a 57-byte one, a flipped
// checksum byte and a version the binary does not read each fail with the
// decoder's own error, matched as damaged or unknown through the sentinel,
// never reinterpreted by the read's.
func TestReadFormat_DecodesTheFileAndPassesDamageThrough(t *testing.T) {
	cases := []struct {
		name string
		edit func(b []byte) []byte
		want error
	}{
		{"55 bytes", func(b []byte) []byte { return b[:55] }, format.ErrDamaged},
		{"57 bytes", func(b []byte) []byte { return append(append([]byte{}, b...), 0) }, format.ErrDamaged},
		{"a bad checksum", func(b []byte) []byte {
			b[52] ^= 1
			return b
		}, format.ErrDamaged},
		{"version 2", func(b []byte) []byte {
			return raw.Format{Version: 2, Identity: core.JournalID(b[8:24]), Installation: core.InstallationID(b[24:40])}.Bytes()
		}, format.ErrUnknownVersion},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.writeFormat()
			r.rewrite(format.FormatFileName, c.edit(r.fileBytes(format.FormatFileName)))

			_, err := ReadFormat(context.Background(), r.runDeps(), core.JournalID{})
			if !errors.Is(err, c.want) {
				t.Errorf("ReadFormat: a format file of %s got err %v, want one matching %v", c.name, err, c.want)
			}
		})
	}
}

// TestReadFormat_DecodesAWholeFile reads back the identity and installation
// of the file the rig wrote, and fails on a header the decode did not reach.
func TestReadFormat_DecodesAWholeFile(t *testing.T) {
	r := newRig(t)
	r.writeFormat()

	ff, err := ReadFormat(context.Background(), r.runDeps(), core.JournalID{})
	if err != nil {
		t.Fatalf("ReadFormat: %v", err)
	}
	if ff.Identity != journalID || ff.Installation != install {
		t.Errorf("ReadFormat: got identity %x installation %x, want %x %x",
			ff.Identity, ff.Installation, journalID, install)
	}
}
