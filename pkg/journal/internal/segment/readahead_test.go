package segment_test

import (
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/journal/internal/storage"
)

// hintFS wraps a filesystem so its files take the read-ahead hint: each
// ReadAhead is recorded and passed to the wrapped file when it takes one,
// and answered with fail instead when fail is set. When block is set, a hint
// first reports on entered and then waits for block to close.
type hintFS struct {
	storage.FS
	fail    error
	entered chan struct{}
	block   chan struct{}

	mu    sync.Mutex
	hints []hintCall
}

type hintCall struct {
	name    string
	off, to int64
}

type hintFile struct {
	storage.File
	fs *hintFS
}

func (f *hintFile) ReadAhead(off, n int64) error {
	f.fs.mu.Lock()
	f.fs.hints = append(f.fs.hints, hintCall{name: f.Name(), off: off, to: off + n})
	f.fs.mu.Unlock()
	if f.fs.block != nil {
		f.fs.entered <- struct{}{}
		<-f.fs.block
	}
	if f.fs.fail != nil {
		return f.fs.fail
	}
	if ra, ok := f.File.(storage.ReadAheader); ok {
		return ra.ReadAhead(off, n)
	}
	return nil
}

func (fs *hintFS) Open(name string) (storage.File, error) {
	f, err := fs.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &hintFile{File: f, fs: fs}, nil
}

// calls returns the hints recorded so far for the segment file of id.
func (fs *hintFS) calls(id core.SegmentID) []hintCall {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []hintCall
	for _, c := range fs.hints {
		if c.name == format.SegmentName(id) {
			out = append(out, c)
		}
	}
	return out
}

// hintEnv is one stream over a hintFS, with the read-ahead worker installed
// and started. Its cleanups stop the worker before the cache's Shutdown, the
// order the journal's close keeps, so a hint holding a reference past the
// stop fails the Shutdown's check.
type hintEnv struct {
	fs *hintFS
	e  streamEnv
	s  *segment.Stream
	h  *segment.Hinter
}

func hintBuild(t *testing.T, fs *hintFS, window int64) hintEnv {
	t.Helper()

	fs.FS = newSeam(t).FS()
	e := streamOverFS(t, streamCap, fs, func(streamEnv) {}).built(t)
	ss := segment.NewStreams(1, streamIdentity, e.set, e.pool, e.cache, nopSinkFor)
	t.Cleanup(func() { _ = ss.Close() })
	h := segment.NewHinter(ss, window, nil)
	h.Start()
	t.Cleanup(h.Stop)
	return hintEnv{fs: fs, e: e, s: ss.Stream(0), h: h}
}

// appendRun appends n 1 KiB writes and returns where each landed.
func (he hintEnv) appendRun(t *testing.T, first, n int) []segment.Location {
	t.Helper()

	var locs []segment.Location
	for i := first; i < first+n; i++ {
		h, body := streamWriteHeader(streamFileID(i), int64(i)<<10, streamTestBody(1<<10, byte(i)), 7, core.Version{Counter: uint64(i)})
		p, err := he.s.Append(h, body)
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		locs = append(locs, p.Loc)
	}
	return locs
}

