package datapath

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/storage"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// TestCallsAfterCloseFailErrClosed checks the fence from every entry point of
// the data path the test names — the handle calls that write, read and
// reserve (WriteAt, ReadAt, Fill, Reserve, WriteReserved, Delete, Since) and
// the journal-level calls (Share, Draw, SetVersionLimit, Strand): once
// BeginClose ran — before the core itself is torn down — each is refused
// with nothing changed. A missing gate lets its call through and fails its
// leg: the handle calls return their own results, the journal-level calls
// succeed. Truncate, Deallocate and CloneTarget take the same beginCall gate
// Delete does and are left to it, and the bare-enter surface (Settle,
// Extents, Files) is each to its own tests.
func TestCallsAfterCloseFailErrClosed(t *testing.T) {
	c, _ := newTestCore(t)
	f1, f2 := testFile(1), testFile(2)
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	ctx := context.Background()

	// The reservation is taken before the fence, so WriteReserved has one to
	// draw on and only the fence can refuse it.
	r, err := c.Reserve(ctx, s1, 4<<10, 1)
	if err != nil {
		t.Fatalf("the Reserve before the fence = %v, want nil", err)
	}

	c.BeginClose()

	if _, err := c.WriteAt(ctx, s1, f1, 0, []byte("late"), 1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the WriteAt refused by the fence = %v, want ErrClosed", err)
	}
	p := make([]byte, 4)
	if _, _, _, _, err := c.ReadAt(ctx, s2, f2, 0, p); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the ReadAt refused by the fence = %v, want ErrClosed", err)
	}
	if err := c.Fill(ctx, s2, f2, 0, []byte("late"), core.Version{}, core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Fill refused by the fence = %v, want ErrClosed", err)
	}
	if _, err := c.Reserve(ctx, s1, 4<<10, 1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Reserve refused by the fence = %v, want ErrClosed", err)
	}
	if _, err := c.WriteReserved(ctx, s1, r, f1, 0, []byte("late"), 1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the WriteReserved refused by the fence = %v, want ErrClosed", err)
	}
	if _, err := c.Delete(ctx, s1, f1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Delete refused by the fence = %v, want ErrClosed", err)
	}
	var sinceErr error
	for _, err := range c.Since(ctx, s1, f1, core.Version{}) {
		sinceErr = err
		break
	}
	if !errors.Is(sinceErr, core.ErrClosed) {
		t.Fatalf("the Since refused by the fence = %v, want ErrClosed", sinceErr)
	}
	if _, err := c.Share(3, 0, core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Share after the close began = %v, want ErrClosed", err)
	}
	if _, err := c.Draw(); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Draw after the close began = %v, want ErrClosed", err)
	}
	if err := c.SetVersionLimit(core.Version{Counter: limitCount}); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the SetVersionLimit after the close began = %v, want ErrClosed", err)
	}
	if err := c.Strand(3); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the Strand after the close began = %v, want ErrClosed", err)
	}
}

