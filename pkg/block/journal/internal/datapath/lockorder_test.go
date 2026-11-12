package datapath

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
)

// recordedWatchdog is the one generous backstop every recorded wait shares:
// it detects a hang — a call that waited on something it must not — and no
// test orders its steps by it. It is real time, not the test clock's, for a
// call blocked on a lock waits on no timer.
const recordedWatchdog = 60 * time.Second

// recordedDump writes every goroutine's stack to the test's log and, so it
// is visible without -v, to stderr.
func recordedDump(t *testing.T, what string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	t.Logf("%s did not return within %s; every goroutine's stack follows:\n%s", what, recordedWatchdog, buf)
	fmt.Fprintf(os.Stderr, "%s did not return within %s; every goroutine's stack follows:\n%s", what, recordedWatchdog, buf)
}

// recordedJoin waits for one answered call on ch, failing the test naming
// what when nothing arrives within recordedWatchdog. On timeout it dumps
// every goroutine's stack and abandons the core — a Close waits for the
// calls in flight, so the cleanup would hang on the same blocked call the
// watchdog already failed the test for, the same discipline the public
// concurrency test's abandon decision names.
func recordedJoin[T any](t *testing.T, c *Core, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(recordedWatchdog):
		recordedDump(t, what)
		abandonRecorded(t, c)
		t.Fatalf("%s did not return within %s: it waited on something it must not", what, recordedWatchdog)
		var zero T
		return zero
	}
}

// recordedAwait waits for a tell channel to close, failing and abandoning
// on timeout the way recordedJoin does.
func recordedAwait(t *testing.T, c *Core, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(recordedWatchdog):
		recordedDump(t, what)
		abandonRecorded(t, c)
		t.Fatalf("%s did not reach its mark within %s", what, recordedWatchdog)
	}
}

// assertNoRecordedViolations fails the test naming where when the recorder
// holds any violation, deduplicated by stack so one report does not print
// twice for one rule broken on two goroutines at the same place.
func assertNoRecordedViolations(t *testing.T, rec *lockorder.Recorder, where string) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, v := range rec.Violations() {
		if _, dup := seen[v.Stack]; dup {
			continue
		}
		seen[v.Stack] = struct{}{}
		t.Errorf("%s: recorder violation: %s acquiring %s over %v\n%s", where, v.Kind, v.Rank, v.Held, v.Stack)
	}
}

// assertHeldNone fails the test when the calling goroutine still holds any
// rank — a hold the test leaked would be on the books of this goroutine
// alone.
func assertHeldNone(t *testing.T, rec *lockorder.Recorder, where string) {
	t.Helper()
	if held := rec.Held(); len(held) != 0 {
		t.Errorf("%s: the goroutine holds ranks %v, want none", where, held)
	}
}

// TestRecorder_ReadReleasesIndexBeforeSegment checks the read hands the index
// back before it takes any segment reference: inside the beforeRead seam,
// which runs after the span is resolved and the index released and before
// the first record is read from storage, the recorder answers that the
// calling goroutine holds nothing at all — so a read that took its reference
// under the file index, the order that deadlocks against reclamation, would
// fail here and not only at the acquire.
func TestRecorder_ReadReleasesIndexBeforeSegment(t *testing.T) {
	// The seam runs on the reading goroutine; it is armed once the fixture
	// hands back the recorder, so the fixture's own publish, which reads
	// nothing, never fires it and only the test's read reports.
	var probe *lockorder.Recorder
	heldInside := make(chan []lockorder.Rank, 1)
	options := func(o *coreOptions) {
		o.beforeRead = func() {
			if probe != nil {
				p := probe
				probe = nil
				heldInside <- p.Held()
			}
		}
	}
	c, rec, shares := newRecordedCoreOpts(t, options)
	probe = rec
	s := shares[0]
	// A fresh file on the fixture's first stream, so it belongs to share 1's
	// own stream and the read runs on the stream the fixture already wrote.
	stream0 := c.streams.IndexFor(fid(1))
	f := streamFileID(c, stream0)
	if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("record"), 1); err != nil {
		t.Fatalf("WriteAt of the read file: %v", err)
	}
	if _, _, _, _, err := c.ReadAt(context.Background(), s, f, 0, make([]byte, 6)); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	// Anything held at the seam names a lock the read carried past its
	// resolve.
	held := recordedJoin(t, c, "the beforeRead seam's report", heldInside)
	if len(held) != 0 {
		t.Fatalf("ranks held at the beforeRead seam = %v, want none: the read still held a lock past its resolve", held)
	}
	assertNoRecordedViolations(t, rec, "the recorded read")
}

