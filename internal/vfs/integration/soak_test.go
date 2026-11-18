//go:build engine_conformance && engine_soak

package integration

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// decision: a seed fixes the sequence of client calls and the crash variants,
// not when each fault fires. The engine's goroutines consult the schedules
// concurrently, so each rule's stream is drawn in whatever order they enter
// it, and one seed can fire its faults in a different call on each run. A
// known entry keyed by a seed may therefore change its message.
var soakSeed = flag.Uint64(
	"soak.seed",
	0,
	"run only this seed of the soak instead of the fixed list (its calls repeat, its faults may not)",
)

// soakSeeds are the seeds every run of the soak replays.
//
// ponytail: a fixed list finds only what these seeds reach. Upgrade to a
// time-boxed run of fresh seeds, each one logged, when the nightly budget
// allows it or the list has found nothing for a month. The count is
// provisional: no green run has been timed yet, so set it from the suite's
// measured time once the soak passes.
var soakSeeds = []uint64{1, 2}

// soakFakeSeeds are the seeds the run over the in-memory journal replays: the
// soak's own, two that reach the debt and flush paths, 26, whose Commit
// waits on an existence commit the engine retries on the test clock, and 59,
// whose SMB flush is the Commit whose own Sync fails a window.
var soakFakeSeeds = []uint64{1, 2, 10, 26, 59, 60}

const (
	soakFiles     = 3                // files each seed writes
	soakRounds    = 4                // restarts each seed runs, each after soakCalls calls
	soakCalls     = 16               // client calls between two restarts
	soakSpan      = 64 << 10         // the offsets the calls reach in a file
	soakMaxWrite  = 4 << 10          // the largest write
	soakCallBound = 10 * time.Second // a real-time backstop on one call, so a wedge fails instead of hanging
	soakSlowCall  = 2 * time.Second  // a call this slow was parked, whatever it answered
	// soakTick is how long, in real time, a call may wait before the loop
	// moves the test clock under it: the soak's one wait on real time.
	soakTick = 20 * time.Millisecond
)

var errInjected = errors.New("soak: an injected fault")

// The schedules' rules. A write may be lost or silently torn, a sync may
// fail, and a store transaction may fail. None delays: a delay waits on the
// test clock, which only the writer moves, between calls.
var (
	soakJournalRules = []sim.Rule{
		{Op: kit.OpWrite, Key: "*.seg", Prob: 0.03, Action: sim.FaultAction{TearAfter: sim.Lose}},
		{Op: kit.OpWrite, Key: "*.seg", Prob: 0.02, Action: sim.FaultAction{TearAfter: 16}},
		{Op: kit.OpSync, Key: "*.seg", Prob: 0.03, Action: sim.FaultAction{Err: errInjected}},
	}
	// The in-memory journal has no storage under it, so its faults are the
	// fake's own: after each write the run asks these rules, keyed by the
	// file's name, whether the write's record is lost (Lose) and, for an
	// unstable append or hole fill, whether its sync window fails (FailSync).
	soakFakeRules = []sim.Rule{
		{Op: kit.OpWrite, Key: "*", Prob: 0.05, Action: sim.FaultAction{TearAfter: sim.Lose}},
		{Op: kit.OpSync, Key: "*", Prob: 0.30, Action: sim.FaultAction{Err: errInjected}},
	}
	// The run over the in-memory journal fails store updates more often, so
	// each of its seeds fires a store fault (seed 60 fired none at 3%).
	soakFakeStoreRules = []sim.Rule{
		{Op: kvtest.OpUpdate, Key: "*", Prob: 0.06, Action: sim.FaultAction{Err: errInjected}},
	}
	soakStoreRules = []sim.Rule{
		{Op: kvtest.OpUpdate, Key: "*", Prob: 0.03, Action: sim.FaultAction{Err: errInjected}},
	}
)

// TestSoak composes the filesystem service over the engine, the real journal
// and the real store, and for each seed issues a seeded mix of writes, stable
// writes, Commits, truncates and deallocates under seeded journal and store
// faults, crashing or restarting the process between rounds; after each
// restart it reads every file back against the oracle.
func TestSoak(t *testing.T) {
	seeds := soakSeeds
	if *soakSeed != 0 {
		seeds = []uint64{*soakSeed}
	}

	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			runSoak(t, seed, false, nil)
		})
	}
}

// TestSoakOverTheInMemoryJournal is TestSoak's loop and oracle over the
// in-memory journal: a restart is its Reopen, and the journal's faults are its
// Lose and FailSync. The fake keeps everything durable across a restart and
// has no crash variants, so this run shows the engine's half (the Lost a loss
// leaves, the verifier, the existence a restart re-applies) and the oracle's
// plumbing, not torn tails or a committed size after a host crash.
//
// It stands in for TestSoak until the journal's Open recovers the segments a
// crash leaves, and goes then, since a soak over a double proves nothing
// about the journal.
func TestSoakOverTheInMemoryJournal(t *testing.T) {
	seeds := soakFakeSeeds
	if *soakSeed != 0 {
		seeds = []uint64{*soakSeed}
	}

	var list listCounts

	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			runSoak(t, seed, true, &list)
		})
	}

	// A list that fails no sync window tests none of the failed-window path:
	// the changed verifier, the open's ErrLost. One that resends nothing
	// tests no NFS client's answer to a changed verifier.
	if *soakSeed == 0 && list.windows == 0 {
		t.Errorf("the seeds %v failed no sync window; the list must fail at least one", seeds)
	}

	if *soakSeed == 0 && list.resends == 0 {
		t.Errorf("the seeds %v resent nothing; the list must resend at least once", seeds)
	}
}

