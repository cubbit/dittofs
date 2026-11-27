package reclaim_test

import (
	"context"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/datapath"
	"github.com/cubbit/dittofs/pkg/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// passFixture is one copy fixture whose core owns the pass under test: the
// selector, the copier and the pass are the core's own wiring over the
// fixture's planted state, so a test drives a repack exactly the way a
// journal's caller does while it keeps the copy family's planted-record
// helpers for everything that must land in a segment before one runs.
type passFixture struct {
	f     *copyFixture
	share *datapath.Share
}

// newPassFixture builds the fixture: a copy fixture whose core runs the pass,
// with no tracer.
func newPassFixture(t *testing.T) *passFixture {
	t.Helper()
	return &passFixture{f: newCopyFixture(t)}
}

// newTracedPassFixture is newPassFixture over a core whose tracer is tracer,
// for a test that reads the spans the core emits; nil traces nothing.
func newTracedPassFixture(t *testing.T, tracer core.Tracer) *passFixture {
	t.Helper()
	return &passFixture{f: newCopyFixtureTraced(t, tracer)}
}

// repack runs the core's own pass over the listed segments, or every
// candidate when none is listed.
func (p *passFixture) repack(ctx context.Context, budget int64, segments ...core.SegmentID) (int64, error) {
	return p.f.core.Repack(ctx, budget, reclaim.Scope{Segments: segments})
}

// repackResult is what one pass run answered: the pass runs in the test's
// goroutine when the test wants it to, and in its own when a test holds a
// seam while it runs.
type repackResult struct {
	freed int64
	err   error
}

// goRepack runs repack in its own goroutine, handing the test the channel its
// answer arrives on.
func (p *passFixture) goRepack(ctx context.Context, budget int64, segments ...core.SegmentID) <-chan repackResult {
	out := make(chan repackResult, 1)
	go func() {
		freed, err := p.repack(ctx, budget, segments...)
		out <- repackResult{freed: freed, err: err}
	}()
	return out
}

// awaitRepack waits for a pass's answer with the copy family's own wait as
// the backstop.
func awaitRepack(t *testing.T, done <-chan repackResult) repackResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(copyWait):
		t.Fatalf("the repack pass did not return within %v", copyWait)
		return repackResult{}
	}
}

// reachHold waits for a held seam call to arrive with the copy family's own
// wait as the backstop: a fence that never fires must fail the test, not
// hang it.
func reachHold(t *testing.T, h *kit.Hold) {
	t.Helper()
	select {
	case <-h.Reached():
	case <-time.After(copyWait):
		t.Fatalf("no storage call reached the hold within %v, want the pass's own to be the first that could", copyWait)
	}
}

// plantSealed plants one write record of payload of file and seals the
// segment it landed in: the standing shape every pass test starts from, a
// sealed source segment the pass can select at once. The pool settles behind
// the seal so the copy has a target.
func (p *passFixture) plantSealed(t *testing.T, file core.FileID, payload []byte) core.SegmentID {
	t.Helper()
	rec, _, _ := p.f.plant(t, copyTag, file, payload, 0)
	seg := rec.Segment
	p.f.sealSource(t, file)
	p.f.waitSpares(t)
	return seg
}

// retainActivity plants one further write of file's stream in a segment of
// its own, so the stream holds an active segment with room beside the sealed
// source: a pass's copies then append into it without rolling, and the pool
// makes nothing behind the pass — the shape a test that holds a seam at a
// sync needs, so the call it stops can only be the pass's own. The pool is
// waited to its settle before the plant — the roll draws the stream's next
// segment from the pool's own spare, which must stand ready — and settles
// again behind the roll before the test returns.
func (p *passFixture) retainActivity(t *testing.T, stream int, file core.FileID) {
	t.Helper()
	other := passFileOnStream(p.f, stream, file)
	p.f.waitSpares(t)
	p.f.plant(t, copyTag, other, []byte("the write that keeps the stream active beyond the sealed source"), 0)
	p.f.waitSpares(t)
}

// passFileOnStream returns a file that appends on stream, and is not file.
func passFileOnStream(f *copyFixture, stream int, file core.FileID) core.FileID {
	for b := byte(1); b != 0; b++ {
		other := copyFile(b)
		if other != file && f.core.Streams().IndexFor(other) == stream {
			return other
		}
	}
	panic("reclaim: the test found no second file of the stream")
}

// passFilesOnDistinctStreams returns two files that append on different
// streams, so two planted writes land in two segments the pass treats
// independently.
func passFilesOnDistinctStreams(f *copyFixture) (core.FileID, core.FileID) {
	for a := byte(1); a != 0; a++ {
		for b := byte(a + 1); b != 0; b++ {
			fa, fb := copyFile(a), copyFile(b)
			if f.core.Streams().IndexFor(fa) != f.core.Streams().IndexFor(fb) {
				return fa, fb
			}
		}
	}
	panic("reclaim: the test found no two files on distinct streams")
}

// freedGain returns how much the retirer has freed since the snapshot was
// taken.
func (p *passFixture) freedGain(before uint64) uint64 {
	return p.f.core.Stats().Counters.FreedBytes - before
}

// setGone reports whether the segment left the set — what a retired source
// answers and an untouched one never does.
func (p *passFixture) setGone(seg core.SegmentID) bool {
	_, err := p.f.set.State(seg)
	return err != nil
}

// theShare returns the fixture's one share handle, attaching it on first
// use: the handle the pass's tests write and read through, closed once when
// the test ends.
func (p *passFixture) theShare(t *testing.T) *datapath.Share {
	t.Helper()
	if p.share == nil {
		p.share = p.f.share(t, copyTag)
	}
	return p.share
}

// readAll reads whole the extent [off, off+len(p)) of file through the file
// handle the writer's own reads take, and returns the bytes it produced.
func (p *passFixture) readAll(t *testing.T, file core.FileID, off int64, n int64) []byte {
	t.Helper()
	buf := make([]byte, n)
	read, missing, _, _, err := p.f.core.ReadAt(t.Context(), p.theShare(t), file, off, buf)
	if err != nil {
		t.Fatalf("ReadAt of file %v at %d: %v", file, off, err)
	}
	if int64(read) != n {
		t.Fatalf("ReadAt of file %v at %d read %d of %d bytes, missing %+v", file, off, read, n, missing)
	}
	if len(missing) != 0 {
		t.Fatalf("ReadAt of file %v at %d reported %+v missing, want the whole extent after the pass", file, off, missing)
	}
	return buf
}