// TestRecorder_RemovalWaitReleasesAppend checks every removal reaches its
// durability wait with the file's append domain released: the wait consults
// the recorder under the label the removal's own assertion carries, so the
// test cannot pass with the assertion dropped, and a removal that ran the
// wait under the domain — the deadlock with the worker that resolves the
// file's window under that same domain — hangs on the worker, which the
// watchdog answers with every goroutine's stack and a failed test rather
// than the run waiting out the suite's timeout. The removal runs on its
// own goroutine behind the watchdog for exactly that.
func TestRecorder_RemovalWaitReleasesAppend(t *testing.T) {
	c, rec, shares := newRecordedCore(t)
	s := shares[0]
	doomed := fid(5)
	if _, err := c.WriteAt(context.Background(), s, doomed, 0, []byte("doomed"), 1); err != nil {
		t.Fatalf("WriteAt of the removed file: %v", err)
	}
	truncated := make(chan error, 1)
	go func() {
		_, err := c.Truncate(context.Background(), s, doomed, 2)
		truncated <- err
	}()
	if err := recordedJoin(t, c, "the recorded truncate", truncated); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if got := rec.Checks("removal wait"); got < 1 {
		t.Fatalf("removal wait checks = %d, want >= 1: the removal never reached its wait's assertion", got)
	}
	assertNoRecordedViolations(t, rec, "the recorded removal")
}

// TestRecorderWiring_ReadUnderIndexReported proves the wiring end to end and
// that the recorder can fail through the real core: a ranked file index
// mutex built with the core's own recorder is held on the test goroutine
// while the same goroutine reads a record through the core. The read's I/O
// reaches the same recorder through the FS wrapper and is reported as I/O
// under the index, and the descriptor reference the read takes is reported
// as a guard under the index — so a dropped FS wiring, a dropped descriptor
// guard recording or a recorder the core never installed each fail here.
func TestRecorderWiring_ReadUnderIndexReported(t *testing.T) {
	c, rec, shares := newRecordedCore(t)
	s := shares[0]
	f := streamFileID(c, c.streams.IndexFor(fid(1)))
	if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("record"), 1); err != nil {
		t.Fatalf("WriteAt of the read file: %v", err)
	}
	before := len(rec.Violations())

	// The ranked file index mutex: built with the core's recorder, so its
	// hold sits on this goroutine's books while the read runs on the same
	// goroutine and everything the read reaches reports against it.
	var ix lockorder.Mutex
	ix.Init(lockorder.FileIndex, rec)
	ix.Lock()
	_, _, _, _, err := c.ReadAt(context.Background(), s, f, 0, make([]byte, 6))
	ix.Unlock()
	if err != nil {
		t.Fatalf("ReadAt under the held index: %v", err)
	}

	// Every lock the read takes reports out of order or of the same domain
	// against the held index; the two the site means to prove are the I/O
	// rule, which only the FS wrapper can fire, and the guard rule, which
	// only the descriptor reference can. The rest are the ordinary
	// out-of-order reports every ranked acquisition under the probe makes.
	vs := rec.Violations()
	if len(vs) <= before {
		t.Fatalf("violations = %d after a read under a held file index, want more than the %d before it", len(vs), before)
	}
	var sawIO, sawGuard bool
	for _, v := range vs[before:] {
		switch v.Kind {
		case lockorder.IOUnderIndex:
			sawIO = true
		case lockorder.GuardUnderIndex:
			sawGuard = true
		case lockorder.OutOfOrder, lockorder.SameDomain:
		default:
			t.Errorf("read under a held file index reported %s of %s over %v, an unexpected rule:\n%s", v.Kind, v.Rank, v.Held, v.Stack)
		}
	}
	if !sawIO {
		t.Fatalf("no I/O-under-index report after a read under a held file index: the read's I/O never reached the recorder through the FS wrapper:\n%+v", vs[before:])
	}
	if !sawGuard {
		t.Fatalf("no guard-under-index report after a read under a held file index: the descriptor reference the read takes was not recorded:\n%+v", vs[before:])
	}
}