// TestHinterHintsTheActiveSegmentAheadOfItsAppends checks the ranges the
// read-ahead worker asks for: per segment they run contiguously from the end
// of the header, never past the cap, and reach past every record the segment
// took. A window at or above the cap asks for the whole segment in one hint
// at the roll; a smaller one asks again as the appends advance.
func TestHinterHintsTheActiveSegmentAheadOfItsAppends(t *testing.T) {
	for _, c := range []struct {
		name   string
		window int64
		whole  bool
	}{
		{"window below the cap", 16 << 10, false},
		{"window above the cap", 2 * streamCap, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			prof.Watch(t, prof.Options{})

			he := hintBuild(t, &hintFS{}, c.window)
			// About 160 KiB of records: two full segments and part of a third.
			locs := he.appendRun(t, 1, 150)

			ends := map[core.SegmentID]int64{}
			for _, l := range locs {
				ends[l.Segment] = max(ends[l.Segment], l.Off+l.Len)
			}
			if len(ends) < 3 {
				t.Fatalf("the appends landed in %d segments, want at least 3 so the roll is crossed", len(ends))
			}

			reissued := false
			for id, end := range ends {
				// The worker runs on its own goroutine: wait for the front
				// to pass the segment's last record, with one backstop.
				deadline := time.Now().Add(10 * time.Second)
				var calls []hintCall
				for {
					calls = he.fs.calls(id)
					if len(calls) > 0 && calls[len(calls)-1].to >= end || time.Now().After(deadline) {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if len(calls) == 0 {
					t.Fatalf("segment %d: no hint, want one from the header", id)
				}
				front := int64(format.SegmentHeaderSize)
				for i, h := range calls {
					if h.off != front {
						t.Fatalf("segment %d hint %d starts at %d, want %d: the hints are not contiguous from the header", id, i, h.off, front)
					}
					if h.to <= h.off || h.to > streamCap {
						t.Fatalf("segment %d hint %d is [%d, %d), want a non-empty range within the cap %d", id, i, h.off, h.to, streamCap)
					}
					front = h.to
				}
				if front < end {
					t.Fatalf("segment %d: hinted through %d, want at least its last record's end %d", id, front, end)
				}
				if c.whole && (len(calls) != 1 || front != streamCap) {
					t.Fatalf("segment %d: got hints %+v, want one over [%d, %d)", id, calls, format.SegmentHeaderSize, streamCap)
				}
				reissued = reissued || len(calls) > 1
			}
			if !c.whole && !reissued {
				t.Fatalf("no segment was hinted more than once, want the window re-asked as the appends advance")
			}
		})
	}
}

// TestHinterFailingHintChangesNothing checks that a hint the platform refuses
// changes no answer of the stream: the same appends across two rolls land at
// the same locations, and the Sync and the seal after them answer as they do
// when every hint succeeds.
func TestHinterFailingHintChangesNothing(t *testing.T) {
	prof.Watch(t, prof.Options{})

	type outcome struct {
		locs []segment.Location
		seal segment.SealResult
	}
	run := func(fail error) (outcome, *hintFS) {
		he := hintBuild(t, &hintFS{fail: fail}, 16<<10)
		locs := he.appendRun(t, 1, 150)
		if err := he.s.Sync(); err != nil {
			t.Fatalf("Sync with hints failing with %v: %v", fail, err)
		}
		seal, err := he.s.Seal(false)
		if err != nil {
			t.Fatalf("Seal with hints failing with %v: %v", fail, err)
		}
		return outcome{locs: locs, seal: seal}, he.fs
	}

	want, _ := run(nil)
	got, fs := run(syscall.EIO)
	if !slices.Equal(got.locs, want.locs) {
		t.Fatalf("locations with every hint failing: got %v, want %v", got.locs, want.locs)
	}
	if got.seal != want.seal {
		t.Fatalf("seal with every hint failing: got %+v, want %+v", got.seal, want.seal)
	}
	if len(fs.calls(got.locs[0].Segment)) == 0 {
		t.Fatalf("no hint reached the first segment, so no failure was injected")
	}
}

// TestHinterStopWaitsForTheHintInFlight checks that Stop joins a hint the
// platform is still running, so no hint holds its descriptor reference past
// the journal's close, and that a stream appending after Stop never blocks on
// the stopped worker.
func TestHinterStopWaitsForTheHintInFlight(t *testing.T) {
	prof.Watch(t, prof.Options{})

	fs := &hintFS{entered: make(chan struct{}, 1), block: make(chan struct{})}
	he := hintBuild(t, fs, 16<<10)
	// Registered after the build's cleanups so it runs first: a failure below
	// releases the blocked hint, and the cache's Shutdown then fails or passes
	// on its own rather than waiting on it forever.
	var release sync.Once
	unblock := func() { release.Do(func() { close(fs.block) }) }
	t.Cleanup(unblock)
	he.appendRun(t, 1, 1)

	select {
	case <-fs.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("the roll's hint never reached the file")
	}
	stopped := make(chan struct{})
	go func() {
		he.h.Stop()
		close(stopped)
	}()
	// No clock reaches the worker, so a bounded wait is the assertion that
	// Stop does not return while the hint runs.
	select {
	case <-stopped:
		t.Fatalf("Stop returned while a hint was in flight, want it to wait for the hint")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatalf("Stop did not return once the hint finished")
	}

	// Past several window steps and a roll, with no worker to take a wake.
	he.appendRun(t, 2, 100)
	if err := he.s.Sync(); err != nil {
		t.Fatalf("Sync after Stop: %v", err)
	}
}
