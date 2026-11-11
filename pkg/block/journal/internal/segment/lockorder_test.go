package segment_test

import (
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// watchdog fails the test with every goroutine's stack when the run does not
// return within one generous bound, so a deadlock in the recorded run is
// diagnosed from the dump rather than from the binary's own timeout: the bound
// is the backstop, never the synchronisation.
func watchdog(t *testing.T, bound time.Duration) {
	t.Helper()

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		select {
		case <-done:
		case <-time.After(bound):
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Errorf("the recorded run did not return within %v; the goroutine stacks follow:\n%s", bound, buf[:n])
		}
	}()
}

// recordedRounds is the record count each stream of the recorded run appends:
// a payload an eighth of the cap rolls the stream every few records, so the
// run exercises rolls and reader holds against the recorder.
const recordedRounds = 40

// recordedPayload is the payload one recorded record carries.
var recordedPayload = make([]byte, 8<<10)

// recordedEnv is the journal's whole segment layer over a real directory, with
// the recorder installed on every lock the layer ranks before any of it is
// used: two streams, their ledger, spare pool and descriptor cache.
type recordedEnv struct {
	seam  *kit.Seam
	ss    *segment.Streams
	set   *segment.Set
	m     *segment.Maker
	pool  *segment.Pool
	cache *segment.Descriptors
	rec   *lockorder.Recorder
}

// recordedStreams builds the recordedEnv: the refill worker starts only once
// the ranks are Init'ed, so every acquisition the run makes is one the
// recorder saw. The cache's Shutdown is registered before the pool's Stop so
// the shutdown runs after the join that held descriptors through it.
func recordedStreams(t *testing.T) *recordedEnv {
	t.Helper()

	seam := newSeam(t)
	set := segment.NewSet(segCap, nil)
	m := segment.NewMaker(seam.FS(), set, noMax, 0, nil)
	pool := segment.NewPool(m, segment.Targets{Content: 2}, nil, nil)
	cache := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(64))
	rec := lockorder.NewRecorder()
	set.SetRecorder(rec)
	pool.SetRecorder(rec)
	cache.SetRecorder(rec)
	ss := segment.NewStreams(2, streamIdentity, set, pool, cache, nopSinkFor)
	ss.SetRecorder(rec)
	t.Cleanup(func() {
		if err := cache.Shutdown(); err != nil {
			t.Errorf("the cache's Shutdown = %v, want nil", err)
		}
	})
	pool.Start()
	t.Cleanup(func() { pool.Stop() })
	return &recordedEnv{seam: seam, ss: ss, set: set, m: m, pool: pool, cache: cache, rec: rec}
}

// reportedViolations keeps the violations each recorder's checks have
// already failed the test for, per recorder, so a violation one goroutine's
// assertion saw is not re-reported by every other goroutine's — and a fresh
// recorder in a later test reports its own violations anew.
var reportedViolations = struct {
	sync.Mutex
	seen map[*lockorder.Recorder]map[string]struct{}
}{seen: make(map[*lockorder.Recorder]map[string]struct{})}

// assertNoViolations fails t on every violation the recorder collected, each
// once, and fails once more when the calling goroutine still holds a rank
// other than a storage guard pin: a pin is the stream's active ref, held
// from the roll that took it to the roll or close that releases it, so a
// worker that ends between the two holds its segment's pin and nothing
// else. It is called on every goroutine the recorded run launches, at its
// own end.
func assertNoViolations(t *testing.T, rec *lockorder.Recorder, where string) {
	t.Helper()

	if vs := rec.Violations(); len(vs) != 0 {
		reportedViolations.Lock()
		if reportedViolations.seen[rec] == nil {
			reportedViolations.seen[rec] = make(map[string]struct{})
		}
		reported := reportedViolations.seen[rec]
		reportedViolations.Unlock()
		for _, v := range vs {
			reportedViolations.Lock()
			_, dup := reported[v.Stack]
			if !dup {
				reported[v.Stack] = struct{}{}
			}
			reportedViolations.Unlock()
			if dup {
				continue
			}
			t.Errorf("%s: recorder violation: %s acquiring %s over %v\n%s", where, v.Kind, v.Rank, v.Held, v.Stack)
		}
		return
	}
	for _, h := range rec.Held() {
		if h != lockorder.StorageGuard {
			t.Errorf("%s: the goroutine holds ranks %v after the run, want only the stream's pin at most", where, rec.Held())
			return
		}
	}
}