// soakRun is one seed's run.
type soakRun struct {
	t     *testing.T
	seed  uint64
	c     *composition
	rng   *rand.Rand
	log   *faultLog
	m     *soakModel
	files []vfs.Handle

	proc  int           // the process running now, from 1
	smb   vfs.Call      // the SMB client of the process running now
	refs  []vfs.OpenRef // its open of each file
	opens []*smbOpen    // the same opens, as the oracle judges their flushes
	calls int
	seen  int // the faults already attributed

	// Over the in-memory journal: the fake, the files' identifiers, and the
	// journal schedule the run consults after each write in the seam's place.
	fake        *enginetest.FakeJournal
	ids         []entity.FileID
	fakeJournal sim.Fault
	lossSeen    uint64 // the newest event of the fake's loss ring the run has read
	// doomed holds, per file, the condemned writes that landed and whose
	// windows have not failed yet.
	doomed map[int][]*soakOp

	// clock is the test clock the run moves, and clockSteps the seed's stream
	// of the steps it moves it by while a call waits, apart from the calls'
	// own stream so a call's wait never changes the calls that follow.
	clock      *sim.TestClock
	clockSteps *rand.Rand

	resends int // the resends a Commit acknowledged

	// untorn counts the Torn crashes the seed drew where no write was
	// unsynced, which the seam cannot tear, so a plain crash ran in its place.
	untorn int
}

// listCounts is what a seed list did, counted across its seeds: the sync
// windows the journal schedule failed, and the resends a Commit acknowledged.
type listCounts struct {
	windows, resends int
}

// runSoak runs one seed; when list is not nil, it adds to it what the seed
// did, even if the seed fails.
func runSoak(t *testing.T, seed uint64, fake bool, list *listCounts) {
	var opts []enginetest.Option
	if fake {
		opts = append(opts, enginetest.WithFakeJournal())
	}

	r := &soakRun{
		t:    t,
		seed: seed,
		c:    compose(t, opts...),
		rng:  rand.New(rand.NewPCG(seed, 0)),
		// The clock's own stream: how far the clock moves while a call waits
		// depends on how long it waits, so it draws apart from the calls.
		clockSteps: rand.New(rand.NewPCG(seed, 1)),
		log:        &faultLog{},
		m:          newSoakModel(),
	}

	r.clock = r.c.Clock

	if list != nil {
		t.Cleanup(func() {
			list.windows += r.windowsFired()
			list.resends += r.resends
		})
	}

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf(
				"seed %d failed; rerun it with -soak.seed=%d, which repeats its calls but not always when its faults fire. Faults that fired, in order:\n%s",
				seed,
				seed,
				r.log,
			)
		}
	})

	// The files exist before any fault, so their creation is no call of the
	// run's.
	for i := range soakFiles {
		name := fmt.Sprintf("f%d", i)
		h, id := r.c.File(name)
		ctx, cancel := r.bounded()
		attr, err := r.c.fs.GetAttr(ctx, r.c.admin, h)
		cancel()
		if err != nil {
			t.Fatalf("seed %d: GetAttr of the new %s: %v", seed, name, err)
		}

		r.files, r.ids = append(r.files, h), append(r.ids, id)
		r.m.file(name, attr.Modify)
	}

	rules, storeRules := soakJournalRules, soakStoreRules
	if fake {
		rules, storeRules = soakFakeRules, soakFakeStoreRules
	}

	jsched, err := sim.NewSchedule(seed, r.c.Clock, rules...)
	if err != nil {
		t.Fatal(err)
	}

	store, err := sim.NewSchedule(seed, r.c.Clock, storeRules...)
	if err != nil {
		t.Fatal(err)
	}

	if fake {
		r.fake = r.c.Fake()
		r.fakeJournal = loggedFault{log: r.log, side: "journal", inner: jsched}
	} else {
		r.c.Seam.SetFault(loggedFault{log: r.log, side: "journal", inner: jsched})
		t.Cleanup(func() {
			r.c.Seam.SetFault(nil)
		})
	}

	r.c.FaultStore(loggedFault{log: r.log, side: "store", inner: store})

	r.startProcess()
	for range soakRounds {
		for range soakCalls {
			r.step()
		}

		r.restart()
		r.check()
	}

	r.firedBoth()
}

// firedBoth fails a seed whose schedules fired no journal fault or no store
// fault: such a seed tested no fault on that side, and a log or a schedule
// that lost every fault would pass unseen.
func (r *soakRun) firedBoth() {
	sides := map[string]int{}
	lost, windows := 0, 0
	for _, f := range r.log.since(0) {
		sides[f.side]++

		switch {
		case f.lostRecord():
			lost++
		case f.side == "journal" && f.op == kit.OpSync && f.err != nil:
			windows++
		}
	}

	r.t.Logf(
		"seed %d: %d calls, %d processes, %d store faults, "+
			"%d journal faults (%d lost records, %d failed sync windows), %d torn crashes found nothing unsynced",
		r.seed,
		r.calls,
		r.proc,
		sides["store"],
		sides["journal"],
		lost,
		windows,
		r.untorn,
	)

	if sides["journal"] == 0 || sides["store"] == 0 {
		r.fail(
			"the schedules fired %d journal and %d store faults; a seed must fire at least one of each",
			sides["journal"],
			sides["store"],
		)
	}
}

// windowsFired is how many sync windows the seed's journal schedule failed.
func (r *soakRun) windowsFired() int {
	n := 0

	for _, f := range r.log.since(0) {
		if f.side == "journal" && f.op == kit.OpSync && f.err != nil {
			n++
		}
	}

	return n
}

// startProcess counts a new process and opens every file through a new SMB
// client of it, with the faults paused.
func (r *soakRun) startProcess() {
	r.log.set(0, "", true)
	defer r.log.set(0, "", false)

	r.proc++
	r.doomed = map[int][]*soakOp{} // a restart drops the windows not yet failed
	r.smb = r.c.smb(r.t, fmt.Sprintf("soak-%d", r.proc), r.c.FS.Admin.Principal)
	r.refs, r.opens = r.refs[:0], r.opens[:0]

	for i := range r.files {
		name := r.m.files[i].name
		var res vfs.OpenResult
		err := vfs.ErrGrace
		// A start puts the shard in grace, which a new open waits out on the
		// test clock.
		for try := 0; errors.Is(err, vfs.ErrGrace) && try < 3; try++ {
			if try > 0 {
				r.c.Clock.Advance(2 * time.Minute)
			}

			ctx, cancel := r.bounded()
			res, err = r.c.fs.Open(ctx, r.smb, vfs.OpenRequest{
				Dir: r.c.FS.Root, Name: []byte(name), Disposition: vfs.OpenExisting,
				Access: entity.ReadData | entity.WriteData, Timeout: time.Minute,
			})
			cancel()
		}

		if err != nil {
			r.fail("process %d: the SMB open of %s: %v", r.proc, name, err)
		}

		r.refs = append(r.refs, res.Ref)
		r.opens = append(r.opens, r.m.open(r.m.files[i], r.proc))
	}
}