// TestRecorder_ReadOfAIgnoresWriteOfB checks a read of one file never waits
// on a write to another: B's write parks inside the afterAppend seam holding
// B's file append domain, and while it is parked a read of A completes.
// A sits on B's own stream, so the read crosses the stream whose append
// lock a record write of B would hold — the seam, however, runs after
// Stream.Append returned, and Append holds the stream's append lock only
// across the record's reservation and completion, never across the seam or
// the I/O, so a parked writer holds the file's append domain alone and the
// stream's lock stays free. A read that took any lock another file's
// parked write holds — B's file domain, the shard's map lock — would block
// past the watchdog. This is the data-path half of the cross-file promise;
// a read naming B's own file would rightly wait on B's file domain, the
// per-file serialisation the domain exists for, and is not run here.
func TestRecorder_ReadOfAIgnoresWriteOfB(t *testing.T) {
	parked := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	stopB := func() { releaseOnce.Do(func() { close(release) }) }
	var writerParked atomic.Bool
	options := func(o *coreOptions) {
		o.afterAppend = func() {
			// The fixture's own publishes run the seam too; only the
			// test's own write parks, and only once.
			if writerParked.CompareAndSwap(true, false) {
				close(parked)
				<-release
			}
		}
	}
	c, rec, shares := newRecordedCoreOpts(t, options)
	// A Fatalf past this point must not leave the writer parked for the
	// core's cleanup to wait on.
	t.Cleanup(stopB)
	s0, s1 := shares[0], shares[1]
	b := fileOnOtherStream(c, fid(1))
	a := streamFileID(c, c.streams.IndexFor(b))
	if _, err := c.WriteAt(context.Background(), s1, b, 0, []byte("B-first"), 1); err != nil {
		t.Fatalf("B's first write: %v", err)
	}
	if _, err := c.WriteAt(context.Background(), s0, a, 0, []byte("A-first"), 1); err != nil {
		t.Fatalf("A's first write: %v", err)
	}

	// B's second write parks in the seam with B's append domain held.
	wrote := make(chan error, 1)
	writerParked.Store(true)
	go func() {
		_, err := c.WriteAt(context.Background(), s1, b, 8, []byte("B-second"), 1)
		wrote <- err
	}()
	recordedAwait(t, c, "B's write reaching the afterAppend seam", parked)

	// A's read names a file on B's own stream and must finish while the
	// write is still parked.
	servedA := make(chan error, 1)
	go func() {
		_, _, _, _, err := c.ReadAt(context.Background(), s0, a, 0, make([]byte, 7))
		servedA <- err
	}()
	if err := recordedJoin(t, c, "a read of A beside B's parked write", servedA); err != nil {
		t.Fatalf("ReadAt of A beside B's parked write: %v", err)
	}
	stopB()
	if err := recordedJoin(t, c, "B's parked write", wrote); err != nil {
		t.Fatalf("B's parked write: %v", err)
	}
	assertNoRecordedViolations(t, rec, "the recorded read-beside-write")
}

// TestRecorder_RemovalDuringHeldCallback pins the offload-callback
// assertion's shape, and that a removal running while a stand-in callback
// blocks still reaches its own recorded waits: the stand-in goroutine
// enters the callback's assertion — which counts its check and records a
// violation only when its own goroutine holds a ranked lock — and blocks
// where the callback would run its upload. While it blocks, a truncate and
// a delete of the same file both complete and their own wait assertions
// run. The test pins the assertion's count and that the removals are not
// parked by the callback's stand-in; the real callback is the reclaim
// epic's, and the lock-exclusion half of the rule is what the assertion
// this test counts is for.
func TestRecorder_RemovalDuringHeldCallback(t *testing.T) {
	c, rec, shares := newRecordedCore(t)
	s := shares[0]
	f := fid(40)
	if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("held"), 1); err != nil {
		t.Fatalf("WriteAt of the removed file: %v", err)
	}
	before := rec.Violations()

	inCallback := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	go func() {
		rec.AssertNoneHeld("offload callback")
		close(inCallback)
		<-release
	}()
	recordedAwait(t, c, "the stand-in callback entering its assertion", inCallback)

	ctx := context.Background()
	truncated := make(chan error, 1)
	go func() {
		_, err := c.Truncate(ctx, s, f, 2)
		truncated <- err
	}()
	if err := recordedJoin(t, c, "the truncate beside the held callback", truncated); err != nil {
		t.Fatalf("Truncate beside a held callback: %v", err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := c.Delete(ctx, s, f)
		deleted <- err
	}()
	if err := recordedJoin(t, c, "the delete beside the held callback", deleted); err != nil {
		t.Fatalf("Delete beside a held callback: %v", err)
	}
	releaseOnce.Do(func() { close(release) })

	if got := rec.Checks("offload callback"); got != 1 {
		t.Fatalf("offload callback checks = %d, want 1", got)
	}
	if got := rec.Checks("removal wait"); got < 2 {
		t.Fatalf("removal wait checks = %d after a truncate and a delete beside the held callback, want >= 2: the removals never reached their wait's assertion", got)
	}
	if got := rec.Violations(); len(got) != len(before) {
		t.Fatalf("violations beside a held callback = %d, want the %d before it\n%+v", len(got), len(before), got[len(before):])
	}
}

