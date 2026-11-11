package datapath

import (
	"context"
	"fmt"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
)

// withRecorder is the newTestCore option that installs the lock-order recorder
// on every lock the core wires.
func withRecorder(rec *lockorder.Recorder) func(*coreOptions) {
	return func(o *coreOptions) { o.recorder = rec }
}

// recFS is a pass-through wrapper over a real storage.FS: it tells the
// recorder about every call before delegating it, answering nothing itself
// nor standing in the real filesystem's place. The core holds no file index
// lock across its I/O, so the calls the recorder sees here are what reports
// that order broke, end to end.
type recFS struct {
	rec *lockorder.Recorder
	fs  storage.FS
}

// Create wraps the file the real FS returns, so the calls on it are told too.
func (r recFS) Create(name string) (storage.File, error) {
	r.rec.IO("create")
	f, err := r.fs.Create(name)
	if err != nil {
		return nil, err
	}
	return recFile{rec: r.rec, f: f}, nil
}

// Open wraps the file the real FS returns, so the calls on it are told too.
func (r recFS) Open(name string) (storage.File, error) {
	r.rec.IO("open")
	f, err := r.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return recFile{rec: r.rec, f: f}, nil
}

func (r recFS) Rename(oldName, newName string) error {
	r.rec.IO("rename")
	return r.fs.Rename(oldName, newName)
}

func (r recFS) Remove(name string) error {
	r.rec.IO("remove")
	return r.fs.Remove(name)
}

func (r recFS) List() ([]string, error) {
	r.rec.IO("list")
	return r.fs.List()
}

func (r recFS) SyncDir() error {
	r.rec.IO("syncdir")
	return r.fs.SyncDir()
}

// recFile is recFS's pass-through over one open file.
type recFile struct {
	rec *lockorder.Recorder
	f   storage.File
}

func (f recFile) ReadAt(p []byte, off int64) (n int, err error) {
	f.rec.IO("readat")
	return f.f.ReadAt(p, off)
}

func (f recFile) ReadAtDirect(p []byte, off int64) (n int, err error) {
	f.rec.IO("readatdirect")
	return f.f.ReadAtDirect(p, off)
}

func (f recFile) WriteAt(p []byte, off int64) (n int, err error) {
	f.rec.IO("writeat")
	return f.f.WriteAt(p, off)
}

func (f recFile) Allocate(off, n int64) error {
	f.rec.IO("allocate")
	return f.f.Allocate(off, n)
}

func (f recFile) Sync() error {
	f.rec.IO("sync")
	return f.f.Sync()
}

func (f recFile) Truncate(size int64) error {
	f.rec.IO("truncate")
	return f.f.Truncate(size)
}

func (f recFile) Size() (int64, error) {
	f.rec.IO("size")
	return f.f.Size()
}

func (f recFile) Close() error {
	f.rec.IO("close")
	return f.f.Close()
}

// Name is the file's own metadata; the FS hands it out with no disk call.
func (f recFile) Name() string { return f.f.Name() }

// newRecordedCore builds a core over a real temp directory with the recorder
// installed on its index, segments, admission and file append locks, two
// streams and two shares, and publishes one file through each share so the
// helper's files sit on both streams before the test starts. Every file call
// the core makes passes the recorder through recFS.
//
// The counts are the fixture's own: a core that comes back with another
// shape of streams or shares fails here, not in whatever test built on it.
func newRecordedCore(t *testing.T) (*Core, *lockorder.Recorder, []*Share) {
	t.Helper()
	return newRecordedCoreOpts(t)
}

// newRecordedCoreOpts is newRecordedCore with further test options applied,
// so a test can park a call at one of the core's seams and still have the
// recorder and its FS wrapper wired. An option that hooks an operation the
// fixture's own publishes run — the afterAppend seam among them — must arm
// itself for the test's own call alone, for the fixture writes before the
// test starts.
func newRecordedCoreOpts(t *testing.T, opts ...func(*coreOptions)) (*Core, *lockorder.Recorder, []*Share) {
	t.Helper()
	rec := lockorder.NewRecorder()
	c, _ := newTestCore(t, append([]func(*coreOptions){
		withRecorder(rec),
		func(o *coreOptions) { o.wrapFS = func(fs storage.FS) storage.FS { return recFS{rec: rec, fs: fs} } },
	}, opts...)...)
	shares := []*Share{mustShare(t, c, 1), mustShare(t, c, 2)}
	if got := c.Streams().Len(); got != testStreams {
		t.Fatalf("the fixture's streams = %d, want %d", got, testStreams)
	}
	f := fid(1)
	files := []core.FileID{f, fileOnOtherStream(c, f)}
	if c.streams.IndexFor(files[0]) == c.streams.IndexFor(files[1]) {
		t.Fatalf("the fixture's files sit on one stream = %d, want streams 0 and 1",
			c.streams.IndexFor(files[0]))
	}
	for i, s := range shares {
		if _, err := c.WriteAt(context.Background(), s, files[i], 0, []byte("fixture"), 1); err != nil {
			t.Fatalf("WriteAt on the fixture's file %d through share %d: %v", i+1, s.Tag, err)
		}
	}
	return c, rec, shares
}