// TestInFlightCallsFinishAtClose checks the drain: Close waits for the calls
// already in flight on every fence share — a write parked before its append
// with its file's append domain held, and a read parked before its storage
// read — and returns nil only after both returned their own results. A close
// that stopped the core under its in-flight calls instead of draining them
// returns while the park holds both, and fails the bounded wait that is the
// assertion.
func TestInFlightCallsFinishAtClose(t *testing.T) {
	writeParked, writeRelease := make(chan struct{}), make(chan struct{})
	readParked, readRelease := make(chan struct{}), make(chan struct{})
	var appendN, readN atomic.Int32
	c, _ := newTestCore(t, func(o *coreOptions) {
		// The parked write is the first call to reach beforeAppend and the
		// parked read the first to reach beforeRead; the probe write below is
		// a later arrival at beforeAppend and passes the seam through.
		o.beforeAppend = func() {
			if appendN.Add(1) == 1 {
				close(writeParked)
				<-writeRelease
			}
		}
		o.beforeRead = func() {
			if readN.Add(1) == 1 {
				close(readParked)
				<-readRelease
			}
		}
	})
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	f1, f2 := testFile(1), testFile(2)
	// The parked read's bytes are published before anything runs, through the
	// index and the streams directly, so the read has a record to serve and
	// the park never waits on a warm-up.
	payload := []byte("the bytes the parked read serves")
	publish(t, c, f2, 0, string(payload))
	stopWrite := releaser(t, writeRelease)
	stopRead := releaser(t, readRelease)

	wrote := make(chan writeResult, 1)
	go func() {
		v, err := c.WriteAt(context.Background(), s1, f1, 0, []byte("in flight"), 1)
		wrote <- writeResult{v, err}
	}()
	within(t, writeParked, "the write to park before its append")

	p := make([]byte, len(payload))
	read := make(chan readResult, 1)
	go func() {
		n, missing, held, asOf, err := c.ReadAt(context.Background(), s1, f2, 0, p)
		read <- readResult{n, missing, held, asOf, err}
	}()
	within(t, readParked, "the read to park before its records")

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	// The fence is observable once a probe call through the second share is
	// refused; before the fence it is a real write on a fresh file and simply
	// lands.
	eventually(t, "a probe write to be refused once the fence went up", func() bool {
		_, err := c.WriteAt(context.Background(), s2, testFile(3), 0, []byte("probe"), 1)
		return errors.Is(err, core.ErrClosed)
	})
	// The bounded wait is the assertion, not a sleep before one: a close that
	// fails to drain its in-flight calls returns while the park holds both
	// and fails here.
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while its write and read were still in flight", err)
	case <-time.After(250 * time.Millisecond):
	}

	stopWrite()
	stopRead()

	got := within(t, wrote, "the write that was in flight at Close")
	if got.err != nil {
		t.Fatalf("the write in flight at Close = %v, want its version", got.err)
	}
	if got.v == (core.Version{}) {
		t.Fatal("the write in flight at Close returned no version")
	}
	r := within(t, read, "the read that was in flight at Close")
	if r.err != nil || r.n != len(payload) || !bytes.Equal(p, payload) {
		t.Fatalf("the read in flight at Close = %d %q (%v), want %q whole", r.n, p, r.err, payload)
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close = %v, want nil: it drained its calls in flight first", err)
	}

	// Every call tried after Close returned is refused as one tried before
	// the drain was over.
	ctx := context.Background()
	if _, err := c.WriteAt(ctx, s1, f1, 0, []byte("late"), 1); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the WriteAt after Close = %v, want ErrClosed", err)
	}
	if _, _, _, _, err := c.ReadAt(ctx, s2, f2, 0, p); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the ReadAt after Close = %v, want ErrClosed", err)
	}
	if err := c.Fill(ctx, s1, f1, 0, []byte("late"), core.Version{}, core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the Fill after Close = %v, want ErrClosed", err)
	}
	if _, err := c.Share(4, 0, core.Version{}); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the Share after Close = %v, want ErrClosed", err)
	}
	if _, err := c.Draw(); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the Draw after Close = %v, want ErrClosed", err)
	}
	if err := c.SetVersionLimit(core.Version{Counter: limitCount}); !errors.Is(err, core.ErrClosed) {
		t.Errorf("the SetVersionLimit after Close = %v, want ErrClosed", err)
	}
}