// restart crashes the process, with a crash variant the seed draws, or stops
// it cleanly, and starts the next.
func (r *soakRun) restart() {
	r.log.set(0, "", true)
	switch r.rng.IntN(4) {
	case 0:
		r.c.Restart()
	case 1:
		r.c.Crash()
	case 2:
		seed := r.rng.Uint64()
		if r.fake != nil {
			r.c.Crash() // the fake keeps everything durable, so no crash variant applies
		} else {
			r.c.Crash(kit.Reordered(seed))
		}
	default:
		n := 1 + r.rng.IntN(16)
		switch {
		case r.fake != nil:
			r.c.Crash()
		case r.c.Seam.Unsynced() == 0:
			r.untorn++
			r.c.Crash()
		default:
			r.c.Crash(kit.Torn(n))
		}
	}

	// A window the stop's last Sync failed belongs to the process that
	// ended, whose report the restart drops.
	r.failedWindows()
	r.attribute()
	r.startProcess()
}

// step issues one client call the seed draws, records it in the oracle,
// attributes the faults that fired, and moves the clock by a step the seed
// draws, so the group commit comes due at drawn points.
func (r *soakRun) step() {
	i := r.rng.IntN(soakFiles)
	f, h := r.m.files[i], r.files[i]
	r.calls++

	ctx, cancel := r.bounded()
	defer cancel()

	var (
		op     *soakOp
		desc   string
		err    error
		commit bool
		// lostOpen is set when the SMB open flushed may answer ErrLost.
		lostOpen bool
		// flushOpen is the SMB open flushed, and spare that its file held a
		// loss a restart found before the call.
		flushOpen *smbOpen
		spare     bool
		// anonymous is set for a Commit without an open, and answered to the
		// verifier it answered with success.
		anonymous bool
		answered  vfs.Verifier
		// record puts a Commit's answer in the oracle. It runs after the
		// call's faults are attributed: a record lost inside the Commit must
		// credit the writes it has not yet acknowledged, as check does.
		record func()
	)
	start := time.Now()
	switch k := r.rng.IntN(100); {
	case k < 50:
		stable := k >= 35
		// A stable write is a Write and a Commit over its extent, so it is the
		// file's next stability point, where a failed window's debt is paid.
		commit = stable
		off := r.rng.Int64N(soakSpan)
		data := make([]byte, 1+r.rng.IntN(soakMaxWrite))
		for j := range data {
			data[j] = byte(1 + r.rng.IntN(255))
		}

		how := vfs.Unstable
		if stable {
			how = vfs.FileSync
		}

		desc = fmt.Sprintf("write %s [%d, %d) stable=%v", f.name, off, off+int64(len(data)), stable)
		r.log.set(r.calls, desc, false)
		doomed := r.doomWrite(i, off, off+int64(len(data)), stable)

		var res vfs.WriteResult

		r.waiting(func() {
			res, err = r.c.fs.Write(ctx, r.c.admin, vfs.Anonymous(h), off, data, how)
		})

		op = &soakOp{
			kind:     opWrite,
			off:      off,
			data:     data,
			proc:     r.proc,
			done:     err == nil,
			verifier: res.Verifier,
		}
		f.add(op, stable)

		if doomed {
			r.fake.FailNextWrite(journal.NewFileID(r.ids[i]), false)

			if err == nil {
				r.doomed[i] = append(r.doomed[i], op)
			}
		}

		if err == nil {
			r.observe(res.Verifier)
			r.faultFake(i, op)
		}
	case k < 62:
		commit = true
		desc = fmt.Sprintf("commit %s", f.name)
		r.log.set(r.calls, desc, false)

		anonymous = true

		var v vfs.Verifier

		r.waiting(func() {
			v, err = r.c.fs.Commit(ctx, r.c.admin, vfs.Anonymous(h), 0, 0)
		})
		record = func() {
			if ferr := f.nfsCommit(r.proc, v, err); ferr != nil {
				r.fail("call %d, %s: %v", r.calls, desc, ferr)
			}

			f.commit(r.proc, 0, 0, v, err)

			if err == nil {
				r.observe(v)
				answered = v
			}
		}
	case k < 74:
		commit = true
		desc = fmt.Sprintf("commit %s through an SMB open", f.name)
		spare, flushOpen = f.owes, r.opens[i]
		r.log.set(r.calls, desc, false)

		var v vfs.Verifier

		r.waiting(func() {
			v, err = r.c.fs.Commit(ctx, r.smb, r.refs[i], 0, 0)
		})
		record = func() {
			if ferr := r.m.flush(r.opens[i], v, err, spare); ferr != nil {
				r.fail("call %d, %s: %v", r.calls, desc, ferr)
			}

			if err == nil {
				r.observe(v)
			}
		}
	case k < 87:
		size := r.rng.Int64N(soakSpan)
		desc = fmt.Sprintf("truncate %s to %d", f.name, size)
		r.log.set(r.calls, desc, false)
		// The engine calls a truncate a grow when it reaches the size the
		// file has now, what its replayed operations leave after any crash,
		// which is the size the service reports; the soak's calls are
		// sequential, so nothing moves it between this read and the call.
		gctx, gcancel := r.bounded()
		before, gerr := r.c.fs.GetAttr(gctx, r.c.admin, h)

		gcancel()

		if gerr != nil {
			r.fail("GetAttr of %s before call %d: %v", f.name, r.calls, gerr)
		}

		grow := truncateGrows(size, before)

		r.waiting(func() {
			_, err = r.c.fs.SetAttr(
				ctx,
				r.c.admin,
				vfs.Target{H: h},
				vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(size)},
			)
		})
		op = f.add(
			&soakOp{kind: opTruncate, off: size, proc: r.proc, done: err == nil, grow: grow},
			false,
		)
	default:
		off, n := r.rng.Int64N(soakSpan), 1+r.rng.Int64N(soakMaxWrite)
		desc = fmt.Sprintf("deallocate %s [%d, %d)", f.name, off, off+n)
		r.log.set(r.calls, desc, false)
		r.waiting(func() {
			_, err = r.c.fs.Deallocate(ctx, r.c.admin, vfs.Anonymous(h), off, n)
		})
		op = f.add(
			&soakOp{kind: opDeallocate, off: off, n: n, proc: r.proc, done: err == nil},
			false,
		)
	}

	r.log.set(0, "", false)
	r.slow(desc, start)
	r.failedWindows()

	if flushOpen != nil {
		// An open across a loss found in this process answers ErrLost on
		// exactly one flush, whatever Commit of the file came between, a
		// window this flush's own Sync failed included; a loss a restart
		// found may also reach the open's loss sequence.
		lostOpen = r.m.owesLost(flushOpen) || spare
	}

	faulted := r.attribute(op)
	if record != nil {
		record()
	}

	// A lost record, or a loss a restart found, may fail the file's next
	// Commit once, through an SMB open as ErrLost; the one after owes
	// nothing. A failed sync window fails no Commit: it changes the verifier.
	owed := commit && f.owed()

	if !r.commitExcused(err, faulted || owed, lostOpen) {
		r.fail("call %d, %s: %v", r.calls, desc, err)
	}

	if anonymous && err == nil {
		r.resend(ctx, i, answered)
	}

	if op != nil {
		gctx, gcancel := r.bounded()
		attr, aerr := r.c.fs.GetAttr(gctx, r.c.admin, h)
		gcancel()
		if aerr != nil {
			r.fail("GetAttr of %s after call %d: %v", f.name, r.calls, aerr)
		}

		op.mtime = attr.Modify
	}

	r.c.Clock.Advance(time.Duration(r.rng.Int64N(int64(2 * enginetest.Settings.ExistenceAge))))
}