// recordedLaunch runs fn on n goroutines and joins them, on the WaitGroup and
// not on a clock.
func recordedLaunch(n int, fn func(i int)) {
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			fn(i)
		}()
	}
	wg.Wait()
}

// TestStreamsUnderRecorder runs the layer's whole concurrent fan under the
// recorder: two streams appending and rolling while readers acquire and
// release the same segments' descriptors, and a retirement draining a sealed
// segment under a reader's hold. The run must record no violation and leave
// every goroutine holding nothing.
func TestStreamsUnderRecorder(t *testing.T) {
	prof.Watch(t, prof.Options{})
	watchdog(t, 60*time.Second)

	e := recordedStreams(t)
	awaitCounters(t, "the two content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	// The locations the appenders publish, read as they land by both
	// readers, so a reader's acquire races the writer's roll of the same
	// segment. A non-blocking send keeps an unread location from stalling
	// an appender: the readers drain the channel far faster than the
	// appends fill it.
	locs := make(chan segment.Location, 8*recordedRounds)

	// The readers run beside the appenders, not after them: each round
	// reads one location twice — through the stream's own ReadAt, an
	// acquire and a release per read, and through a reference held across
	// the read, the shape a reader of one record in pieces takes — until
	// the appenders join and the stop channel closes. A segment already
	// retired answers its refusal to the reader that watches for it, not
	// an error.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(2)
	for range 2 {
		go func() {
			defer readers.Done()
			s := e.ss.Stream(0)
			buf := make([]byte, format.RecordHeaderSize)
			for {
				select {
				case <-stop:
					assertNoViolations(t, e.rec, "the reader")
					return
				case loc := <-locs:
					if _, err := s.ReadAt(loc.Segment, buf, loc.Off); err != nil {
						t.Errorf("ReadAt of the record at %d: %v", loc.Off, err)
						return
					}
					ref, err := s.AcquireRead(loc.Segment)
					if err == nil {
						_, rerr := ref.ReadAt(buf, loc.Off)
						ref.Release()
						if rerr != nil {
							t.Errorf("the held reference's read: %v", rerr)
							return
						}
					} else if !slices.Contains([]error{segment.ErrStreamClosed, segment.ErrSegmentGone, segment.ErrCacheClosed}, err) {
						t.Errorf("AcquireRead: %v", err)
						return
					}
				default:
					runtime.Gosched()
				}
			}
		}()
	}

	// Each stream must roll at least once for the run to say anything
	// about rolls: the appends each carry an eighth of the cap, so a
	// stream's locations must span at least two distinct segments. The
	// map is mutex-guarded, for the two appenders fill it together.
	var distMu sync.Mutex
	distinct := make(map[core.SegmentID]struct{})
	recordedLaunch(2, func(i int) {
		s := e.ss.Stream(i)
		fid := streamFileID(i)
		for v := range recordedRounds {
			h, body := streamWriteHeader(fid, int64(v)*int64(len(recordedPayload)), recordedPayload, core.ShareTag(i+1), core.Version{Counter: uint64(v + 1)})
			p, err := s.Append(h, body)
			if err != nil {
				t.Errorf("stream %d's append %d: %v", i, v, err)
				return
			}
			distMu.Lock()
			distinct[p.Loc.Segment] = struct{}{}
			distMu.Unlock()
			select {
			case locs <- p.Loc:
			default:
			}
		}
		assertNoViolations(t, e.rec, "the appender")
	})
	close(stop)
	readers.Wait()
	close(locs)
	if len(distinct) < 4 {
		t.Fatalf("the run's appends landed in %d distinct segments, want at least 4 (two rolls per stream at least)", len(distinct))
	}

	// The retirer drains one. The active segment is the one the next
	// record lands in; the appenders have all joined, so the segment
	// stream 0's small record below lands in is the one Seal then closes.
	// The roll away after the seal releases the stream's own reference to
	// it — the one the drain would else wait on forever — and the retirer
	// retires it while a reader still holds its descriptor, so the drain
	// wait runs under the recorder with a real reference outstanding.
	s0 := e.ss.Stream(0)
	active, err := appendSmall(t, s0, 1, uint64(recordedRounds+1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s0.Seal(true); err != nil {
		t.Fatalf("Seal of stream 0's active segment: %v", err)
	}
	awaitCounters(t, "a spare for the roll away", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares >= 1 })
	if _, err := appendSmall(t, s0, 2, uint64(recordedRounds+2)); err != nil {
		t.Fatal(err)
	}
	r := segment.NewRetirer(segment.RetirerConfig{
		Set:   e.set,
		Cache: e.cache,
		Pool:  e.pool,
		Maker: e.m,
		FS:    e.seam.FS(),
	})
	armed := make(chan struct{})
	release := make(chan struct{})
	go func() {
		ref, err := e.cache.Acquire(active)
		if err != nil {
			t.Errorf("the reader's hold of the sealed segment %d: %v", active, err)
			close(armed)
			return
		}
		close(armed)
		<-release
		ref.Release()
		assertNoViolations(t, e.rec, "the retirer's reader")
	}()
	<-armed
	go func() {
		for !segment.EntryGone(e.cache, active) {
			runtime.Gosched()
		}
		close(release)
	}()
	// The retirement runs on a goroutine that holds no pin: the test
	// goroutine still pins the active segment its own appends landed in,
	// and a drain wait taken across any pin is reported — production
	// retirers hold none, so the check must not run on this one's books.
	retired := make(chan error, 1)
	go func() {
		_, err := r.Retire(active, segment.NotHeld)
		retired <- err
		assertNoViolations(t, e.rec, "the retirer")
	}()
	if err := <-retired; err != nil {
		t.Fatalf("Retire of the sealed segment %d: %v", active, err)
	}

	// The streams close before the cache's Shutdown cleanup: each stream
	// holds its active segment's reference until its close, and a shutdown
	// that ran first would wait on them forever. The close is also what
	// clears the test goroutine's own pin, so the empty-books check runs
	// after it alone.
	if err := e.ss.Close(); err != nil {
		t.Fatalf("the streams' close: %v", err)
	}
	if held := e.rec.Held(); len(held) != 0 {
		t.Fatalf("the test goroutine holds ranks %v after the close, want none", held)
	}

	// The zero above would pass over locks that were never recorded, so
	// the same recorder is made to prove it saw the ranked locks of this
	// very fixture: with a higher rank held on this goroutine, one read
	// per lock — a stream's append lock, the pool's and the ledger's —
	// and each that goes unreported means an unwired Init. The probes
	// hold a foreign identity, so no real lock is taken.
	probe := lockorder.StorageGuard
	e.rec.Acquire(probe, "probe")
	_ = e.ss.Stream(0).Room()
	_ = e.pool.Counters()
	_ = e.set.Counters()
	vs := e.rec.Violations()
	e.rec.Release(probe, "probe")
	// One report per lock: rank storage guard is held, so each capacity
	// and segment append acquisition goes out of order.
	if len(vs) != 3 {
		t.Fatalf("the probes recorded %d violations, want 3 (one per ranked lock of the fixture): %+v", len(vs), vs)
	}
	saw := map[lockorder.Rank]bool{}
	for _, v := range vs {
		if v.Kind != lockorder.OutOfOrder {
			t.Errorf("probe violation kind = %v, want out of order (acquiring %s over %v)", v.Kind, v.Rank, v.Held)
		}
		saw[v.Rank] = true
	}
	for _, rank := range []lockorder.Rank{lockorder.SegmentAppend, lockorder.Capacity} {
		if !saw[rank] {
			t.Errorf("the probes saw no %s violation; the fixture left that lock unwired (got ranks %v)", rank, vs)
		}
	}
	if held := e.rec.Held(); len(held) != 0 {
		t.Fatalf("the test goroutine holds ranks %v after the probes' release, want none", held)
	}
}

// appendSmall appends one 1-byte write of file i to s and returns the segment
// it landed in: the stream's active segment at the call, so a check can seal
// or roll exactly there.
func appendSmall(t *testing.T, s *segment.Stream, i int, version uint64) (core.SegmentID, error) {
	t.Helper()

	h, body := streamWriteHeader(streamFileID(i), 1<<20, []byte("x"), core.ShareTag(1), core.Version{Counter: version})
	p, err := s.Append(h, body)
	if err != nil {
		return 0, err
	}
	if p.Loc.Segment == 0 {
		return 0, errNoSegment{}
	}
	return p.Loc.Segment, nil
}

// errNoSegment names an append that returned no location.
type errNoSegment struct{}

func (errNoSegment) Error() string { return "the append landed in no segment" }

// TestGuardRecorded checks the descriptor reference is the segment's storage
// guard under the recorder: an Acquire puts the guard on the caller's books
// until the Release drops it, and Close's drain wait takes the guard for the
// length of its wait — which a probe holding the file index makes report, so
// the check cannot pass over an unrecorded guard.
func TestGuardRecorded(t *testing.T) {
	prof.Watch(t, prof.Options{})

	// The guard until the release.
	seam, _, m := maker(t, 0, noMax)
	c := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(4))
	rec := lockorder.NewRecorder()
	c.SetRecorder(rec)
	id, err := m.Make()
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	ref, err := c.Acquire(id)
	if err != nil {
		t.Fatalf("Acquire of the first segment: %v", err)
	}
	wantHeld := []lockorder.Rank{lockorder.StorageGuard}
	if got := rec.Held(); !slices.Equal(got, wantHeld) {
		t.Fatalf("Held() after Acquire = %v, want %v", got, wantHeld)
	}
	if vs := rec.Violations(); len(vs) != 0 {
		for _, v := range vs {
			t.Errorf("the guard's acquire recorded %s acquiring %s over %v, want none:\n%s", v.Kind, v.Rank, v.Held, v.Stack)
		}
	}
	ref.Release()
	if got := rec.Held(); len(got) != 0 {
		t.Fatalf("Held() after Release = %v, want none", got)
	}

	// The drain wait's guard, reported by the probe: a file index held on
	// this goroutine makes Close's guard acquisition the guard-under-index
	// the recorder exists to report, so a Close that never records its
	// guard cannot pass. A second cache and segment keep the two parts'
	// books apart.
	id2, err := m.Make()
	if err != nil {
		t.Fatalf("Make of the second segment: %v", err)
	}
	probe := lockorder.NewRecorder()
	c2 := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(4))
	c2.SetRecorder(probe)
	armed := make(chan struct{})
	release := make(chan struct{})
	go func() {
		ref, err := c2.Acquire(id2)
		if err != nil {
			t.Errorf("the holding reader's Acquire: %v", err)
			close(armed)
			return
		}
		close(armed)
		<-release
		ref.Release()
		if held := probe.Held(); len(held) != 0 {
			t.Errorf("the holding reader's goroutine holds ranks %v after its release, want none", held)
		}
	}()
	<-armed
	go func() {
		for !segment.EntryGone(c2, id2) {
			runtime.Gosched()
		}
		close(release)
	}()
	probe.Acquire(lockorder.FileIndex, "probe")
	if err := c2.Close(id2); err != nil {
		probe.Release(lockorder.FileIndex, "probe")
		t.Fatalf("Close under the drain: %v", err)
	}
	probe.Release(lockorder.FileIndex, "probe")
	vs := probe.Violations()
	if len(vs) != 1 {
		t.Fatalf("the probe recorded %d violations, want the drain wait's guard once: %+v", len(vs), vs)
	}
	if v := vs[0]; v.Kind != lockorder.GuardUnderIndex || v.Rank != lockorder.StorageGuard {
		t.Fatalf("the drain wait's violation = %s of %s over %v, want guard under index of storage guard", v.Kind, v.Rank, v.Held)
	}
	if held := probe.Held(); len(held) != 0 {
		t.Fatalf("the closing goroutine holds ranks %v after Close, want none", held)
	}
}