// TestCallsWhileAWriteIsHeldAtItsAppend checks the surface that takes no
// context against a write held at the seam before its append: the parked
// write holds its file's append domain, its admission's grant and its share's
// drain, and Extents, Files, DirtyFiles and Settle must each answer while it
// is held. A call of that surface that waited on the parked write — its
// file's append domain among them — sits past the watchdog and fails its leg
// here rather than surfacing only once Close's drain waits it out. Each call
// names the held write's own file and share, the strongest case. Stats is
// not a leg here: it waits on the change that owns its counters'
// close-time behaviour.
func TestCallsWhileAWriteIsHeldAtItsAppend(t *testing.T) {
	parked, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	var parkN atomic.Int32
	c, _ := newTestCore(t, func(o *coreOptions) {
		// The park answers only the test's own held write: the warm-up below
		// runs before the arm and passes the seam through.
		o.beforeAppend = func() {
			if armed.Load() && parkN.Add(1) == 1 {
				close(parked)
				<-release
			}
		}
	})
	s := mustShare(t, c, 1)
	f := testFile(1)
	warmV, err := c.WriteAt(context.Background(), s, f, 0, []byte("warm"), 1)
	if err != nil {
		t.Fatalf("the warm-up write: %v", err)
	}
	// A failure between the park and the release must not leave the write
	// parked for the core's cleanup to wait on.
	stopWrite := releaser(t, release)

	held := make(chan writeResult, 1)
	armed.Store(true)
	go func() {
		v, err := c.WriteAt(context.Background(), s, f, 1<<20, []byte("held at its append"), 1)
		held <- writeResult{v, err}
	}()
	within(t, parked, "the write to park before its append")

	// Extents names the held write's own file, whose append domain it parks
	// under: a call that took that domain waits on it and fails the bound.
	extents := make(chan error, 1)
	go func() {
		es, err := c.Extents(s, f)
		if err == nil && len(es) == 0 {
			err = errors.New("no extent for the warmed file")
		}
		extents <- err
	}()
	if err := within(t, extents, "Extents while the write is held"); err != nil {
		t.Fatalf("Extents beside the held write = %v, want the warmed extents", err)
	}

	files := make(chan error, 1)
	go func() {
		if !slices.Contains(c.Files(s), f) {
			files <- errors.New("the warmed file is not among the share's files")
			return
		}
		files <- nil
	}()
	if err := within(t, files, "Files while the write is held"); err != nil {
		t.Fatalf("Files beside the held write = %v, want the warmed file listed", err)
	}

	dirty := make(chan error, 1)
	go func() {
		if !slices.ContainsFunc(c.DirtyFiles(s), func(d DirtyFile) bool { return d.File == f }) {
			dirty <- errors.New("the warmed file is not dirty")
			return
		}
		dirty <- nil
	}()
	if err := within(t, dirty, "DirtyFiles while the write is held"); err != nil {
		t.Fatalf("DirtyFiles beside the held write = %v, want the warmed file dirty", err)
	}

	settled := make(chan struct{})
	go func() {
		c.Settle(s, f, warmV)
		close(settled)
	}()
	within(t, settled, "Settle while the write is held")

	stopWrite()
	got := within(t, held, "the held write")
	if got.err != nil {
		t.Fatalf("the held write = %v, want its version", got.err)
	}
	if got.v == (core.Version{}) {
		t.Fatal("the held write returned no version")
	}
}

// seamOps is every operation name the seam counts, so a test can hold all of
// them and compare.
var seamOps = []string{
	kit.OpCreate, kit.OpOpen, kit.OpAllocate, kit.OpWrite, kit.OpRead,
	kit.OpReadDirect, kit.OpSync, kit.OpTruncate, kit.OpClose, kit.OpRename,
	kit.OpRemove, kit.OpList, kit.OpSyncDir,
}

// seamCounts is the seam's call count of every operation, in seamOps order.
func seamCounts(seam *kit.Seam) []int {
	counts := make([]int, len(seamOps))
	for i, op := range seamOps {
		counts[i] = seam.Calls(op)
	}
	return counts
}

// opLog is the ordered event log of one core's storage calls and its close's
// own steps: every event is one string, appended under a mutex so a reader
// sees the order the events happened in.
type opLog struct {
	mu   sync.Mutex
	evts []string
}

func (l *opLog) add(e string) {
	l.mu.Lock()
	l.evts = append(l.evts, e)
	l.mu.Unlock()
}

// snapshot returns the events so far, in order.
func (l *opLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.evts)
}

// closeLogFS is a pass-through wrapper over a real storage.FS: it records the
// calls that can watch the segment set's close — each file it opens and each
// file's close — and answers in the wrapped FS's place never, so every answer
// the core sees is the real one and every count the seam keeps stays true.
type closeLogFS struct {
	log *opLog
	fs  storage.FS
}

func (f closeLogFS) Create(name string) (storage.File, error) {
	file, err := f.fs.Create(name)
	if err != nil {
		return nil, err
	}
	return closeLogFile{log: f.log, f: file}, nil
}

func (f closeLogFS) Open(name string) (storage.File, error) {
	file, err := f.fs.Open(name)
	if err != nil {
		return nil, err
	}
	f.log.add("open " + name)
	return closeLogFile{log: f.log, f: file}, nil
}

func (f closeLogFS) Rename(oldName, newName string) error { return f.fs.Rename(oldName, newName) }

func (f closeLogFS) Remove(name string) error {
	f.log.add("remove " + name)
	return f.fs.Remove(name)
}