// TestRecorder_TakeRunsUnderAppendOnly checks the admission runs under the
// file append domain alone: the core takes a tagged write's domain, then
// the admission decides under its mutex, and inside that decision section
// the calling goroutine holds exactly the file append and capacity ranks. A
// Take that ran with the file index held — a capacity lock taken under
// it, the order RFC 1 §8.3 forbids — or under any lock besides the write's
// own domain, fails the exact-equality below. The hook is set through the
// core's options, so it is in place before the core starts; it answers only
// the test's own write, once the fixture's publishes have finished and the
// recorder is armed, and the test waits on the answer it sends.
func TestRecorder_TakeRunsUnderAppendOnly(t *testing.T) {
	// The fixture's own publishes run Takes before the test's write: the
	// hook answers nothing until the recorder is armed, after the build.
	var armed atomic.Pointer[lockorder.Recorder]
	var took atomic.Bool
	heldInside := make(chan []lockorder.Rank, 1)
	c, rec, shares := newRecordedCoreOpts(t, func(o *coreOptions) {
		o.takeHook = func() {
			if r := armed.Load(); r != nil && took.CompareAndSwap(false, true) {
				heldInside <- r.Held()
			}
		}
	})
	armed.Store(rec)
	s := shares[0]
	f := streamFileID(c, c.streams.IndexFor(fid(1)))
	wrote := make(chan error, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), s, f, 0, []byte("admitted"), 1)
		wrote <- err
	}()
	if err := recordedJoin(t, c, "the write under the take hook", wrote); err != nil {
		t.Fatalf("WriteAt under the take hook: %v", err)
	}
	held := recordedJoin(t, c, "the admission's decision-section report", heldInside)
	if want := []lockorder.Rank{lockorder.FileAppend, lockorder.Capacity}; !slices.Equal(held, want) {
		t.Fatalf("ranks held inside the admission's decision section = %v, want %v: the admission ran under a lock besides the write's own append domain", held, want)
	}
	assertNoRecordedViolations(t, rec, "the recorded admission")
}