// faultFake stands for the storage the in-memory journal does not have:
// after write op of file i answered, still inside its call, it asks the
// journal schedule whether the write's record is lost, and applies it with
// the fake's Lose. A lost record is a loss event, which raises the file's
// loss sequence, so the file's next Commit may answer it once.
func (r *soakRun) faultFake(i int, op *soakOp) {
	if r.fake == nil {
		return
	}

	f := r.m.files[i]
	id := journal.NewFileID(r.ids[i])
	e := journal.Extent{Off: op.off, Len: int64(len(op.data))}

	if _, tear, _ := r.fakeJournal.At(kit.OpWrite, []byte(f.name)); tear < 0 {
		r.fake.Lose(id, e, journal.LossCorrupt)

		f.owes = true
		r.m.lose(f, r.proc)
	}
}

// doomWrite asks the journal schedule, before an unstable write of file i to
// [off, end) is issued, whether its sync window fails, and if so has the fake
// fail it at the next Sync that covers the record, as the journal does. It
// answers whether it armed that, so the caller disarms it once the call
// returns, a failed write having appended nothing.
//
// decision: a failed window only on an unstable append or hole fill, never
// on an overwrite, because the fake's unsynced loss cuts the extent instead
// of holding again what the write overwrote, which RFC 1 §3.8 has a failed
// window do. Withdraw it once the fake holds that content again.
func (r *soakRun) doomWrite(i int, off, end int64, stable bool) bool {
	f := r.m.files[i]
	if r.fake == nil || stable || f.meets(off, end) {
		return false
	}

	if _, _, err := r.fakeJournal.At(kit.OpSync, []byte(f.name)); err == nil {
		return false
	}

	r.fake.FailNextWrite(journal.NewFileID(r.ids[i]), true)

	return true
}

// resend is the NFS client's answer to a Commit of file i that answered
// verifier v: it writes again, unstable, every write of this process that
// carries another verifier and no Commit acknowledged, then commits again,
// as a client resends after a changed verifier. The oracle checks after the
// next restart that every byte it stabilised reads back.
func (r *soakRun) resend(ctx context.Context, i int, v vfs.Verifier) {
	f, h := r.m.files[i], r.files[i]

	var again []*soakOp

	for _, o := range f.ops {
		if o.kind == opWrite && o.done && !o.acked && o.proc == r.proc && o.verifier != v {
			again = append(again, o)
		}
	}

	if len(again) == 0 {
		return
	}

	for _, o := range again {
		desc := fmt.Sprintf("resend %s [%d, %d)", f.name, o.off, o.off+int64(len(o.data)))
		r.log.set(r.calls, desc, false)

		start := time.Now()

		var (
			res vfs.WriteResult
			err error
		)

		r.waiting(func() {
			res, err = r.c.fs.Write(ctx, r.c.admin, vfs.Anonymous(h), o.off, o.data, vfs.Unstable)
		})
		r.log.set(0, "", false)
		r.slow(desc, start)

		op := &soakOp{kind: opWrite, off: o.off, data: o.data, proc: r.proc, done: err == nil}
		op.verifier = res.Verifier
		f.add(op, false)

		if !r.excused(err, r.attribute(op)) {
			r.fail("call %d, %s: %v", r.calls, desc, err)
		}

		if err != nil {
			return
		}

		r.observe(res.Verifier)

		gctx, gcancel := r.bounded()
		attr, aerr := r.c.fs.GetAttr(gctx, r.c.admin, h)

		gcancel()

		if aerr != nil {
			r.fail("GetAttr of %s after resending in call %d: %v", f.name, r.calls, aerr)
		}

		op.mtime = attr.Modify
	}

	desc := fmt.Sprintf("commit %s after resending", f.name)
	r.log.set(r.calls, desc, false)

	start := time.Now()

	var (
		v2  vfs.Verifier
		err error
	)

	r.waiting(func() {
		v2, err = r.c.fs.Commit(ctx, r.c.admin, vfs.Anonymous(h), 0, 0)
	})
	r.log.set(0, "", false)
	r.slow(desc, start)

	faulted := r.attribute()
	f.commit(r.proc, 0, 0, v2, err)

	if !r.excused(err, faulted) {
		r.fail("call %d, %s: %v", r.calls, desc, err)
	}

	if err != nil {
		return
	}

	r.observe(v2)
	r.resends++
}

