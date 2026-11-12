package unread

import (
	"context"
	"errors"
	"io"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// recordFile is the file the Try bits read ref's bytes from: one name, so
// every fault an attempt meets is its OpRead.
const recordFile = "record"

// rig is one journal directory on the real filesystem, reached through a
// crash-simulating seam whose schedule injects the read faults, as the
// fixtures of internal/recovery's attach pass do: the bytes the Try bits
// read are real bytes behind a faulted device, not a stub answering in its
// place.
type rig struct {
	t    *testing.T
	seam *kit.Seam
	clk  kit.DrivenClock
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	seam, err := kit.NewSeam(dir)
	if err != nil {
		t.Fatalf("fixture: new seam: %v", err)
	}
	t.Cleanup(seam.Abandon)
	return &rig{t: t, seam: seam, clk: kit.NewStopwatchClock()}
}

// write makes the file name from whole bytes, as a synced writer would have
// left it.
func (r *rig) write(name string, b []byte) {
	r.t.Helper()
	f, err := r.seam.Create(name)
	if err != nil {
		r.t.Fatalf("fixture: create %s: %v", name, err)
	}
	if len(b) > 0 {
		if _, err := f.WriteAt(b, 0); err != nil {
			r.t.Fatalf("fixture: write %s: %v", name, err)
		}
		if err := f.Sync(); err != nil {
			r.t.Fatalf("fixture: sync %s: %v", name, err)
		}
	}
	if err := f.Close(); err != nil {
		r.t.Fatalf("fixture: close %s: %v", name, err)
	}
	if err := r.seam.SyncDir(); err != nil {
		r.t.Fatalf("fixture: sync dir: %v", err)
	}
}

// failReads makes reads of name fail count times with EIO, then pass; zero
// fails every read.
func (r *rig) failReads(name string, count int) {
	r.t.Helper()
	if err := r.seam.Schedule(kit.Failing(kit.OpRead, name, count, syscall.EIO)); err != nil {
		r.t.Fatalf("fixture: schedule: %v", err)
	}
}

// clearFaults takes the fault off, so a later read passes.
func (r *rig) clearFaults() {
	r.t.Helper()
	r.seam.SetFault(nil)
}

// read is the read closure a Try of ref takes: ref's bytes of name, opened
// and read through the seam on every attempt, as the journal's storage does.
// A read ending at the file's end is a completed read, not a fault; failure
// returns no bytes, as a failed device read does.
func (r *rig) read(ref Ref, name string) func() ([]byte, error) {
	return func() ([]byte, error) {
		f, err := r.seam.Open(name)
		if err != nil {
			return nil, err
		}
		// The close's own error is dropped: the read this attempt exists
		// for has already finished.
		defer func() { _ = f.Close() }()
		b := make([]byte, ref.Len)
		n, err := f.ReadAt(b, ref.Off)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return b[:n], nil
	}
}

// TestTryTransientErrorsReturnBytes checks that EIO twice then success
// returns the bytes with nothing marked.
func TestTryTransientErrorsReturnBytes(t *testing.T) {
	r := newRig(t)
	r.write(recordFile, []byte("record bytes"))
	r.failReads(recordFile, 2)
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 0, Len: 12}, Kind: Content}
	s := New(r.clk)

	b, err := s.Try(context.Background(), ref, r.read(ref, recordFile))

	if err != nil {
		t.Fatalf("Try: EIO twice then a third read returned err %v, want nil", err)
	}
	if string(b) != "record bytes" {
		t.Errorf("Try: bytes got %q, want %q", b, "record bytes")
	}
	if got := s.Unread(ref); got {
		t.Errorf("Unread(ref) after a read that succeeded = %v, want false", got)
	}
	if got := s.SuspectSegments(); got != 0 {
		t.Errorf("SuspectSegments() after a read that succeeded = %d, want 0", got)
	}
}