// TestCloseRecordsDrainWithoutReaders checks Close(id) records its drain
// whether or not a reader holds the segment: a retirer that calls Close while
// it holds a ranked lock is reported on every run, not only on the runs where
// a reader is in the way.
func TestCloseRecordsDrainWithoutReaders(t *testing.T) {
	prof.Watch(t, prof.Options{})

	cases := []struct {
		name string
		held lockorder.Rank // 0 holds nothing
		want []lockorder.Kind
	}{
		{"nothing held", 0, nil},
		{"capacity held", lockorder.Capacity, []lockorder.Kind{lockorder.GuardWaitUnderLock}},
		{"file index held", lockorder.FileIndex, []lockorder.Kind{lockorder.GuardUnderIndex}},
	}
	for _, tc := range cases {
		for _, acquired := range []bool{false, true} {
			name := tc.name + ", segment never acquired"
			if acquired {
				name = tc.name + ", segment acquired and released"
			}
			t.Run(name, func(t *testing.T) {
				seam, _, m := maker(t, 0, noMax)
				c := segment.NewDescriptors(seam.FS(), format.SegmentName, core.NewDescriptorBudget(4))
				rec := lockorder.NewRecorder()
				c.SetRecorder(rec)
				id, err := m.Make()
				if err != nil {
					t.Fatalf("Make: %v", err)
				}
				if acquired {
					ref, err := c.Acquire(id)
					if err != nil {
						t.Fatalf("Acquire: %v", err)
					}
					ref.Release()
				}
				if tc.held != 0 {
					rec.Acquire(tc.held, "probe")
				}
				err = c.Close(id)
				if tc.held != 0 {
					rec.Release(tc.held, "probe")
				}
				if err != nil {
					t.Fatalf("Close with no reference outstanding: %v", err)
				}
				var got []lockorder.Kind
				for _, v := range rec.Violations() {
					got = append(got, v.Kind)
				}
				if !slices.Equal(got, tc.want) {
					t.Fatalf("violations recorded by Close = %v, want %v", got, tc.want)
				}
				if held := rec.Held(); len(held) != 0 {
					t.Fatalf("Held() after Close = %v, want none", held)
				}
			})
		}
	}
}