// failedWindows reads the windows the fake failed since the last read and
// puts each in the oracle, under the process running now.
func (r *soakRun) failedWindows() {
	if r.fake == nil {
		return
	}

	for _, ev := range r.fake.Stats().Losses {
		if ev.SeqNum <= r.lossSeen {
			continue
		}

		r.lossSeen = ev.SeqNum
		if ev.Reason != journal.LossFailedWindow {
			continue
		}

		for i, id := range r.ids {
			if journal.NewFileID(id) == ev.File {
				// The event names the dropped write's extent, which picks it
				// among the file's condemned writes.
				var w *soakOp

				for k, o := range r.doomed[i] {
					if o.off == ev.Extent.Off && int64(len(o.data)) == ev.Extent.Len {
						w = o
						r.doomed[i] = slices.Delete(r.doomed[i], k, k+1)

						break
					}
				}

				r.m.failWindow(r.m.files[i], r.proc, w)
			}
		}
	}
}

// meets reports whether [off, end) meets any write of f, so that a failed
// window over a write there would have older content to hold again.
func (f *fileModel) meets(off, end int64) bool {
	for _, o := range f.ops {
		if o.kind == opWrite && o.off < end && off < o.off+int64(len(o.data)) {
			return true
		}
	}

	return false
}

// attribute credits the faults fired since the last call to the oracle, and
// reports whether any of them failed its operation: one fired in the call
// just made, or with no call in flight since the one before. A lost record in
// call k is call k's own when k is a write; in any other call it is credited
// to every write since its file's last sync, since the soak cannot tell whose
// record it was. One with no call in flight is credited to no range. A failed
// sync makes every file with an unacknowledged write owe one failure. own is
// the op the call k issued, if any.
func (r *soakRun) attribute(own ...*soakOp) bool {
	failed := false
	for _, f := range r.log.since(r.seen) {
		r.seen++
		// Over the in-memory journal a sync fault is only the draw that dooms
		// a write's window, which fails nothing until a later Sync answers it
		// as a changed verifier, so it excuses no error.
		doom := r.fake != nil && f.side == "journal" && f.op == kit.OpSync
		failed = failed || f.err != nil && !doom
		// Over the in-memory journal the run knows which file a failed
		// window held and requires its report (failedWindows); over
		// the real journal it cannot tell, so it only excuses one failure of
		// every file with an unacknowledged write.
		if f.side == "journal" && f.op == kit.OpSync && f.err != nil && r.fake == nil {
			r.m.syncFailed()
		}

		switch {
		case !f.lostRecord():
		case f.call == 0:
			r.m.unattributed++
		case len(own) > 0 && own[0] != nil && own[0].kind == opWrite && f.call == r.calls:
			own[0].faulted = true
		default:
			r.m.credit(r.proc)
		}
	}

	return failed
}

// excused reports whether a call may answer err: success; ErrDelay, the
// retry a failed window, a refusal or the group commit's backoff after a
// store fault answers (RFC 8 §11.2's own answer); or any other error when a fault
// that failed an operation fired since the call before (faulted). A lost or
// torn record fails no operation, so it excuses nothing, and an answer the
// call's backstop cut off is a wedge, never excused.
func (r *soakRun) excused(err error, faulted bool) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return false
	}

	return faulted || errors.Is(err, vfs.ErrDelay)
}

// commitExcused is excused for a Commit's answer: an SMB open across a loss
// found in its process may also answer ErrLost, whatever Commit of the file
// came between (lostOpen).
func (r *soakRun) commitExcused(err error, faulted, lostOpen bool) bool {
	return r.excused(err, faulted || lostOpen && errors.Is(err, vfs.ErrLost))
}

// bounded is a context cut off after soakCallBound, for one call of the
// soak's own; the caller cancels it when the call returns.
func (r *soakRun) bounded() (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.t.Context(), soakCallBound)
}

// slow fails a call that took soakSlowCall or longer from start, whatever it
// answered: the clock moves under a waiting call, so a call that still takes
// this long waits on something no timer of the engine ends, a wedge.
func (r *soakRun) slow(desc string, start time.Time) {
	if err := stalled(desc, time.Since(start)); err != nil {
		r.fail("%v", err)
	}
}

// truncateGrows reports whether a truncate to size is a grow to the engine:
// a size at or past the one the service reports for the file just before
// the call, which is what the engine's replayed operations leave after any
// crash, not what every call that answered would leave.
func truncateGrows(size int64, reported vfs.File) bool {
	return size >= reported.Size
}

// stalled is slow's verdict on a call that took d: no fault before it and no
// answer it gave excuses it.
func stalled(desc string, d time.Duration) error {
	if d < soakSlowCall {
		return nil
	}

	return fmt.Errorf("%s took %v, parked until it was cut off", desc, d)
}

// waiting runs call, a client call, and moves the test clock while it waits:
// every soakTick of real time, by a step the seed's clock stream draws, up to
// twice ExistenceAge as between calls. A call waiting on the engine's timers,
// such as an existence commit it retries after the store failed it, gets them
// as it would on a running clock; a call waiting on anything else still meets
// its own deadline.
func (r *soakRun) waiting(call func()) {
	done, moved := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(moved)

		for {
			select {
			case <-done:
				return
			case <-time.After(soakTick):
				r.clock.Advance(
					time.Duration(r.clockSteps.Int64N(int64(2 * enginetest.Settings.ExistenceAge))),
				)
			}
		}
	}()

	call()
	close(done)
	<-moved
}