// TestTryExhaustionMarks checks that every attempt failing makes exactly
// three attempts, marks the ref and errors with both the unreadable verdict
// and the device fault.
func TestTryExhaustionMarks(t *testing.T) {
	r := newRig(t)
	r.write(recordFile, []byte("record bytes"))
	r.failReads(recordFile, 0) // every read
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 0, Len: 12}, Kind: Content}
	s := New(r.clk)

	b, err := s.Try(context.Background(), ref, r.read(ref, recordFile))

	if b != nil {
		t.Errorf("Try: a read no attempt completed got bytes %q, want nil", b)
	}
	if !errors.Is(err, core.ErrUnreadable) {
		t.Errorf("Try: a read no attempt completed got err %v, want one matching ErrUnreadable", err)
	}
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("Try: the exhaustion err %v does not match EIO, want the device fault named", err)
	}
	if calls := r.seam.CallsFor(kit.OpRead, recordFile); calls != 3 {
		t.Errorf("Try: read attempts got %d, want 3, the readretry bound", calls)
	}
	if got := s.Unread(ref); !got {
		t.Errorf("Unread(ref) after a read no attempt completed = %v, want true", got)
	}
	if got := s.Suspect(ref.Segment); !got {
		t.Errorf("Suspect(%d) after a read no attempt completed = %v, want true", ref.Segment, got)
	}
	if got := s.SuspectSegments(); got != 1 {
		t.Errorf("SuspectSegments() after a read no attempt completed = %d, want 1", got)
	}
}

// TestTrySecondAccessMakesOneAttempt checks the custody rule the attempts
// divide on: a ref already unread gets one attempt per access, and a read
// that then succeeds clears it.
func TestTrySecondAccessMakesOneAttempt(t *testing.T) {
	r := newRig(t)
	r.write(recordFile, []byte("record bytes"))
	r.failReads(recordFile, 0) // every read
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 0, Len: 12}, Kind: Content}
	s := New(r.clk)

	if _, err := s.Try(context.Background(), ref, r.read(ref, recordFile)); !errors.Is(err, core.ErrUnreadable) {
		t.Fatalf("Try: a read no attempt completed got err %v, want one matching ErrUnreadable", err)
	}
	if calls := r.seam.CallsFor(kit.OpRead, recordFile); calls != 3 {
		t.Fatalf("Try: the first access made %d attempts, want 3", calls)
	}

	// A ref already unread gets one attempt, not the bound's three.
	_, err := s.Try(context.Background(), ref, r.read(ref, recordFile))
	if !errors.Is(err, core.ErrUnreadable) {
		t.Errorf("Try: a second failing access got err %v, want one matching ErrUnreadable", err)
	}
	if calls := r.seam.CallsFor(kit.OpRead, recordFile); calls != 4 {
		t.Errorf("Try: attempts after exhaustion got %d more, want exactly 1 on a ref already unread", calls-3)
	}

	r.clearFaults()
	b, err := s.Try(context.Background(), ref, r.read(ref, recordFile))
	if err != nil {
		t.Fatalf("Try: a successful third access got err %v, want nil", err)
	}
	if string(b) != "record bytes" {
		t.Errorf("Try: bytes got %q, want %q", b, "record bytes")
	}
	if got := s.Unread(ref); got {
		t.Errorf("Unread(ref) after a successful third access = %v, want false", got)
	}
}

// TestTryWindowStopsRetries checks that the fifteen-second window drops a
// fresh attempt on the injected clock, so only one is made.
func TestTryWindowStopsRetries(t *testing.T) {
	r := newRig(t)
	r.write(recordFile, []byte("record bytes"))
	r.failReads(recordFile, 1) // the first attempt faults
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 0, Len: 12}, Kind: Content}
	s := New(r.clk)

	// A read that takes sixteen seconds between its first and second
	// attempt: the kernel bound the window models, not a sleeping test.
	read := r.read(ref, recordFile)
	_, err := s.Try(context.Background(), ref, func() ([]byte, error) {
		b, rerr := read()
		if rerr != nil {
			r.clk.Advance(16 * time.Second)
		}
		return b, rerr
	})

	if !errors.Is(err, core.ErrUnreadable) {
		t.Errorf("Try: a read the window cut short got err %v, want one matching ErrUnreadable", err)
	}
	if calls := r.seam.CallsFor(kit.OpRead, recordFile); calls != 1 {
		t.Errorf("Try: read attempts past the window got %d, want 1", calls)
	}
	if got := s.Unread(ref); !got {
		t.Errorf("Unread(ref) after a read the window cut short = %v, want true", got)
	}
}