// TestRecorder_RefusedWriteHoldsNothing checks a write the admission refuses
// holds no lock after it returns: the share's limit is drawn to the
// fixture's spend plus one write's record bytes, so the first write of the
// pair below lands and every later one is refused with a NoSpaceError. A
// reader loops over the same file while the refusals run, started before
// them and stopped only after they returned, so it crosses each refusal's
// admission: a refusal that held the file's index or append domain behind
// itself would stop a read past the watchdog. Each refused write's own
// goroutine — where a leaked hold would sit on the books of no other
// goroutine — must come back holding nothing.
func TestRecorder_RefusedWriteHoldsNothing(t *testing.T) {
	c, rec, shares := newRecordedCore(t)
	s := shares[0]
	f := streamFileID(c, c.streams.IndexFor(fid(1)))
	const each = 4096
	// A share limit of the fixture's own spend on the tag plus one write's
	// record bytes: the first write of the pair below lands, the second is
	// refused naming the share.
	limit := recordBytes(each) + c.set.ShareBytes(s.Tag)
	limited, err := c.Share(s.Tag, limit, core.Version{})
	if err != nil {
		t.Fatalf("Share(%d, %d): %v", s.Tag, limit, err)
	}
	if _, err := c.WriteAt(context.Background(), limited, f, 0, make([]byte, each), 1); err != nil {
		t.Fatalf("the admitted first write: %v", err)
	}

	// stopReads ends the reader loop below; the cleanup closes it so a
	// Fatalf path cannot strand the reader for the core's cleanup.
	stopReads := make(chan struct{})
	var stopOnce sync.Once
	stopTheReads := func() { stopOnce.Do(func() { close(stopReads) }) }
	t.Cleanup(stopTheReads)

	// The reader loops over the same file, counting every read it
	// completed: the refusals hold no lock the read needs — the file's
	// index or append domain among them — so reads keep answering beside
	// them. Its end closes readerDone however it returns, so an error
	// inside the loop fails the join promptly and not at the watchdog.
	var reads atomic.Int64
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stopReads:
				return
			default:
			}
			if err := func() error {
				_, _, _, _, err := c.ReadAt(context.Background(), limited, f, 0, make([]byte, 4))
				if err != nil {
					return err
				}
				reads.Add(1)
				return nil
			}(); err != nil {
				t.Errorf("ReadAt beside the refused writes: %v", err)
				return
			}
		}
	}()

	// The reader must have completed at least one read before the first
	// refusal: the test's promise is about the refusals' behaviour, not
	// about a reader that never ran.
	waitForReads := func(stage string, atLeast int64) {
		t.Helper()
		// The wait polls the counter on a short tick and bounds the total
		// by the watchdog: a reader parked on a lock a refusal holds would
		// sit between ticks until the bound, then dump every stack.
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for deadline := time.Now().Add(recordedWatchdog); ; {
			select {
			case <-readerDone:
				t.Fatalf("the reader returned at %s after %d completed reads, want >= %d", stage, reads.Load(), atLeast)
			case <-tick.C:
				if reads.Load() >= atLeast {
					return
				}
				if time.Now().After(deadline) {
					recordedDump(t, "a read beside the refused writes at "+stage)
					abandonRecorded(t, c)
					t.Fatalf("no read completed at %s within %s: a refusal the run never reached was holding a lock the read needs", stage, recordedWatchdog)
				}
			}
		}
	}
	waitForReads("the first refusal", 1)

	// Several refused writes, each in turn: every one must answer naming
	// the share, and the goroutine it ran on must hold nothing
	// once it has. Each refusal runs on its own goroutine behind the
	// watchdog, so a regression answers as a stack dump, not as the run
	// waiting out the suite's timeout. A write that is not refused fails
	// with its own answer: the typed result carries the error through, so
	// an unexpected success is an assertion failure and not a crash.
	for attempt := 1; attempt <= 3; attempt++ {
		refused := make(chan refusal, 1)
		go func() {
			_, err := c.WriteAt(context.Background(), limited, f, int64(attempt)*each, make([]byte, each), 1)
			refused <- refusal{err: err, held: rec.Held()}
		}()
		out := recordedJoin(t, c, "the refused write", refused)
		var ns *core.NoSpaceError
		if !errors.As(out.err, &ns) || ns.Limit != core.LimitShare {
			t.Fatalf("refused write %d got error %v, want a NoSpaceError naming the share's limit", attempt, out.err)
		}
		if len(out.held) != 0 {
			t.Fatalf("refused write %d's goroutine holds ranks %v after its refusal, want none", attempt, out.held)
		}
	}
	// At least one further read must complete past the last refusal
	// before the reader is stopped: the refusals left nothing behind them.
	waitForReads("the joined refusals", reads.Load()+1)
	stopTheReads()
	recordedAwait(t, c, "the reader beside the refused writes", readerDone)
	assertNoRecordedViolations(t, rec, "the recorded refusals")
}

// refusal is one refused write's answer: the error it returned and the
// ranks its goroutine still held, typed so an unexpected success carries a
// nil error to the assertion instead of crashing the run.
type refusal struct {
	err  error
	held []lockorder.Rank
}