func (f closeLogFS) List() ([]string, error) { return f.fs.List() }

func (f closeLogFS) SyncDir() error { return f.fs.SyncDir() }

// closeLogFile is closeLogFS's pass-through over one open file; it records
// the file's close and answers in the wrapped file's place never.
type closeLogFile struct {
	log *opLog
	f   storage.File
}

func (g closeLogFile) ReadAt(p []byte, off int64) (n int, err error) { return g.f.ReadAt(p, off) }

func (g closeLogFile) ReadAtDirect(p []byte, off int64) (n int, err error) {
	return g.f.ReadAtDirect(p, off)
}

func (g closeLogFile) WriteAt(p []byte, off int64) (n int, err error) { return g.f.WriteAt(p, off) }

func (g closeLogFile) Allocate(off, n int64) error { return g.f.Allocate(off, n) }

func (g closeLogFile) Sync() error { return g.f.Sync() }

func (g closeLogFile) Truncate(size int64) error { return g.f.Truncate(size) }

func (g closeLogFile) Size() (int64, error) { return g.f.Size() }

func (g closeLogFile) Close() error {
	g.log.add("file close " + g.f.Name())
	return g.f.Close()
}

func (g closeLogFile) Name() string { return g.f.Name() }

// registeredReader is the repack-like worker of a test's own: a real
// goroutine that takes an open descriptor reference of the segment holding
// the file's record, reads its first bytes, blocks until its stop signals,
// then drops the reference and exits. addStop receives the stop: it signals
// AND joins the worker, and logs name joined once the goroutine has exited.
// The reference it holds keeps the descriptor open, so a set close that shut
// the cache before the worker was joined would wait on it forever. It
// returns the channel closed once the reader holds and has read the
// reference.
func registeredReader(t *testing.T, c *Core, log *opLog, f core.FileID, name string) <-chan struct{} {
	t.Helper()
	p, ok := c.Index().Lookup(f, 1<<20)
	if !ok {
		t.Fatalf("file %v holds no record for the reader to touch", f)
	}
	seg := format.SegmentName(p.Record.Segment)
	release := make(chan struct{})
	ready := make(chan struct{})
	var joined sync.WaitGroup
	joined.Add(1)
	go func() {
		defer joined.Done()
		ref, err := c.cache.Acquire(p.Record.Segment)
		if err != nil {
			log.add(name + " could not open " + seg + ": " + err.Error())
			return
		}
		buf := make([]byte, 16)
		_, _ = ref.ReadAt(buf, 0)
		log.add(name + " is reading " + seg)
		close(ready)
		<-release
		ref.Release()
	}()
	c.addStop(func() {
		close(release)
		joined.Wait()
		log.add(name + " joined")
	})
	return ready
}

// assertJoinedBeforeClose checks the close's order on the log: every worker
// of names joined before the set closed, which the log shows as each joined
// event preceding the close of its own segment file. A worker left unjoined
// holds its descriptor's reference, so the set close would wait on it and
// Close would not have returned.
func assertJoinedBeforeClose(t *testing.T, log *opLog, c *Core, files map[string]core.FileID) {
	t.Helper()
	evts := log.snapshot()
	for name, f := range files {
		e, _ := c.Index().Lookup(f, 1<<20)
		seg := format.SegmentName(e.Record.Segment)
		joined, closedAt := -1, -1
		for i, ev := range evts {
			switch ev {
			case name + " joined":
				joined = i
			case "file close " + seg:
				if closedAt < 0 {
					closedAt = i
				}
			}
		}
		if joined < 0 {
			t.Fatalf("%s never joined before Close returned: events %v", name, evts)
		}
		if closedAt < 0 {
			t.Fatalf("Close returned without closing %s's own segment %v: events %v", name, seg, evts)
		}
		if joined > closedAt {
			t.Fatalf("%s joined at event %d, after its segment's close at %d: events %v", name, joined, closedAt, evts)
		}
	}
}