// fileOnOtherStream picks a file id the core's streams place on a stream
// other than f's.
func fileOnOtherStream(c *Core, f core.FileID) core.FileID {
	fid0 := c.streams.IndexFor(f)
	for n := uint64(1); ; n++ {
		id := fid(n)
		if c.streams.IndexFor(id) != fid0 {
			return id
		}
	}
}

// streamFileID picks a fresh file id the core's streams place on stream n,
// beyond the ids the fixture published: it searches from a base no fixture
// file uses, so the id names a file no entry holds yet.
func streamFileID(c *Core, n int) core.FileID {
	for k := uint64(1000); ; k++ {
		if id := fid(k); c.streams.IndexFor(id) == n {
			return id
		}
	}
}

// TestRecordedCoreFixture proves the recorder's wiring end to end: the
// fixture's own publishes record no violation, and a probe that holds a rank
// above the ones the core's layers take is reported by each of them, so a
// dropped Init or a dropped wiring fails here rather than everywhere.
func TestRecordedCoreFixture(t *testing.T) {
	c, rec, shares := newRecordedCore(t)

	// A publish to one fresh file from each share.
	for i, s := range shares {
		if _, err := c.WriteAt(context.Background(), s, fid(uint64(40+i)), 0, []byte("fixture"), 1); err != nil {
			t.Fatalf("WriteAt through share %d: %v", s.Tag, err)
		}
	}
	if got := rec.Violations(); len(got) != 0 {
		t.Fatalf("recorder violations after the publishes: %+v", got)
	}
	if got := rec.Checks("removal wait"); got != 0 {
		t.Fatalf("removal wait checks before any removal = %d, want 0", got)
	}

	// One real removal — a file written, then deleted — consults the wait's
	// check exactly once, so the fixture's later tests cannot pass on a
	// dropped assertion. The wait runs with the append domain released, so
	// the removal records no violation.
	doomed := fid(70)
	if _, err := c.WriteAt(context.Background(), shares[0], doomed, 0, []byte("doomed"), 1); err != nil {
		t.Fatalf("WriteAt of the doomed file: %v", err)
	}
	if _, err := c.Delete(context.Background(), shares[0], doomed); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := rec.Checks("removal wait"); got != 1 {
		t.Fatalf("removal wait checks after one deletion = %d, want 1", got)
	}
	if got := rec.Violations(); len(got) != 0 {
		t.Fatalf("recorder violations after the deletion: %+v", got)
	}

	// The probes hold a rank by hand, through the recorder's own entry
	// point, then drive one real entry point of each component; each must
	// report exactly the out-of-order acquisitions the wired lock makes,
	// so a dropped SetRecorder on any of them fails here.
	expectOutOfOrder(t, c, rec, lockorder.SegmentAppend, "pool",
		[]lockorder.Rank{lockorder.Capacity}, func() { c.pool.ReadyContent() })
	expectOutOfOrder(t, c, rec, lockorder.SegmentAppend, "set",
		[]lockorder.Rank{lockorder.Capacity}, func() { c.set.Counters() })
	// A view of a written file takes the shard's file map lock, releases
	// it, then the file's index lock: two acquisitions out of order, the
	// map's first.
	expectOutOfOrder(t, c, rec, lockorder.SegmentAppend, "index",
		[]lockorder.Rank{lockorder.FileMap, lockorder.FileIndex}, func() {
			c.index.Span(fid(40), core.Extent{Off: 0, Len: 1024})
		})
	expectOutOfOrder(t, c, rec, lockorder.SegmentAppend, "capacity admission",
		[]lockorder.Rank{lockorder.Capacity}, func() { c.cap.ReservedBytes() })
	expectOutOfOrder(t, c, rec, lockorder.SegmentAppend, "headroom",
		[]lockorder.Rank{lockorder.Capacity}, func() { c.headroom.Draws() })
	expectOutOfOrder(t, c, rec, lockorder.Capacity, "file append table",
		[]lockorder.Rank{lockorder.FileAppend}, func() {
			unlock := c.domains.Lock(fid(99))
			unlock()
		})

	// The stream probes hold a plain storage guard — the one rank ordered
	// above the segment append a probe can hold — then read each stream's
	// room, which takes that stream's append lock alone. One probe per
	// stream, so a stream whose lock was left unranked fails its own.
	for i := 0; i < c.streams.Len(); i++ {
		expectOutOfOrder(t, c, rec, lockorder.StorageGuard, fmt.Sprintf("stream %d", i),
			[]lockorder.Rank{lockorder.SegmentAppend}, func() { c.streams.Stream(i).Room() })
	}

	// The descriptor cache's guard is a pin, which orders nothing and
	// breaks no rule: its wiring is proven by the pin appearing on this
	// goroutine's books across a reference's lifetime and leaving with it.
	pins := countRank(rec.Held(), lockorder.StorageGuard)
	placed, _ := c.index.Span(fid(40), core.Extent{Off: 0, Len: 1024})
	if len(placed) == 0 {
		t.Fatalf("the fixture's written file holds no placed record")
	}
	ref, err := c.cache.Acquire(placed[0].Record.Segment)
	if err != nil {
		t.Fatalf("Acquire of the fixture's segment: %v", err)
	}
	// A reference is released exactly once, on the passing path and on a
	// failed assertion alike: the deferred closure releases a reference no
	// explicit release returned from, so a failed assertion cannot leak it
	// into the cleanup's Close — whose Shutdown waits for every reference
	// and would hang — while a second release on the passing path would
	// corrupt the cache's refcounts.
	var released bool
	defer func() {
		if !released {
			ref.Release()
		}
	}()
	if got := countRank(rec.Held(), lockorder.StorageGuard); got != pins+1 {
		t.Fatalf("cache wiring: storage guard holds while the reference is held = %d, want %d", got, pins+1)
	}
	ref.Release()
	released = true
	if got := countRank(rec.Held(), lockorder.StorageGuard); got != pins {
		t.Fatalf("cache wiring: storage guard holds after the reference released = %d, want %d", got, pins)
	}
}