// TestRecorder_FileMapHeldOnlyAcrossALookup checks, on the data path, that
// nothing is acquired while the file map is held. The exhaustive proof —
// the map's lock taken for finding, adding or removing alone — is the
// index-layer recorded run, TestIndexUnderRecorder, whose probe holds a
// higher rank and drives each of the index's own operations beneath it;
// this is its data-path counterpart, in two halves. First a probe: with a
// segment append rank held on a fresh goroutine, a write that creates the
// file's entry and a removal that drops what it holds run through the
// core, and every index lock they take reports out of order against it —
// the file map's rank and the file index's among them, so a recorder the
// index never saw, or a data path that bypasses the index's locks, fails
// this half. Then the same operations with no probe held record nothing,
// the no-acquire-under-map half this side can see.
func TestRecorder_FileMapHeldOnlyAcrossALookup(t *testing.T) {
	seededFiles := func(t *testing.T, c *Core, s *Share, files ...core.FileID) {
		t.Helper()
		for _, f := range files {
			if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("seeded"), 1); err != nil {
				t.Fatalf("the seed write of %v: %v", f, err)
			}
		}
	}
	t.Run("the probe half", func(t *testing.T) {
		c, rec, shares := newRecordedCore(t)
		s := shares[0]
		stream := c.streams.IndexFor(fid(1))
		fresh := streamFileID(c, stream)
		doomed := otherFreshFileOn(c, stream, fresh)
		// Only the doomed file is seeded: the probe's own write to fresh
		// creates that file's entry under the held rank, which is the
		// map-add acquisition this half exists to see.
		seededFiles(t, c, s, doomed)
		before := len(rec.Violations())
		done := make(chan struct{})
		go func() {
			defer close(done)
			rec.Acquire(lockorder.SegmentAppend, probeLock{component: "file map probe"})
			defer rec.Release(probeLock{component: "file map probe"})
			if _, err := c.WriteAt(context.Background(), s, fresh, 4096, []byte("entry"), 1); err != nil {
				t.Errorf("the probe's write of the fresh file: %v", err)
				return
			}
			if _, err := c.Truncate(context.Background(), s, doomed, 0); err != nil {
				t.Errorf("the probe's truncate of the doomed file: %v", err)
			}
		}()
		recordedAwait(t, c, "the file map probe's operations", done)
		sawMap, sawIndex := false, false
		for _, v := range rec.Violations()[before:] {
			// Every ranked lock the data path takes reports against the
			// probe — the file append and capacity ones among them — and
			// each report is the recorder seeing that lock; the two this
			// half exists for are the file map's and the file index's.
			switch v.Rank {
			case lockorder.FileMap:
				sawMap = true
			case lockorder.FileIndex:
				sawIndex = true
			}
		}
		if !sawMap || !sawIndex {
			t.Fatalf("the probe reported no file map or file index acquisition through the data path: the index's locks were not recorded")
		}
	})
	t.Run("the no-probe half", func(t *testing.T) {
		c, rec, shares := newRecordedCore(t)
		s := shares[0]
		stream := c.streams.IndexFor(fid(1))
		fresh := streamFileID(c, stream)
		doomed := otherFreshFileOn(c, stream, fresh)
		seededFiles(t, c, s, fresh, doomed)
		if _, err := c.WriteAt(context.Background(), s, fresh, 4096, []byte("entry"), 1); err != nil {
			t.Fatalf("the entry-creating write: %v", err)
		}
		if _, err := c.Truncate(context.Background(), s, doomed, 0); err != nil {
			t.Fatalf("the truncate of the doomed file: %v", err)
		}
		assertNoRecordedViolations(t, rec, "the recorded entry-creating write and removal")
	})
}

// recordedFiles returns the file set the recorded no-deadlock run works
// over: four fresh ids, one per (stream, share) pair — files 0 and 2 run
// through share 1, files 1 and 3 through share 2, and each share names one
// file on each stream. Each slot searches from its own base past every id
// the other helpers hand out, so no two picks collide and none meets an
// entry the fixture seeded; the four are asserted distinct before the run
// uses them.
func recordedFiles(c *Core) [4]core.FileID {
	var out [4]core.FileID
	s0 := c.streams.IndexFor(fid(1))
	s1 := c.streams.IndexFor(fileOnOtherStream(c, fid(1)))
	for i := range out {
		stream := s0
		if i%2 == 1 {
			stream = s1
		}
		for k := uint64(1000 + 4*i); ; k++ {
			if id := fid(k); c.streams.IndexFor(id) == stream {
				out[i] = id
				break
			}
		}
	}
	for i := range out {
		for j := range i {
			if out[j] == out[i] {
				panic("recordedFiles picked colliding ids")
			}
		}
	}
	return out
}