// TestPoolCondUnderRecorder runs the pool's settle condition under the
// recorder with the pool's mutex ranked: a WaitSpares that parks on a fill
// the seam holds is woken by the refill's settle and answers the fill it
// parked for, with no violation on any goroutine and nothing held after.
func TestPoolCondUnderRecorder(t *testing.T) {
	prof.Watch(t, prof.Options{})

	seam, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)
	rec := lockorder.NewRecorder()
	p.SetRecorder(rec)

	// The worker is held inside the first pass's make, so the pool stands
	// short and filling and the wait parks on the settle for real.
	h1 := seam.HoldFirst(t, kit.OpCreate, "*")
	p.Start()
	awaitCounters(t, "the worker parked at the held make", p,
		func(c segment.PoolCounters) bool { return c.Spares == 0 })
	answered := make(chan error, 1)
	go func() {
		err := p.WaitSpares(t.Context())
		// On this very goroutine, after the wake: a hold the wait leaked
		// would be on the books of no other goroutine but this one's.
		if held := rec.Held(); len(held) != 0 {
			t.Errorf("the waiting goroutine holds ranks %v after its answer, want none", held)
		}
		answered <- err
	}()
	awaitParked(t, p, 1)
	h1.Release()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("WaitSpares over the held fill answered %v, want the refill", err)
		}
	case <-time.After(10 * time.Second):
		h1.Release()
		t.Fatal("WaitSpares never answered the fill the settle woke it for")
	}
	awaitCounters(t, "the content spare the fill owed", p,
		func(c segment.PoolCounters) bool { return c.Spares == 1 })
	assertNoViolations(t, rec, "the pool's cond run")
	p.Stop()
}