// expectOutOfOrder drives one component's probe: it holds the rank held by
// hand on a fresh goroutine — the recorder keeps each goroutine's books
// apart, so a probe there is free of the pins the fixture's own appends left
// on the test goroutine's — runs drive, one real entry point of the
// component, and asserts the recorder reports exactly one out-of-order
// acquisition for each rank in want, in order, each against the probe's own
// hold. A component whose SetRecorder wiring was dropped keeps its locks
// unranked, records nothing, and fails the count.
func expectOutOfOrder(t *testing.T, c *Core, rec *lockorder.Recorder, held lockorder.Rank, component string, want []lockorder.Rank, drive func()) {
	t.Helper()
	before := len(rec.Violations())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec.Acquire(held, probeLock{component: component})
		drive()
		rec.Release(held, probeLock{component: component})
	}()
	recordedAwait(t, c, component+" probe", done)
	vs := rec.Violations()
	if len(vs) != before+len(want) {
		t.Fatalf("%s probe: violations = %d, want %d: %+v", component, len(vs), before+len(want), vs[before:])
	}
	for i, v := range vs[before:] {
		if v.Kind != lockorder.OutOfOrder || v.Rank != want[i] {
			t.Fatalf("%s probe: violation %d = %s of %s, want out of order of %s", component, i, v.Kind, v.Rank, want[i])
		}
		if held == lockorder.StorageGuard {
			// What must not appear beside the probe's own guard hold
			// is any rank of another domain.
			for _, r := range v.Held {
				if r != lockorder.StorageGuard {
					t.Fatalf("%s probe: violation %d held = %v, want storage guard holds only", component, i, v.Held)
				}
			}
			continue
		}
		if len(v.Held) != 1 || v.Held[0] != held {
			t.Fatalf("%s probe: violation %d held = %v, want [%s]", component, i, v.Held, held)
		}
	}
}

// countRank counts how many of the goroutine's held locks are of rank r.
func countRank(held []lockorder.Rank, r lockorder.Rank) int {
	n := 0
	for _, h := range held {
		if h == r {
			n++
		}
	}
	return n
}

// probeLock names the probe's hold books: the recorder's entry point takes
// any identity, and one struct per component never collides with a real
// lock's.
type probeLock struct{ component string }

// withoutPins returns the held ranks with the storage guard reference pins a
// stream's appender leaves on the writing goroutine's books taken out: a pin
// is a shared hold that never waits on anything and orders nothing, so an
// assertion on a goroutine that appended wants its locks, not its pins.
func withoutPins(held []lockorder.Rank) []lockorder.Rank {
	out := held[:0:0]
	for _, h := range held {
		if h != lockorder.StorageGuard {
			out = append(out, h)
		}
	}
	return out
}

// abandonRecorded leaves the core unclosed when its test already failed on
// the watchdog: the cleanup's Close would wait on the blocked call the run
// failed the test on, so the core is registered as abandoned and the cleanup
// skips it, the same reason the public concurrency test abandons its
// journal rather than closing it.
func abandonRecorded(t *testing.T, c *Core) {
	t.Helper()
	abandonedCores.Store(c, struct{}{})
}

// otherFreshFileOn picks a fresh file id on stream n other than the ones
// named, starting its search past every id the other fresh-picking helpers
// hand out so two picks on one stream cannot collide.
func otherFreshFileOn(c *Core, n int, not ...core.FileID) core.FileID {
outer:
	for k := uint64(1100); ; k++ {
		id := fid(k)
		if c.streams.IndexFor(id) != n {
			continue
		}
		for _, f := range not {
			if f == id {
				continue outer
			}
		}
		return id
	}
}