// TestRegisteredWorkerJoinsBeforeSetCloses checks the stop-function registry:
// a worker the core has registered — a goroutine holding an open descriptor
// reference and reading its segment — is signalled and joined by Close before
// the segment set is closed, so the descriptor it holds is still open and
// still valid while the worker is running. A close that shut the set first
// would wait on the worker's reference forever and never return; a close that
// stopped nothing leaves the goroutine running.
func TestRegisteredWorkerJoinsBeforeSetCloses(t *testing.T) {
	log := &opLog{}
	c, seam := newTestCore(t, func(o *coreOptions) {
		o.wrapFS = func(fs storage.FS) storage.FS { return closeLogFS{log: log, fs: fs} }
	})
	s := mustShare(t, c, 1)
	f := testFile(1)
	warm(t, c, s, f)

	within(t, registeredReader(t, c, log, f, "the worker"), "the registered worker to hold its segment")

	log.add("begin")
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	if err := within(t, closed, "Close to join the registered worker then close the set"); err != nil {
		t.Fatalf("Close = %v, want nil: the registered worker joined before the set closed", err)
	}
	assertJoinedBeforeClose(t, log, c, map[string]core.FileID{"the worker": f})

	// Nothing reaches the storage after Close returned. The wait is bounded
	// and the real condition is that the counts stay at what Close left them:
	// any late goroutine reaching storage shows up as a changed count.
	before := seamCounts(seam)
	for deadline := time.Now().Add(250 * time.Millisecond); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if cur := seamCounts(seam); !slices.Equal(cur, before) {
			t.Fatalf("storage changed after Close returned: counts %v became %v, want unchanged", before, cur)
		}
	}
}

// TestSetClosesAfterLastStopReturns checks the registry's order: both of two
// registered workers are joined — each records its own stop's completion in
// the log — before the set's close observes its first storage call. A stop
// sequence that closed the set before the last stop would wait on the last
// worker's reference and never return.
func TestSetClosesAfterLastStopReturns(t *testing.T) {
	log := &opLog{}
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.wrapFS = func(fs storage.FS) storage.FS { return closeLogFS{log: log, fs: fs} }
	})
	s := mustShare(t, c, 1)
	f1, f2 := testFile(1), testFile(2)
	warm(t, c, s, f1, f2)

	w1 := registeredReader(t, c, log, f1, "worker 1")
	w2 := registeredReader(t, c, log, f2, "worker 2")
	within(t, w1, "the first registered worker to hold its segment")
	within(t, w2, "the second registered worker to hold its segment")

	log.add("begin")
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	if err := within(t, closed, "Close to stop both workers then close the set"); err != nil {
		t.Fatalf("Close = %v, want nil: both registered workers stopped before the set closed", err)
	}
	// The log carries each stop's own record of its completion, in the order
	// its share of the close ran it; both must precede the set's closes.
	assertJoinedBeforeClose(t, log, c, map[string]core.FileID{"worker 1": f1, "worker 2": f2})
}

// TestPoolStopsBeforeRetirer pins the registry's order against the segment
// group's own: the pool stops before the retirer, so the wake the retirer's
// queue drop sends reaches a pool that makes nothing, and a retirement still
// in flight unlinks its segment rather than reserving a slot in a live pool.
// The probe registers between the pool's and the retirer's stop, so reverse
// order runs it after the pool stopped and before the retirer stops, and it
// reads the state the pool's Stop doc names: Take on a stopped pool answers
// core.ErrClosed. A registry that left the pool to the group's close runs
// the probe while the pool still serves takes, and the take nothing refused
// fails the probe's assertion.
func TestPoolStopsBeforeRetirer(t *testing.T) {
	probeDone := make(chan error, 1)
	c, _ := newTestCore(t, func(o *coreOptions) {
		o.stopProbe = func(p *segment.Pool) func() {
			return func() {
				// Take is the stopped state the pool's Stop doc names; a
				// stopped pool refuses it changing nothing, and a live one
				// answers a real spare.
				_, err := p.Take(segment.Content)
				probeDone <- err
			}
		}
	})

	// Close runs behind the package's watchdog, so a close that blocked on a
	// wrong order fails at the watchdog's message instead of hanging the run
	// to the suite's timeout. The core is abandoned before the failure is
	// raised, for the cleanup's own Close waits on this one through the
	// close's once and would block on the same stuck close.
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
	case <-time.After(watchdog):
		abandonRecorded(t, c)
		t.Fatalf("Close did not return within %s: the group closed before the workers it owns stopped", watchdog)
	}
	got := within(t, probeDone, "the probe's stop, between the pool's and the retirer's")
	if !errors.Is(got, core.ErrClosed) {
		t.Fatalf("when the probe's stop ran, the pool still served a take: Take = %v, want ErrClosed", got)
	}
}