// TestRFC1B_NoDeadlockRecorded runs the data path's whole concurrent fan
// under the lock-order recorder: writers, readers, fills and removals at
// once, over four files spread over both shares and both streams, for a
// fixed round count. The epic's reclamation-side calls are outside this
// package and are not run. The run must record no violation, and every
// operation kind must have actually completed something — a write that
// landed, a read that answered, a fill that filled, a removal that
// carried — so the run cannot pass hollow, with every call finding nothing
// to do while the others ran. The workers join through a watchdog: a run
// blocked past the bound dumps every goroutine's stack to the log and to
// stderr, fails the test, and the core is abandoned rather than closed,
// for its Close would wait on the very call the stack names — the
// discipline of the public TestRFC1B_NoDeadlock. Each worker defers its
// own held-set check at its end; the test goroutine's final check is the
// same assertion on the goroutine that seeded the files.
func TestRFC1B_NoDeadlockRecorded(t *testing.T) {
	const rounds = 100
	const chunk = 4096

	c, rec, shares := newRecordedCore(t)
	ctx := context.Background()
	files := recordedFiles(c)
	shareOf := func(i int) *Share { return shares[i%2] }
	// Each file is published once up front, through the share that owns
	// it: the first write of a file claims it for the handle that ran it,
	// so a fixed owner per file keeps every later operation on the owning
	// share.
	for i := range files {
		if _, err := c.WriteAt(ctx, shareOf(i), files[i], 0, make([]byte, 512), 1); err != nil {
			t.Fatalf("the seed write of file %d: %v", i, err)
		}
	}

	// did counts, per kind, the calls that completed: the ones that
	// changed something and the ones a limit refused naming a real state —
	// a NoSpace refusal is a completed answer, not a hollow call. Fills
	// refused for staleness are counted apart from the ones that landed.
	var did struct {
		writes, reads, fills  atomic.Int64
		truncates, deallocs   atomic.Int64
		deletes, refusedFills atomic.Int64
	}
	// landed removals of every kind, for the wait-count assertion below.
	landedRemovals := func() int64 {
		return did.truncates.Load() + did.deallocs.Load() + did.deletes.Load()
	}
	reportErr := func(name string, err error) {
		if err == nil {
			return
		}
		t.Errorf("%s: %v", name, err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	// Each worker's source is seeded from a fixed constant and its index, so
	// a failure replays with the same choices.
	const seedBase = 0x8115eed
	t.Logf("workers' random seed base %#x", seedBase)
	var workers uint64
	worker := func(name string, fn func(r *rand.Rand)) {
		wg.Add(1)
		workers++
		stream := workers
		go func() {
			defer wg.Done()
			defer func() { assertHeldNone(t, rec, name) }()
			r := rand.New(rand.NewPCG(seedBase, stream))
			<-start
			for range rounds {
				fn(r)
			}
		}()
	}
	// pattern is one writer's payload, distinct per writer index.
	pattern := func(seed byte, n int) []byte {
		p := make([]byte, n)
		for i := range p {
			p[i] = seed + byte(i)
		}
		return p
	}

	// The writers cycle over all four files, so both shares and both
	// streams see concurrent appends beside the readers and the removals.
	worker("writer", func(r *rand.Rand) {
		i := int(r.IntN(4))
		_, err := c.WriteAt(ctx, shareOf(i), files[i], int64(r.IntN(16))*chunk, pattern(byte(i), chunk), 1)
		var ns *core.NoSpaceError
		switch {
		case err == nil:
			did.writes.Add(1)
		case errors.As(err, &ns):
			// tolerated: the share or journal limit naming a real state
		default:
			reportErr("WriteAt", err)
		}
	})
	worker("writer", func(r *rand.Rand) {
		i := int(r.IntN(4))
		_, err := c.WriteAt(ctx, shareOf(i), files[i], int64(r.IntN(16))*chunk, pattern(byte(i+8), chunk), 1)
		var ns *core.NoSpaceError
		switch {
		case err == nil:
			did.writes.Add(1)
		case errors.As(err, &ns):
			// tolerated: the share or journal limit naming a real state
		default:
			reportErr("WriteAt", err)
		}
	})
	// The readers answer whatever the files hold — served and missing
	// alike are completed reads, and an error past a NoSpace refusal is
	// the run's to report.
	worker("reader", func(r *rand.Rand) {
		i := int(r.IntN(4))
		_, _, _, _, err := c.ReadAt(ctx, shareOf(i), files[i], int64(r.IntN(16))*chunk, make([]byte, chunk))
		reportErr("ReadAt", err)
		did.reads.Add(1)
	})
	worker("reader", func(r *rand.Rand) {
		i := int(r.IntN(4))
		_, _, _, _, err := c.ReadAt(ctx, shareOf(i), files[i], 0, make([]byte, 4*chunk))
		reportErr("ReadAt", err)
		did.reads.Add(1)
	})
	// The filler retries, with a fresh drawn version each try, until one
	// of its fills lands or its tries run out: a fill refused for
	// staleness races a writer, its refusal is tolerated and counted
	// apart, and the end of the run can still require a landed fill and
	// name how many were refused instead. A Draw that fails ends the
	// round and is reported: the limit stays far above the run's draws,
	// so a refusal there is the surprise the error path names.
	worker("filler", func(r *rand.Rand) {
		for tries := 0; tries < 8; tries++ {
			i := int(r.IntN(4))
			s, f := shareOf(i), files[i]
			v, err := c.Draw()
			if err != nil {
				reportErr("Draw", err)
				return
			}
			p := pattern(byte(i+16), chunk)
			asOf := c.Index().AsOf(f)
			err = c.Fill(ctx, s, f, int64(r.IntN(16))*chunk, p, asOf, v)
			switch {
			case err == nil:
				did.fills.Add(1)
				return
			case errors.Is(err, core.ErrStaleFill):
				did.refusedFills.Add(1) // written since the asOf was read: retry
			default:
				reportErr("Fill", err)
				return
			}
		}
	})
	// The removals cycle truncate, deallocate and delete over the files,
	// each kind counted in its own landed counter: the run passes only
	// when every kind actually changed something, so a remover whose dice
	// never picked one kind cannot pass hollow. Their records draw on the
	// reserved headroom, which no content write of this run touches, so
	// a refused removal would name a real state; none should be, and one
	// that is lands in reportErr like any other surprise.
	worker("remover", func(r *rand.Rand) {
		i := int(r.IntN(4))
		s, f := shareOf(i), files[i]
		var err error
		var landed *atomic.Int64
		switch r.IntN(3) {
		case 0:
			_, err = c.Truncate(ctx, s, f, int64(r.IntN(8))*chunk)
			landed = &did.truncates
		case 1:
			_, err = c.Deallocate(ctx, s, f, core.Extent{Off: int64(r.IntN(8)) * chunk, Len: chunk})
			landed = &did.deallocs
		default:
			_, err = c.Delete(ctx, s, f)
			landed = &did.deletes
		}
		if err != nil {
			reportErr("removal", err)
			return
		}
		landed.Add(1)
	})

	close(start)
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(recordedWatchdog):
		recordedDump(t, "the recorded no-deadlock run's workers")
		abandonRecorded(t, c)
		t.Fatalf("the recorded no-deadlock run did not finish within %s", recordedWatchdog)
	}

	// Every kind must have completed something, and a fill must have
	// landed: a run whose every call found nothing to do, or whose filler
	// never got past a racing writer, cannot pass.
	for kind, n := range map[string]int64{
		"a landed write":      did.writes.Load(),
		"a completed read":    did.reads.Load(),
		"a landed fill":       did.fills.Load(),
		"a landed truncate":   did.truncates.Load(),
		"a landed deallocate": did.deallocs.Load(),
		"a landed delete":     did.deletes.Load(),
	} {
		if n <= 0 {
			t.Errorf("no %s completed in the run of %d rounds per worker: the run would pass hollow", kind, rounds)
		}
	}
	// Every landed removal reaches the recorded wait, and nothing else in
	// the run runs a removal, so the assertion's count answers them.
	if got, want := int64(rec.Checks("removal wait")), landedRemovals(); got < want {
		t.Fatalf("removal wait checks = %d, want >= the %d removals that landed: a removal never reached its wait's assertion", got, want)
	}
	assertNoRecordedViolations(t, rec, "the recorded no-deadlock run")
	// The test goroutine's own books: it seeded the files, so this is the
	// same check the workers deferred; the per-worker ones are the real
	// assertions, this one closes the set.
	assertHeldNone(t, rec, "the test goroutine")
}
