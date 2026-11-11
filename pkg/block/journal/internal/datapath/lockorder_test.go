package datapath

import (
	"context"
	"errors"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/lockorder"
)

// recordedWatchdog is how long a recorded run waits on one call before it
// fails the test with every goroutine's stack: it detects a hang, and no
// test orders its steps by it. It is real time, not the test clock's, for a
// call blocked on a lock waits on no timer.
const recordedWatchdog = 60 * time.Second

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
// rank beyond the storage guard pins a stream's appender leaves — a hold the
// test leaked would be on the books of this goroutine alone.
func assertHeldNone(t *testing.T, rec *lockorder.Recorder, where string) {
	t.Helper()
	for _, h := range rec.Held() {
		if h != lockorder.StorageGuard {
			t.Errorf("%s: the goroutine holds ranks %v, want none", where, rec.Held())
			return
		}
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
	// The stream appender's storage guard pins ride this goroutine's books
	// from the writes it made and order nothing; anything else held at the
	// seam names a lock the read carried past its resolve.
	if got := withoutPins(<-heldInside); len(got) != 0 {
		t.Fatalf("ranks held at the beforeRead seam = %v, want only the appender's pins: the read still held a lock past its resolve", got)
	}
	assertNoRecordedViolations(t, rec, "the recorded read")
}

// TestRecorder_RemovalWaitReleasesAppend checks every removal reaches its
// durability wait with the file's append domain released: the wait consults
// the recorder under the label the removal's own assertion carries, so the
// test cannot pass with the assertion dropped, and a removal that parked the
// wait under the domain — the deadlock with the worker that resolves the
// file's window under that same domain — is reported as a held-at-wait
// violation.
func TestRecorder_RemovalWaitReleasesAppend(t *testing.T) {
	c, rec, shares := newRecordedCore(t)
	doomed := fid(5)
	s := shares[0]
	if _, err := c.WriteAt(context.Background(), s, doomed, 0, []byte("doomed"), 1); err != nil {
		t.Fatalf("WriteAt of the removed file: %v", err)
	}
	if _, err := c.Truncate(context.Background(), s, doomed, 2); err != nil {
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

	vs := rec.Violations()
	if len(vs) <= before {
		t.Fatalf("violations = %d after a read under a held file index, want more than the %d before it", len(vs), before)
	}
	// Every lock the read takes reports out of order or of the same domain
	// against the held index; the two the site means to prove are the I/O
	// rule, which only the FS wrapper can fire, and the guard rule, which
	// only the descriptor reference can. The rest are the ordinary
	// out-of-order reports every ranked acquisition under the probe makes.
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
// on a write to another: a read of B first passes its asOf step, which takes
// and releases B's own append domain, and then the write of B parks inside
// the afterAppend seam holding that same domain. While it is parked, a read
// of A - another file, whose only shared locks are the recorder's to take -
// and B's own reader, past its domain, both complete before the writer is
// released. A reader that waited on any lock a parked write holds, the
// stream's append lock among them, would block past the bound.
func TestRecorder_ReadOfAIgnoresWriteOfB(t *testing.T) {
	// pastOwnDomain closes once B's read is past its own file's append
	// domain, so the write parks only then: parking first would have B's
	// read wait on its own file's domain - the per-file serialisation the
	// append domain exists for - which says nothing about cross-file waits.
	pastOwnDomain := make(chan struct{})
	parked := make(chan struct{})
	release := make(chan struct{})
	var writeBack, armAfter chan error
	options := func(o *coreOptions) {
		o.beforeResolve = func() {
			if armAfter != nil {
				ch := armAfter
				armAfter = nil
				close(pastOwnDomain)
				_ = ch
			}
		}
		o.afterAppend = func() {
			if writeBack != nil {
				ch := writeBack
				writeBack = nil
				close(parked)
				<-release
				_ = ch
			}
		}
	}
	c, rec, shares := newRecordedCoreOpts(t, options)
	s0, s1 := shares[0], shares[1]
	// A on stream 0 through share 1, B on stream 1 through share 2: the
	// fixture's second file is exactly that.
	b := fileOnOtherStream(c, fid(1))
	a := fid(1)
	if _, err := c.WriteAt(context.Background(), s1, b, 0, []byte("B-first"), 1); err != nil {
		t.Fatalf("B's first write: %v", err)
	}

	// B's reader goes first: the seam arms the writer's park once the read
	// is past its own domain, so the parked write never blocks it at the
	// domain and what remains on its path is what the test means to catch.
	type readOut struct {
		n   int
		err error
	}
	readB := make(chan readOut, 1)
	armAfter = make(chan error, 1)
	go func() {
		n, _, _, _, err := c.ReadAt(context.Background(), s1, b, 0, make([]byte, 7))
		readB <- readOut{n, err}
	}()
	select {
	case <-pastOwnDomain:
	case <-time.After(10 * time.Second):
		t.Fatal("B's read never passed its own file's append domain")
	}

	// B's second write parks in the seam with B's append domain held. The
	// writer hands its answer to a channel it copies locally, for the seam
	// disarms the shared variable on entry and a goroutine reading it after
	// would send on nil and hang the run's cleanup.
	wb := make(chan error, 1)
	writeBack = wb
	go func() {
		_, err := c.WriteAt(context.Background(), s1, b, 8, []byte("B-second"), 1)
		wb <- err
	}()
	select {
	case <-parked:
	case <-time.After(10 * time.Second):
		t.Fatal("B's write never reached the afterAppend seam")
	}

	// Both reads must finish before the writer is released: A's read names
	// another file, and B's reader is past its own domain; a reader waiting
	// on the stream's append lock or the file map would block past the
	// bound.
	readA := make(chan readOut, 1)
	go func() {
		n, _, _, _, err := c.ReadAt(context.Background(), s0, a, 0, make([]byte, 7))
		readA <- readOut{n, err}
	}()
	for _, r := range []struct {
		name string
		ch   chan readOut
		want int
	}{
		{"A beside B's parked write", readA, 7},
		{"B's earlier content", readB, 7},
	} {
		select {
		case out := <-r.ch:
			if out.err != nil {
				t.Fatalf("ReadAt of %s: %v", r.name, out.err)
			}
			if out.n != r.want {
				t.Fatalf("ReadAt of %s copied %d bytes, want %d", r.name, out.n, r.want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("ReadAt of %s did not return beside B's parked write: it waited on a lock the write holds", r.name)
		}
	}
	close(release)
	if err := <-wb; err != nil {
		t.Fatalf("B's parked write: %v", err)
	}
	assertNoRecordedViolations(t, rec, "the recorded read-beside-write")
}

// TestRecorder_RemovalDuringHeldCallback checks no lock a removal takes is
// held across the offload callback: a stand-in goroutine enters the
// callback's assertion — which records a violation when the calling
// goroutine holds any ranked lock — and blocks where the callback would run
// its upload. While it blocks, a truncate and a delete of the same file both
// complete: the removals take no lock the held callback holds. The real
// callback is the reclaim epic's; this pins the assertion and the ordering
// it promises.
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
	go func() {
		rec.AssertNoneHeld("offload callback")
		close(inCallback)
		<-release
	}()
	select {
	case <-inCallback:
	case <-time.After(10 * time.Second):
		t.Fatal("the stand-in callback never entered its assertion")
	}

	ctx := context.Background()
	if _, err := c.Truncate(ctx, s, f, 2); err != nil {
		t.Fatalf("Truncate beside a held callback: %v", err)
	}
	if _, err := c.Delete(ctx, s, f); err != nil {
		t.Fatalf("Delete beside a held callback: %v", err)
	}
	close(release)

	if got := rec.Checks("offload callback"); got != 1 {
		t.Fatalf("offload callback checks = %d, want 1", got)
	}
	if got := rec.Violations(); len(got) != len(before) {
		t.Fatalf("violations beside a held callback = %d, want the %d before it: a removal took a lock the callback holds\n%+v", len(got), len(before), got[len(before):])
	}
}

// TestRecorder_TakeRunsUnderAppendOnly checks the admission runs under the
// file append domain alone: the core takes a tagged write's domain, then the
// admission decides under its mutex, and inside that decision section the
// calling goroutine holds exactly the file append and capacity ranks. A Take
// that ran with the file index held — a capacity lock taken under it, the
// order RFC 1 §8.3 forbids — or under any lock besides the write's own
// domain, fails the exact-equality below.
func TestRecorder_TakeRunsUnderAppendOnly(t *testing.T) {
	var probe *lockorder.Recorder
	heldInside := make(chan []lockorder.Rank, 1)
	c, rec, shares := newRecordedCore(t)
	s := shares[0]
	f := streamFileID(c, c.streams.IndexFor(fid(1)))
	c.cap.SetTakeHook(func() {
		if probe != nil {
			p := probe
			probe = nil
			heldInside <- p.Held()
		}
	})
	probe = rec
	if _, err := c.WriteAt(context.Background(), s, f, 0, []byte("admitted"), 1); err != nil {
		t.Fatalf("WriteAt under the take hook: %v", err)
	}
	got := withoutPins(<-heldInside)
	want := []lockorder.Rank{lockorder.FileAppend, lockorder.Capacity}
	if !slices.Equal(got, want) {
		t.Fatalf("ranks held inside the admission's decision section = %v, want %v: the admission ran under a lock besides the write's own append domain", got, want)
	}
	assertNoRecordedViolations(t, rec, "the recorded admission")
}

// TestRecorder_RefusedWriteHoldsNothing checks a write the admission refuses
// holds no lock after it returns: the share's limit is drawn to exactly its
// first write's record bytes, so the second write is refused with a
// NoSpaceError, and on its own goroutine — the one the refusal ran on, where
// a hold it leaked would sit on the books of no other goroutine — nothing
// but the stream appender's pins remains held. A read of the same file
// between the refusal's admission and its return completes on another
// goroutine, which it could not were the refusal holding the file's index or
// append domain.
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

	// The refused write runs on its own goroutine and reports what it holds
	// once it returns: the bound is generous, for a refused write must
	// answer at once, holding nothing behind.
	refused := make(chan error, 1)
	heldAfter := make(chan []lockorder.Rank, 1)
	go func() {
		_, err := c.WriteAt(context.Background(), limited, f, each, make([]byte, each), 1)
		refused <- err
		heldAfter <- rec.Held()
	}()
	select {
	case err := <-refused:
		var ns *core.NoSpaceError
		if !errors.As(err, &ns) || ns.Limit != core.LimitShare {
			t.Fatalf("the second write = %v, want a NoSpaceError naming the share's limit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refused write never returned: it held something behind its refusal")
	}
	if got := withoutPins(<-heldAfter); len(got) != 0 {
		t.Fatalf("the refused write's goroutine holds ranks %v after its refusal, want only the stream's pins", got)
	}

	// A read of the same file completes beside the refusal's admission: a
	// write holding the file's index or append domain across its refusal
	// would stop the read past the bound.
	read := make(chan error, 1)
	go func() {
		_, _, _, _, err := c.ReadAt(context.Background(), limited, f, 0, make([]byte, 4))
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("ReadAt beside the refused write: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReadAt beside the refused write did not return: the refusal held a lock the read needs")
	}
	assertNoRecordedViolations(t, rec, "the recorded refusal")
}

// TestRecorder_FileMapHeldOnlyAcrossALookup checks nothing is acquired
// while the file map is held: a first write to a fresh file creates the
// file's entry under the map, and a removal drops what it holds — the two
// paths the map's domain covers. Both run against the recorder, whose
// acquire-under-map rule fires the moment anything is taken with the map
// held, so the assertion is the counts on the window the operations kept.
func TestRecorder_FileMapHeldOnlyAcrossALookup(t *testing.T) {
	c, rec, shares := newRecordedCore(t)
	s := shares[0]
	fresh := streamFileID(c, c.streams.IndexFor(fid(1)))
	if _, err := c.WriteAt(context.Background(), s, fresh, 0, []byte("entry"), 1); err != nil {
		t.Fatalf("WriteAt of the fresh file: %v", err)
	}
	doomed := fid(80)
	if _, err := c.WriteAt(context.Background(), s, doomed, 0, []byte("doomed"), 1); err != nil {
		t.Fatalf("WriteAt of the doomed file: %v", err)
	}
	if _, err := c.Truncate(context.Background(), s, doomed, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	assertNoRecordedViolations(t, rec, "the recorded entry-creating write and removal")
}

// recordedFiles returns the file set the recorded no-deadlock run works over:
// four fresh ids, one per (stream, share) pair — files 0 and 2 run through
// share 1, files 1 and 3 through share 2, and each share names one file on
// each stream. The ids sit well above the fixture's own, so no run operation
// meets an entry the fixture seeded.
func recordedFiles(c *Core) [4]core.FileID {
	var out [4]core.FileID
	s0 := c.streams.IndexFor(fid(1))
	s1 := c.streams.IndexFor(fileOnOtherStream(c, fid(1)))
	pick := func(stream int, i int) core.FileID {
		want := s0
		if stream == 1 {
			want = s1
		}
		for k := uint64(1000 + i); ; k++ {
			if id := fid(k); c.streams.IndexFor(id) == want {
				return id
			}
		}
	}
	// One file per (stream, share) pair: files 0 and 2 run through share 1,
	// files 1 and 3 through share 2, and each share names one file on each
	// stream — files 0 and 3 on the first, files 1 and 2 on the second.
	out[0], out[3] = pick(0, 0), pick(0, 3)
	out[1], out[2] = pick(1, 1), pick(1, 2)
	return out
}

// TestRFC1B_NoDeadlockRecorded runs the data path's whole concurrent fan
// under the lock-order recorder: writers, readers, fills and removals at
// once, over four files spread over both shares and both streams, for a
// fixed round count. The epic's reclamation-side calls are outside this
// package and are not run. The run must record no violation, and every call
// is bounded by a watchdog that dumps every goroutine's stack and fails the
// test if a call is blocked past it — the discipline of the public
// TestRFC1B_NoDeadlock, over the core the recorder watches. It ends with the
// test goroutine holding nothing but its own appender's pins.
func TestRFC1B_NoDeadlockRecorded(t *testing.T) {
	const rounds = 100
	// callWait bounds one operation call, generous against a loaded machine
	// and small against a lock cycle: a real deadlock parks far past it.
	const callWait = recordedWatchdog

	c, rec, shares := newRecordedCore(t)
	ctx := context.Background()
	files := recordedFiles(c)
	// Each file is published once up front, through the share that owns it:
	// the first write of a file claims it for the handle that ran it, so a
	// fixed owner per file keeps every later operation on the owning share
	// — files 0 and 2 through share 1, files 1 and 3 through share 2, so
	// both handles see both streams.
	shareOf := func(i int) *Share { return shares[i%2] }
	seeds := func() {
		for i := range files {
			if _, err := c.WriteAt(ctx, shareOf(i), files[i], 0, make([]byte, 512), 1); err != nil {
				t.Fatalf("the seed write of file %d: %v", i, err)
			}
		}
	}
	seeds()

	// A blocked call fails the test from any goroutine: inFlight counts the
	// calls that began and did not return, with each entrant's start time,
	// and the watchdog walks the table, dumps every stack and fails once a
	// call sits past callWait. The table's own mutex is a test-local leaf.
	type call struct {
		name  string
		start time.Time
	}
	var mu sync.Mutex
	inFlight := map[int]*call{}
	next := 0
	enter := func(name string) (id int) {
		mu.Lock()
		id = next
		next++
		inFlight[id] = &call{name: name, start: time.Now()}
		mu.Unlock()
		return id
	}
	leave := func(id int) {
		mu.Lock()
		delete(inFlight, id)
		mu.Unlock()
	}
	watchDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-watchDone:
				return
			case <-time.After(100 * time.Millisecond):
			}
			mu.Lock()
			for _, cl := range inFlight {
				if time.Since(cl.start) > callWait {
					buf := make([]byte, 1<<20)
					buf = buf[:runtime.Stack(buf, true)]
					mu.Unlock()
					t.Errorf("a %s call blocked for more than %v; every goroutine's stack follows:\n%s", cl.name, callWait, buf)
					return
				}
			}
			mu.Unlock()
		}
	}()

	// pattern is one writer's payload, distinct per writer.
	pattern := func(seed byte, n int) []byte {
		p := make([]byte, n)
		for i := range p {
			p[i] = seed + byte(i)
		}
		return p
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	worker := func(name string, fn func(r *rand.Rand)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				assertHeldNone(t, rec, name)
			}()
			r := rand.New(rand.NewPCG(uint64(rand.Uint64()), 1))
			<-start
			for range rounds {
				fn(r)
			}
		}()
	}
	const chunk = 4096
	// The writers use their own file per goroutine, filing the same span
	// under a cycling version their reader matches by asOf alone.
	worker("writer", func(r *rand.Rand) {
		i := int(r.IntN(4))
		id := enter("WriteAt")
		_, err := c.WriteAt(ctx, shareOf(i), files[i], int64(r.IntN(16))*chunk, pattern(byte(i), chunk), 1)
		leave(id)
		if err != nil {
			t.Errorf("WriteAt: %v", err)
		}
	})
	worker("writer", func(r *rand.Rand) {
		i := int(r.IntN(4))
		id := enter("WriteAt")
		_, err := c.WriteAt(ctx, shareOf(i), files[i], int64(r.IntN(16))*chunk, pattern(byte(i+8), chunk), 1)
		leave(id)
		if err != nil {
			t.Errorf("WriteAt: %v", err)
		}
	})
	worker("reader", func(r *rand.Rand) {
		i := int(r.IntN(4))
		id := enter("ReadAt")
		_, _, _, _, err := c.ReadAt(ctx, shareOf(i), files[i], int64(r.IntN(16))*chunk, make([]byte, chunk))
		leave(id)
		if err != nil {
			t.Errorf("ReadAt: %v", err)
		}
	})
	worker("reader", func(r *rand.Rand) {
		i := int(r.IntN(4))
		id := enter("ReadAt")
		_, _, _, _, err := c.ReadAt(ctx, shareOf(i), files[i], 0, make([]byte, 4*chunk))
		leave(id)
		if err != nil {
			t.Errorf("ReadAt: %v", err)
		}
	})
	// The fills use the version the counters draw, at the asOf a read of
	// the file reports, exactly as the journal's own fill path feeds them;
	// a stale fill is refused whole and that refusal is the run's to see.
	worker("filler", func(r *rand.Rand) {
		i := int(r.IntN(4))
		s := shareOf(i)
		f := files[i]
		p := pattern(byte(i+16), chunk)
		v, err := c.Draw()
		if err != nil {
			return // the generous limit stays far above the run's draws; a refusal ends the round only
		}
		id := enter("Fill")
		err = c.Fill(ctx, s, f, int64(r.IntN(16))*chunk, p, c.Index().AsOf(f), v)
		leave(id)
		if err != nil && !errors.Is(err, core.ErrStaleFill) {
			t.Errorf("Fill: %v", err)
		}
	})
	// The removals cycle truncate, deallocate and delete over the files,
	// re-creating what they removed so later rounds still find content.
	worker("remover", func(r *rand.Rand) {
		i := int(r.IntN(4))
		s := shareOf(i)
		f := files[i]
		id := enter("removal")
		var err error
		switch r.IntN(3) {
		case 0:
			_, err = c.Truncate(ctx, s, f, int64(r.IntN(8))*chunk)
		case 1:
			_, err = c.Deallocate(ctx, s, f, core.Extent{Off: int64(r.IntN(8)) * chunk, Len: chunk})
		default:
			_, err = c.Delete(ctx, s, f)
		}
		leave(id)
		if err != nil {
			t.Errorf("removal of file %d: %v", i, err)
		}
	})

	close(start)
	wg.Wait()
	close(watchDone)

	assertNoRecordedViolations(t, rec, "the recorded no-deadlock run")
	assertHeldNone(t, rec, "the test goroutine")
}