// TestRemovalInWaitDurableIsWaited checks the drain against the removal that
// is waiting for its record's durability: Close fences the share, refuses a
// probe while the removal stays parked in its durability wait, and returns
// nil only after the removal ended. The parked wait is released two ways: the
// held sync completing, which returns the removal its version, and the held
// sync failing, which answers the removal whatever its durability wait
// yields — its own refusal, never the core's ErrClosed, never its context's
// error — with Close still returning nil. The removal's context is cancelled
// while it is parked in the failed-sync leg, so a wait that consulted it
// would answer it and fail the leg.
func TestRemovalInWaitDurableIsWaited(t *testing.T) {
	parkRemoval := func(t *testing.T, fail bool) (chan writeResult, chan error, func()) {
		t.Helper()
		c, seam := newTestCore(t)
		s := mustShare(t, c, 1)
		f := testFile(1)
		warm(t, c, s, f)
		rec, _ := c.Index().Lookup(f, 1<<20)
		seg := format.SegmentName(rec.Record.Segment)
		hold := seam.HoldFirst(t, kit.OpSync, seg)
		if fail {
			if err := seam.Schedule(kit.Failing(kit.OpSync, seg, 1, errors.New("disk on fire"))); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		removed := make(chan writeResult, 1)
		go func() {
			v, err := c.Delete(ctx, s, f)
			removed <- writeResult{v, err}
		}()
		within(t, hold.Reached(), "the removal's segment sync to be parked")

		closed := make(chan error, 1)
		go func() { closed <- c.Close() }()

		// The fence is observable once a probe write is refused; before it,
		// the probe on a fresh file lands and its refusal never comes.
		eventually(t, "a probe write to be refused once the fence went up", func() bool {
			_, err := c.WriteAt(context.Background(), s, testFile(2), 0, []byte("probe"), 1)
			return errors.Is(err, core.ErrClosed)
		})
		select {
		case err := <-closed:
			t.Fatalf("Close returned %v while its removal was parked in its durability wait", err)
		case <-time.After(250 * time.Millisecond):
		}

		return removed, closed, func() {
			cancel()
			hold.Release()
		}
	}

	t.Run("the held sync released", func(t *testing.T) {
		removed, closed, end := parkRemoval(t, false)
		end()
		got := within(t, removed, "the removal in flight at Close")
		if got.err != nil {
			t.Fatalf("the removal in flight at Close = %v, want its version", got.err)
		}
		if got.v == (core.Version{}) {
			t.Fatal("the removal in flight at Close returned no version")
		}
		if err := <-closed; err != nil {
			t.Fatalf("Close = %v, want nil: it waited the removal out first", err)
		}
	})

	t.Run("the held sync failed", func(t *testing.T) {
		removed, closed, end := parkRemoval(t, true)
		end()
		got := within(t, removed, "the removal whose segment sync failed")
		if errors.Is(got.err, core.ErrClosed) {
			t.Fatalf("the removal answered the core's close: %v, want its own result", got.err)
		}
		if errors.Is(got.err, context.Canceled) || errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("the removal answered its cancelled context: %v, want its own result", got.err)
		}
		if got.err == nil && got.v == (core.Version{}) {
			t.Fatal("the removal completed but returned no version")
		}
		if err := <-closed; err != nil {
			t.Fatalf("Close = %v, want nil after its removal was answered", err)
		}
	})
}