// TestSegmentLocksNameTheirRanks proves the two ranked mutexes of the layer
// carry their ranks: with a higher rank held on this goroutine, a stream read
// takes the append lock and a pool read takes the pool's, and each that goes
// unreported would mean a lock left unranked, invisible to every check above.
func TestSegmentLocksNameTheirRanks(t *testing.T) {
	prof.Watch(t, prof.Options{})

	e := streamOver(t, streamCap, func(e streamEnv) {})
	srec := lockorder.NewRecorder()
	e.s.SetRecorder(srec)
	srec.Acquire(lockorder.StorageGuard, "probe")
	_ = e.s.Room()
	vs := srec.Violations()
	srec.Release(lockorder.StorageGuard, "probe")
	if len(vs) != 1 {
		t.Fatalf("the append probe recorded %d violations, want the append lock once: %+v", len(vs), vs)
	}
	if v := vs[0]; v.Kind != lockorder.OutOfOrder || v.Rank != lockorder.SegmentAppend {
		t.Fatalf("the append probe's violation = %s of %s over %v, want out of order of segment append", v.Kind, v.Rank, v.Held)
	}

	_, _, _, p := poolOf(t, segment.Targets{Content: 1}, noMax, 0, nil)
	prec := lockorder.NewRecorder()
	p.SetRecorder(prec)
	prec.Acquire(lockorder.FileIndex, "probe")
	_ = p.Counters()
	pvs := prec.Violations()
	prec.Release(lockorder.FileIndex, "probe")
	if len(pvs) != 1 {
		t.Fatalf("the pool probe recorded %d violations, want the pool's capacity lock once: %+v", len(pvs), pvs)
	}
	if v := pvs[0]; v.Kind != lockorder.OutOfOrder || v.Rank != lockorder.Capacity {
		t.Fatalf("the pool probe's violation = %s of %s over %v, want out of order of capacity", v.Kind, v.Rank, v.Held)
	}

	// The ledger's capacity-ranked mutex: a counter read under a held
	// segment append rank — which outranks capacity — must report, so a
	// Set whose SetRecorder was dropped or whose Init is missing cannot
	// pass this check.
	setrec := lockorder.NewRecorder()
	e.set.SetRecorder(setrec)
	setrec.Acquire(lockorder.SegmentAppend, "probe")
	_ = e.set.Counters()
	svs := setrec.Violations()
	setrec.Release(lockorder.SegmentAppend, "probe")
	if len(svs) != 1 {
		t.Fatalf("the ledger probe recorded %d violations, want the ledger's capacity lock once: %+v", len(svs), svs)
	}
	if v := svs[0]; v.Kind != lockorder.OutOfOrder || v.Rank != lockorder.Capacity {
		t.Fatalf("the ledger probe's violation = %s of %s over %v, want out of order of capacity", v.Kind, v.Rank, v.Held)
	}
}