// check reads every file back after a restart: its size and modify time
// first, then each piece of it, judged by the oracle. A read that drops a
// record as corrupt finds a loss in this process, after its SMB opens were
// taken, so the file's open must report it on its next flush. Last, the
// verifier the new process answers must differ from every earlier one's.
func (r *soakRun) check() {
	for i, h := range r.files {
		f := r.m.files[i]
		ctx, cancel := r.bounded()
		attr, err := r.c.fs.GetAttr(ctx, r.c.admin, h)
		cancel()
		if err != nil {
			r.fail("process %d: GetAttr of %s: %v", r.proc, f.name, err)
		}

		if err := f.stat(attr.Size, attr.Modify); err != nil {
			r.fail("process %d: %v", r.proc, err)
		}

		found := false
		for _, p := range f.pieces(attr.Size) {
			buf := make([]byte, p[1]-p[0])
			rctx, rcancel := r.bounded()
			n, _, err := r.c.fs.Read(rctx, r.c.admin, vfs.Anonymous(h), p[0], buf)
			rcancel()
			corrupt := errors.Is(err, vfs.ErrCorrupt)
			lost := corrupt || errors.Is(err, vfs.ErrLost)
			// A loss a restart found may raise the loss sequence at the next
			// Commit's drain; a record dropped as corrupt by this read raises
			// it in this process, after the opens, when an answered write
			// was dropped.
			if lost {
				f.owes = true
			}

			if corrupt && f.answered(p[0]) {
				r.m.lose(f, r.proc)
				found = true
			}

			switch {
			case err != nil && !lost:
				r.fail("process %d: read %s [%d, %d): %v", r.proc, f.name, p[0], p[1], err)
			case err == nil && n < len(buf):
				r.fail("process %d: read %s [%d, %d) answered %d bytes inside its size %d",
					r.proc, f.name, p[0], p[1], n, attr.Size)
			}

			if err := r.m.judge(f, p[0], buf, lost); err != nil {
				r.fail("process %d: %v", r.proc, err)
			}
		}

		if found {
			cctx, ccancel := r.bounded()
			start := time.Now()

			var (
				v   vfs.Verifier
				err error
			)

			r.calls++
			r.log.set(r.calls, "check flush of "+f.name, false)
			r.waiting(func() {
				v, err = r.c.fs.Commit(cctx, r.smb, r.refs[i], 0, 0)
			})
			r.log.set(0, "", false)
			ccancel()
			r.slow("the flush of "+f.name+" after its loss", start)
			// Attribute first, so a record lost in this Commit credits the
			// writes it has not acknowledged yet.
			faulted, owed := r.attribute(), f.owed()
			if ferr := r.m.flush(r.opens[i], v, err, true); ferr != nil {
				r.fail("process %d: %v", r.proc, ferr)
			}

			if !r.excused(err, faulted || owed) {
				r.fail("process %d: the flush of %s after its loss: %v", r.proc, f.name, err)
			}
		}
	}

	vctx, vcancel := r.bounded()
	start := time.Now()

	var (
		v   vfs.Verifier
		err error
	)

	r.calls++
	r.log.set(r.calls, "check verifier Commit", false)
	r.waiting(func() {
		v, err = r.c.fs.Commit(vctx, r.c.admin, vfs.Anonymous(r.files[0]), 0, 0)
	})
	r.log.set(0, "", false)
	vcancel()
	r.slow("the verifier's Commit", start)
	faulted := r.attribute()
	owed := r.m.files[0].owed()
	if err != nil {
		if r.excused(err, faulted || owed) {
			return // a fault failed it; the next restart checks the verifier
		}

		r.fail("process %d: the verifier's Commit: %v", r.proc, err)
	}

	r.m.files[0].commit(r.proc, 0, 0, v, nil)
	r.observe(v)
}

func (r *soakRun) observe(v vfs.Verifier) {
	if err := r.m.observe(r.proc, v); err != nil {
		r.fail("%v", err)
	}
}

func (r *soakRun) fail(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("seed %d: "+format, append([]any{r.seed}, args...)...)
}