// TestNoGoroutineOutlivesClose covers the close's leak bound once, with a
// close under load: a fan of concurrent writes, reads, removals and Since
// scans over two shares, both files and both streams, and one fan write
// parked at its append while Close runs, so the fence refuses the fan's
// later arrivals and the drain waits the parked write out — the leak watch
// then sees a close that really drained. The profiler's watch — installed
// before the core is built, so its check runs after the core's cleanup closed
// it — fails the test if any goroutine the fan or the core started is still
// running when the test ends.
func TestNoGoroutineOutlivesClose(t *testing.T) {
	prof.Watch(t, prof.Options{})
	parked := make(chan struct{})
	release := make(chan struct{})
	var armed atomic.Bool
	var parkN atomic.Int32
	c, _ := newTestCore(t, func(o *coreOptions) {
		// One fan write parks before its first append, held for the close to
		// arrive at; the warm-ups before the arm and every later fan arrival
		// pass the seam through.
		o.beforeAppend = func() {
			if armed.Load() && parkN.Add(1) == 1 {
				close(parked)
				<-release
			}
		}
	})
	// A failure between the park and the release must not leave the write
	// parked for the core's cleanup to wait on.
	stopPark := releaser(t, release)
	s1, s2 := mustShare(t, c, 1), mustShare(t, c, 2)
	f1, f2 := testFile(1), testFile(2)
	warm(t, c, s1, f1)
	warm(t, c, s2, f2)

	ctx := context.Background()
	var fan sync.WaitGroup
	run := func(n int, step func(i int)) {
		fan.Add(1)
		go func() {
			defer fan.Done()
			for i := 0; i < n; i++ {
				step(i)
			}
		}()
	}
	// One write is parked in flight before the close begins, so the drain has
	// a call to wait for and the watcher sees it end.
	heldDone := make(chan writeResult, 1)
	armed.Store(true)
	go func() {
		v, err := c.WriteAt(ctx, s1, f1, int64(1<<20), []byte("held for the close"), 1)
		heldDone <- writeResult{v, err}
	}()
	within(t, parked, "the fan's held write to park before its append")

	p := make([]byte, 64)
	run(50, func(i int) { _, _ = c.WriteAt(ctx, s1, f1, int64(i)*11, []byte("fan"), 1) })
	run(50, func(i int) { _, _ = c.WriteAt(ctx, s2, f2, int64(i)*13, []byte("fan"), 1) })
	run(20, func(i int) {
		if i%5 == 4 {
			_, _ = c.Delete(ctx, s2, f2)
			return
		}
		_, _ = c.WriteAt(ctx, s2, f2, int64(1<<20), []byte("fan again"), 1)
	})
	run(20, func(i int) { _, _, _, _, _ = c.ReadAt(ctx, s1, f1, int64(i)*7, p) })
	run(5, func(i int) {
		for ch, err := range c.Since(ctx, s1, f1, core.Version{}) {
			if err != nil {
				break
			}
			_ = ch
		}
	})

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()

	// The bounded wait is the assertion, not a sleep before one: a close that
	// returned while its held write was still in flight drained nothing.
	select {
	case err := <-closed:
		t.Fatalf("Close returned %v while its fan's write was still in flight", err)
	case <-time.After(250 * time.Millisecond):
	}

	stopPark()
	got := within(t, heldDone, "the write held in flight at Close")
	if got.err != nil {
		t.Fatalf("the write held in flight at Close = %v, want its version", got.err)
	}
	if got.v == (core.Version{}) {
		t.Fatal("the write held in flight at Close returned no version")
	}
	// The fan keeps calling until every step ran out; the calls the fence
	// refuses answer ErrClosed at once, so none of them can hang.
	fan.Wait()

	if err := within(t, closed, "Close after the fan and its held write"); err != nil {
		t.Fatalf("Close under the fan = %v, want nil", err)
	}
	if _, err := c.WriteAt(ctx, s1, f1, 0, []byte("late"), 1); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the WriteAt after Close = %v, want ErrClosed", err)
	}
}

// TestReadBegunAfterCloseFails checks the fence's reach before any state
// change: a ReadAt begun after BeginClose is refused with ErrClosed before
// it touches storage, so the seam's counts stay what they were — a read that
// slipped past the fence would reach its records and count there, failing
// the unchanged-counts assertion.
func TestReadBegunAfterCloseFails(t *testing.T) {
	c, seam := newTestCore(t)
	s := mustShare(t, c, 1)
	f := testFile(1)
	// The file holds a warmed record, so a read that reached storage would
	// count at the seam.
	warm(t, c, s, f)

	before := seamCounts(seam)
	c.BeginClose()
	p := make([]byte, 8)
	if _, _, _, _, err := c.ReadAt(context.Background(), s, f, 1<<20, p); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("the ReadAt begun after the fence = %v, want ErrClosed", err)
	}
	if after := seamCounts(seam); !slices.Equal(after, before) {
		t.Fatalf("the refused read touched storage: counts %v before the refusal and %v after, want unchanged", before, after)
	}
}