// TestStreamRollsTwiceUnderRecorder checks the case the ref exemption was
// made for on its own: one stream, one goroutine, appends large enough to
// roll, twice over. The successor's ref is acquired while the predecessor's
// is held — two guard references on one goroutine, exempt — and the run
// must record no violation from the exemption itself and leave the
// goroutine holding nothing once the stream is closed.
func TestStreamRollsTwiceUnderRecorder(t *testing.T) {
	prof.Watch(t, prof.Options{})
	watchdog(t, 60*time.Second)

	e := recordedStreams(t)
	rec := e.rec
	s := e.ss.Stream(0)
	awaitCounters(t, "the two content spares ready", e.pool,
		func(c segment.PoolCounters) bool { return c.Spares == 2 })

	// The rolls' books: each successor's ref is pinned while the
	// predecessor's still is, and the pinned pair must record nothing.
	// reported counts the violations already failed for, so the first one
	// names the append that caused it and none is printed twice.
	payload := make([]byte, segCap/8)
	distinct := make(map[core.SegmentID]struct{})
	reported := 0
	for v := range 32 {
		h, body := streamWriteHeader(streamFileID(0), int64(v)*int64(segCap), payload, core.ShareTag(1), core.Version{Counter: uint64(v + 1)})
		p, err := s.Append(h, body)
		if err != nil {
			t.Fatalf("append %d: %v", v, err)
		}
		distinct[p.Loc.Segment] = struct{}{}
		if now := len(rec.Violations()); reported < now {
			v0 := rec.Violations()[now-1]
			reported = now
			t.Errorf("append %d recorded %s acquiring %s over %v\n%s", v, v0.Kind, v0.Rank, v0.Held, v0.Stack)
		}
	}
	// Thirty-two appends of an eighth of the cap roll the stream several
	// times over — at least two segments must have been landed in for the
	// check to have exercised a roll at all.
	if len(distinct) < 2 {
		t.Fatalf("the appends landed in %d distinct segments, want at least 2: no roll ran for the exemption to matter", len(distinct))
	}
	// The close is what clears the appender's last pin off its books, so
	// the hold check runs after it, never before.
	if err := e.ss.Close(); err != nil {
		t.Fatalf("the streams' close: %v", err)
	}
	if held := rec.Held(); len(held) != 0 {
		t.Fatalf("the appender holds ranks %v after the close, want none", held)
	}
	// The zero above would pass over a recorder that was never wired: the
	// same recorder is made to prove it saw this fixture's locks, one
	// probe read per ranked lock, each that goes unreported meaning an
	// unwired Init.
	probe := lockorder.StorageGuard
	rec.Acquire(probe, "probe")
	_ = s.Room()
	_ = e.pool.Counters()
	vs := rec.Violations()
	rec.Release(probe, "probe")
	if got := len(vs) - reported; got < 2 {
		t.Fatalf("the probes recorded %d new violations, want at least 2 (the append lock once, the pool's and the ledger's capacity lock): %+v", got, vs[reported:])
	}
	if held := rec.Held(); len(held) != 0 {
		t.Fatalf("the appender holds ranks %v after the probes' release, want none", held)
	}
}