// TestSoakExcused holds the rule for which errors a soak call may answer: a
// call cut off by its backstop is a wedge whatever fired, and an ordinary
// error needs a fault that failed an operation.
func TestSoakExcused(t *testing.T) {
	r := &soakRun{}
	cases := []struct {
		name    string
		err     error
		faulted bool
		want    bool
	}{
		{"success", nil, false, true},
		{"ErrDelay with no fault", vfs.ErrDelay, false, true},
		{"an error with no fault", errInjected, false, false},
		{"an error after a failing fault", errInjected, true, true},
		{"the backstop cut it off, with a failing fault", context.DeadlineExceeded, true, false},
		{"cancelled, with a failing fault", context.Canceled, true, false},
	}

	for _, c := range cases {
		if got := r.excused(c.err, c.faulted); got != c.want {
			t.Errorf("%s: excused %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSoakAttribute holds that only a fault that failed an operation, fired
// since the call before, excuses a call's error: a lost record fails nothing,
// and a fault an earlier call already met excuses no later call.
func TestSoakAttribute(t *testing.T) {
	r := &soakRun{log: &faultLog{}, m: newSoakModel(), proc: 1}
	fire := func(call int, f firedFault) {
		f.call = call
		r.log.fired = append(r.log.fired, f)
	}

	r.calls = 1
	fire(1, firedFault{side: "journal", op: kit.OpWrite, tear: sim.Lose})
	if r.attribute() {
		t.Error("call 1: a lost record excuses its error")
	}

	r.calls = 2
	fire(2, firedFault{side: "store", op: kvtest.OpUpdate, err: errInjected})
	if !r.attribute() {
		t.Error("call 2: a failed store update in the call does not excuse its error")
	}

	r.calls = 3
	if r.attribute() {
		t.Error("call 3: call 2's fault excuses a later call's error")
	}

	// Where each fault lands in the oracle.
	f := r.m.file("f", time.Time{})
	pending := f.add(&soakOp{kind: opWrite, data: []byte("a"), proc: 1, done: true}, false)

	r.calls = 4
	own := &soakOp{kind: opWrite, data: []byte("b"), proc: 1, done: true}
	fire(4, firedFault{side: "journal", op: kit.OpWrite, tear: sim.Lose})
	r.attribute(own)
	if !own.faulted || pending.faulted {
		t.Errorf(
			"call 4, a write losing its own record: own faulted %v, the other write faulted %v; want true, false",
			own.faulted,
			pending.faulted,
		)
	}

	r.calls = 5
	fire(5, firedFault{side: "journal", op: kit.OpWrite, tear: sim.Lose})
	r.attribute(&soakOp{kind: opTruncate, proc: 1, done: true})
	if !pending.faulted {
		t.Error(
			"call 5, a truncate meeting a lost record: the unacknowledged write is not credited",
		)
	}

	fire(0, firedFault{side: "journal", op: kit.OpWrite, tear: sim.Lose})
	r.attribute()
	if r.m.unattributed != 1 {
		t.Errorf("a lost record with no call in flight: unattributed %d, want 1", r.m.unattributed)
	}

	r.calls = 6
	fire(6, firedFault{side: "journal", op: kit.OpSync, err: errInjected})
	r.attribute()
	if !f.owed() {
		t.Error("call 6, a failed sync: the file with an unacknowledged write owes no failure")
	}

	// Which faults lose the call's own record: a silent tear does, a torn
	// write that failed does not (the call failed), and a fault on another
	// op than a write does not.
	for i, c := range []struct {
		name string
		f    firedFault
		want bool
	}{
		{"a silent tear", firedFault{side: "journal", op: kit.OpWrite, tear: 16}, true},
		{"a torn write that failed", firedFault{side: "journal", op: kit.OpWrite, tear: 16, err: errInjected}, false},
		{"a lost sync", firedFault{side: "journal", op: kit.OpSync, tear: sim.Lose}, false},
	} {
		r.calls = 7 + i
		mine := &soakOp{kind: opWrite, data: []byte("c"), proc: 1, done: true}
		fire(r.calls, c.f)
		r.attribute(mine)
		if mine.faulted != c.want {
			t.Errorf(
				"call %d, %s in the call's own write: own faulted %v, want %v",
				r.calls,
				c.name,
				mine.faulted,
				c.want,
			)
		}
	}
}

// pieces splits [0, size) at every offset where an op of f starts or ends, so
// each piece reads as one thing: its bytes, or Lost.
func (f *fileModel) pieces(size int64) [][2]int64 {
	cuts := []int64{0, size}
	for _, o := range f.ops {
		switch o.kind {
		case opWrite:
			cuts = append(cuts, o.off, o.off+int64(len(o.data)))
		case opTruncate:
			cuts = append(cuts, o.off)
		case opDeallocate:
			cuts = append(cuts, o.off, o.off+o.n)
		}
	}

	slices.Sort(cuts)
	cuts = slices.Compact(cuts)

	var ps [][2]int64
	for i := 0; i+1 < len(cuts) && cuts[i] < size; i++ {
		ps = append(ps, [2]int64{cuts[i], min(cuts[i+1], size)})
	}

	return ps
}

// TestSoakStalled holds that nothing excuses a stalled call any more: a call
// that took the slow bound fails, whatever it answered and whatever store or
// sync fault came before it, since the clock moves while a call waits.
func TestSoakStalled(t *testing.T) {
	if err := stalled("a quick call", soakSlowCall-time.Millisecond); err != nil {
		t.Errorf("a call under the slow bound: %v, want no failure", err)
	}

	for _, d := range []time.Duration{soakSlowCall, soakCallBound} {
		if stalled("a parked call", d) == nil {
			t.Errorf("a call that took %v is not failed", d)
		}
	}
}

// TestSoakWaitingMovesTheClock holds the loop's clock rule: a call waiting on
// the test clock gets it moved by the seed's steps, and a call waiting on
// anything else still meets its own deadline and is failed as stalled.
func TestSoakWaitingMovesTheClock(t *testing.T) {
	r := &soakRun{
		clock:      sim.NewTestClock(time.Unix(1000, 0)),
		clockSteps: rand.New(rand.NewPCG(1, 1)),
		rng:        rand.New(rand.NewPCG(1, 0)),
	}

	ctx, cancel := context.WithTimeout(t.Context(), soakCallBound)
	defer cancel()

	start := time.Now()

	var err error

	r.waiting(func() {
		select {
		case <-r.clock.After(10 * time.Minute):
		case <-ctx.Done():
			err = ctx.Err()
		}
	})

	if err != nil {
		t.Errorf(
			"a call waiting ten minutes on the test clock: %v after %v, "+
				"want it woken by the moved clock",
			err,
			time.Since(start),
		)
	}

	// The clock's steps come from their own stream, so a wait never changes
	// the calls the seed draws after it.
	if got, want := r.rng.Uint64(), rand.New(rand.NewPCG(1, 0)).Uint64(); got != want {
		t.Errorf(
			"the calls' stream after a wait drew %d, want %d: the wait drew from it",
			got,
			want,
		)
	}

	start = time.Now()
	wedge, cancelWedge := context.WithTimeout(t.Context(), soakSlowCall)
	defer cancelWedge()

	r.waiting(func() {
		<-wedge.Done()
	})

	if stalled("a wedged call", time.Since(start)) == nil {
		t.Errorf("a call waiting on no timer of the clock returned after %v and is not failed",
			time.Since(start))
	}
}

// TestSoakTruncateGrows holds the grow rule: a truncate to the size the
// service reports, or past it, is a grow, and one below it a shrink, whatever
// the writes a crash dropped would have left.
func TestSoakTruncateGrows(t *testing.T) {
	for _, c := range []struct {
		size, reported int64
		want           bool
	}{
		{32, 8, true},   // a crash dropped a write to 64: the engine sees 8
		{8, 8, true},    // the same size is a grow, as the engine counts it
		{7, 8, false},   // below the reported size is a shrink
		{32, 64, false}, // the service still holds 64
	} {
		if got := truncateGrows(c.size, vfs.File{Size: c.reported}); got != c.want {
			t.Errorf("a truncate to %d with %d reported: grow %v, want %v",
				c.size, c.reported, got, c.want)
		}
	}
}

// TestSoakCommitExcused holds that an SMB open's flush may answer ErrLost
// only across a loss found in its process.
func TestSoakCommitExcused(t *testing.T) {
	r := &soakRun{}
	if !r.commitExcused(vfs.ErrLost, false, true) {
		t.Error("ErrLost through an open across a loss is not excused")
	}

	if r.commitExcused(vfs.ErrLost, false, false) {
		t.Error("ErrLost through an open with no loss after it is excused")
	}

	if r.commitExcused(errInjected, false, true) {
		t.Error("another error through an open across a loss is excused")
	}
}

// TestSoakFaultFake holds what the run does to the in-memory journal, and to
// the oracle, when the journal schedule fires around a write: a lost record is
// the fake's Lose and a loss each earlier open owes; a failed window is drawn
// before an unstable append or hole fill, never an overwrite or a stable
// write, fails at the next Sync, and is then a window the file's next
// stability point must report.
func TestSoakFaultFake(t *testing.T) {
	type world struct {
		r *soakRun
		f *fileModel
		o *smbOpen
		h interface {
			WriteAt(
				ctx context.Context, id journal.FileID, off int64, p []byte, mtime int64,
			) (journal.Version, error)
			Sync(ctx context.Context, ids ...journal.FileID) ([]entity.SyncMark, error)
		}
		id journal.FileID
	}

	setup := func(t *testing.T, action sim.FaultAction, op string, earlier int64) world {
		t.Helper()

		sched, err := sim.NewSchedule(1, nil, sim.Rule{Op: op, Key: "*", Action: action})
		if err != nil {
			t.Fatal(err)
		}

		r := &soakRun{
			log:  &faultLog{},
			m:    newSoakModel(),
			proc: 1,
			fake: enginetest.NewFakeJournal(),
		}
		r.fakeJournal = loggedFault{log: r.log, side: "journal", inner: sched}
		r.ids = []entity.FileID{{1}}
		f := r.m.file("f", time.Time{})

		if err := r.fake.SetVersionLimit(journal.Version{Counter: 1 << 32}); err != nil {
			t.Fatal(err)
		}

		h, err := r.fake.Share(1, 0, journal.Version{})
		if err != nil {
			t.Fatal(err)
		}

		// earlier, when not negative, is where an earlier write of 3 bytes
		// sits in the model.
		if earlier >= 0 {
			f.add(
				&soakOp{kind: opWrite, off: earlier, data: []byte("old"), proc: 1, done: true},
				false,
			)
		}

		return world{r: r, f: f, o: r.m.open(f, 1), h: h, id: journal.NewFileID(r.ids[0])}
	}

	// write writes 3 bytes at 3 through the fake, then closes the window.
	write := func(t *testing.T, w world) {
		t.Helper()

		if _, err := w.h.WriteAt(t.Context(), w.id, 3, []byte("new"), 0); err != nil {
			t.Fatal(err)
		}

		if _, err := w.h.Sync(t.Context(), journal.NewFileID(entity.FileID{2})); err != nil {
			t.Fatal(err)
		}

		w.r.failedWindows()
	}

	t.Run("a lost record", func(t *testing.T) {
		w := setup(t, sim.FaultAction{TearAfter: sim.Lose}, kit.OpWrite, -1)
		write(t, w)
		op := w.f.add(
			&soakOp{kind: opWrite, off: 3, data: []byte("new"), proc: 1, done: true},
			false,
		)
		w.r.faultFake(0, op)

		if !w.r.m.owesLost(w.o) {
			t.Error("an open taken before a lost record owes no ErrLost")
		}

		if !w.f.owed() {
			t.Error("a lost record leaves the file's next Commit owing nothing")
		}
	})

	t.Run("a failed window", func(t *testing.T) {
		w := setup(t, sim.FaultAction{Err: errInjected}, kit.OpSync, -1)
		if !w.r.doomWrite(0, 3, 6, false) {
			t.Fatal("a failed window drawn over an append was not armed")
		}

		if w.r.attribute() {
			t.Error("the draw that dooms a write's window excuses the write's own error")
		}

		op := w.f.add(
			&soakOp{kind: opWrite, off: 3, data: []byte("new"), proc: 1, done: true},
			false,
		)
		w.r.doomed = map[int][]*soakOp{0: {op}}
		write(t, w)

		if !op.dropped {
			t.Error("the write whose window failed may still read back")
		}

		if w.r.fake.Durable(w.id) || !w.r.m.owesLost(w.o) {
			t.Errorf("after the window's Sync: durable %v, the open may answer ErrLost %v; "+
				"want undurable, ErrLost", w.r.fake.Durable(w.id), w.r.m.owesLost(w.o))
		}

		if err := w.f.nfsCommit(1, vfs.Verifier{}, nil); err == nil {
			t.Error("the file's next Commit may answer the dropped write's verifier " +
				"after its failed window")
		}
	})

	t.Run("two failed windows of one file before a Sync", func(t *testing.T) {
		w := setup(t, sim.FaultAction{Err: errInjected}, kit.OpSync, -1)
		w.r.doomed = map[int][]*soakOp{}

		var ops []*soakOp

		for k, off := range []int64{3, 10} {
			if !w.r.doomWrite(0, off, off+3, false) {
				t.Fatalf("a failed window drawn over [%d, %d) was not armed", off, off+3)
			}

			if _, err := w.h.WriteAt(t.Context(), w.id, off, []byte("new"), 0); err != nil {
				t.Fatal(err)
			}

			op := w.f.add(&soakOp{
				kind:     opWrite,
				off:      off,
				data:     []byte("new"),
				proc:     1,
				done:     true,
				verifier: vfs.Verifier{byte(k + 1)},
			}, false)
			w.r.doomed[0] = append(w.r.doomed[0], op)
			ops = append(ops, op)
		}

		if _, err := w.h.Sync(t.Context(), journal.NewFileID(entity.FileID{2})); err != nil {
			t.Fatal(err)
		}

		w.r.failedWindows()

		for _, op := range ops {
			if !op.dropped {
				t.Errorf("the write at %d whose window failed may still read back", op.off)
			}
		}

		if w.f.window == nil || w.f.window.verifier == (vfs.Verifier{}) {
			t.Error("the second failed window lost the verifier of the write it dropped")
		}
	})

	t.Run("a disarmed window takes no later write", func(t *testing.T) {
		w := setup(t, sim.FaultAction{Err: errInjected}, kit.OpSync, -1)
		if !w.r.doomWrite(0, 3, 6, false) {
			t.Fatal("a failed window drawn over an append was not armed")
		}

		w.r.fake.FailNextWrite(w.id, false)
		write(t, w)

		if !w.r.fake.Durable(w.id) || w.r.m.owesLost(w.o) {
			t.Error("a window disarmed after a write that appended nothing failed the next one")
		}
	})

	for _, c := range []struct {
		name    string
		earlier int64
		stable  bool
		want    bool
	}{
		{"no failed window over an overwrite", 1, false, false},
		{"no failed window over a stable write", -1, true, false},
		{"a failed window on an append past an earlier write", 20, false, true},
		{"a failed window on a write right after an earlier one", 0, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := setup(t, sim.FaultAction{Err: errInjected}, kit.OpSync, c.earlier)
			if got := w.r.doomWrite(0, 3, 6, c.stable); got != c.want {
				t.Errorf(
					"a failed window drawn over [3, 6), an earlier write at %d, "+
						"stable %v: armed %v, want %v",
					c.earlier,
					c.stable,
					got,
					c.want,
				)
			}
		})
	}
}