// TestTryContextCutAfterAFailedAttemptMarks checks that a context ending the
// retries after an attempt has failed still marks the range — its bytes were
// read by no attempt, whatever ended them — while the caller answers with
// its own cancellation, not an unreadable verdict.
func TestTryContextCutAfterAFailedAttemptMarks(t *testing.T) {
	r := newRig(t)
	r.write(recordFile, []byte("record bytes"))
	r.failReads(recordFile, 0) // every read
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 0, Len: 12}, Kind: Content}
	s := New(r.clk)

	ctx, cancel := context.WithCancel(context.Background())
	held := r.seam.HoldFirst(t, kit.OpRead, recordFile)
	done := make(chan error, 1)
	go func() {
		_, err := s.Try(ctx, ref, r.read(ref, recordFile))
		done <- err
	}()

	<-held.Reached() // the first read attempt is held under it
	cancel()
	held.Release()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Try: a read the cancellation cut got err %v, want context.Canceled", err)
		}
		if errors.Is(err, core.ErrUnreadable) {
			t.Errorf("Try: the cancellation was wrapped in ErrUnreadable (%v), want the context's error bare", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Try: a read the cancellation cut did not return")
	}
	if got := s.Unread(ref); !got {
		t.Errorf("Unread(ref) after an attempt failed and the context cut the retries = %v, want true", got)
	}
	if got := s.SuspectSegments(); got != 1 {
		t.Errorf("SuspectSegments() after an attempt failed and the context cut the retries = %d, want 1", got)
	}
}

// TestTryContextDoneBeforeAnyAttemptMarksNothing checks that a context
// already ended when Try is called reads nothing and marks nothing: no
// attempt of it ever failed.
func TestTryContextDoneBeforeAnyAttemptMarksNothing(t *testing.T) {
	r := newRig(t)
	r.write(recordFile, []byte("record bytes"))
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 0, Len: 12}, Kind: Content}
	s := New(r.clk)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Try(ctx, ref, r.read(ref, recordFile))

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Try: a context ended before any attempt got err %v, want context.Canceled", err)
	}
	if calls := r.seam.CallsFor(kit.OpRead, recordFile); calls != 0 {
		t.Errorf("Try: a context ended before any attempt made %d reads, want 0", calls)
	}
	if got := s.Unread(ref); got {
		t.Errorf("Unread(ref) after a context ended before any attempt = %v, want false", got)
	}
	if got := s.SuspectSegments(); got != 0 {
		t.Errorf("SuspectSegments() after a context ended before any attempt = %d, want 0", got)
	}
}

// TestTryZeroBytesNoErrorIsSuccess checks that a read of zero bytes with no
// error clears a previously marked ref.
func TestTryZeroBytesNoErrorIsSuccess(t *testing.T) {
	r := newRig(t)
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 3, Off: 0, Len: 4}, Kind: Content}
	s := New(r.clk)
	s.mark(ref)

	b, err := s.Try(context.Background(), ref, func() ([]byte, error) { return nil, nil })

	if err != nil {
		t.Fatalf("Try: a read of zero bytes with no error got err %v, want nil", err)
	}
	if b != nil {
		t.Errorf("Try: bytes got %v, want nil", b)
	}
	if got := s.Unread(ref); got {
		t.Errorf("Unread(ref) after a read that returned no bytes and no error = %v, want false", got)
	}
}

// TestTryKeepsHeaderOnlyKind checks that a HeaderOnly ref a failing Try
// marked stays HeaderOnly in the set with its length in Ranges.
func TestTryKeepsHeaderOnlyKind(t *testing.T) {
	r := newRig(t)
	r.write(recordFile, []byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEF"))
	r.failReads(recordFile, 0) // every read
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 32, Len: 8}, Kind: HeaderOnly}
	s := New(r.clk)

	_, err := s.Try(context.Background(), ref, r.read(ref, recordFile))
	if !errors.Is(err, core.ErrUnreadable) {
		t.Fatalf("Try: a read no attempt completed got err %v, want one matching ErrUnreadable", err)
	}

	if got := s.Unread(ref); !got {
		t.Errorf("Unread(ref) after a failed Try = %v, want true", got)
	}
	kind, ok := s.entry(ref)
	if !ok {
		t.Fatalf("entry(ref) after a failed Try = nothing, want the marked ref")
	}
	if kind != HeaderOnly {
		t.Errorf("entry(ref) after a failed Try = kind %v, want %v, the kind the ref named", kind, HeaderOnly)
	}
	want := []core.SegmentRange{{Segment: 7, Off: 32, Len: 8}}
	if got := s.Ranges(ref.Segment); !slices.Equal(got, want) {
		t.Errorf("Ranges(%d) after a failed Try = %v, want %v with its length", ref.Segment, got, want)
	}
}

// TestTryCancelDuringLastAttemptReturnsAttemptFailure checks the case the
// bound decides: a marked range's one permitted attempt, cancelled inside
// the read and failing it, answers with the attempt's own failure wrapped
// in ErrUnreadable — the bound spent the attempt, not the context — and
// the range stays marked.
func TestTryCancelDuringLastAttemptReturnsAttemptFailure(t *testing.T) {
	r := newRig(t)
	r.write(recordFile, []byte("record bytes"))
	r.failReads(recordFile, 0) // every read
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 0, Len: 12}, Kind: Content}
	s := New(r.clk)
	s.mark(ref) // the range is unread, so its next access gets one attempt

	ctx, cancel := context.WithCancel(context.Background())
	held := r.seam.HoldFirst(t, kit.OpRead, recordFile)
	done := make(chan error, 1)
	go func() {
		_, err := s.Try(ctx, ref, r.read(ref, recordFile))
		done <- err
	}()

	<-held.Reached() // the one permitted attempt is held under it
	cancel()         // the context ends during the attempt's own read
	held.Release()   // and the read then fails with EIO

	select {
	case err := <-done:
		if !errors.Is(err, core.ErrUnreadable) {
			t.Errorf("Try: a cancelled one-attempt read got err %v, want one matching ErrUnreadable, the attempt's failure", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("Try: a cancelled one-attempt read got %v, want the attempt's failure, not the context's", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Try: a cancelled one-attempt read did not return")
	}
	if got := s.Unread(ref); !got {
		t.Errorf("Unread(ref) after a cancelled one-attempt read = %v, want true", got)
	}
}

// TestTryForeignDeadlineExceededMarks checks that a deadline the read names
// is a device answer, not the end of Try's own retries: a live context
// whose read returns context.DeadlineExceeded does not answer Try with the
// context's error but marks the range and wraps ErrUnreadable around it.
func TestTryForeignDeadlineExceededMarks(t *testing.T) {
	s := New(kit.NewStopwatchClock())
	ref := Ref{SegmentRange: core.SegmentRange{Segment: 7, Off: 0, Len: 12}, Kind: Content}

	_, err := s.Try(context.Background(), ref, func() ([]byte, error) {
		return nil, context.DeadlineExceeded // a deadline that was not Try's
	})

	if !errors.Is(err, core.ErrUnreadable) {
		t.Errorf("Try: a read naming a foreign deadline got err %v, want one matching ErrUnreadable", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Try: the device fault was dropped from %v, want the deadline still matched", err)
	}
	if got := s.Unread(ref); !got {
		t.Errorf("Unread(ref) after a read naming a foreign deadline = %v, want true", got)
	}
	if got := s.SuspectSegments(); got != 1 {
		t.Errorf("SuspectSegments() after a read naming a foreign deadline = %d, want 1", got)
	}
}
