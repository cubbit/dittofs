//go:build engine_conformance && engine_soak

package integration

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
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

const (
	soakFiles     = 3                // files each seed writes
	soakRounds    = 4                // restarts each seed runs, each after soakCalls calls
	soakCalls     = 16               // client calls between two restarts
	soakSpan      = 64 << 10         // the offsets the calls reach in a file
	soakMaxWrite  = 4 << 10          // the largest write
	soakCallBound = 10 * time.Second // a real-time backstop on one call, so a wedge fails instead of hanging
	soakSlowCall  = 2 * time.Second  // a call this slow was parked, whatever it answered
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
			runSoak(t, seed)
		})
	}
}

// firedFault is one fault a schedule fired: where, on what, and the client
// call in flight, 0 for none.
type firedFault struct {
	side, op, key string
	tear          int
	err           error
	call          int
	desc          string
}

// faultLog records every fault the seed's schedules fire, in order, with the
// client call in flight when each fired. The writer sets the call; the
// engine's goroutines consult the schedules, so every field is under mu.
type faultLog struct {
	mu     sync.Mutex
	call   int
	desc   string
	paused bool
	fired  []firedFault
}

// loggedFault is a schedule whose every non-zero answer goes to log.
type loggedFault struct {
	log   *faultLog
	side  string
	inner sim.Fault
}

func (l loggedFault) At(op string, key []byte) (time.Duration, int, error) {
	// One lock covers the pause, the draw and the record, so a fault drawn
	// while paused is never logged, and one drawn while running is logged
	// with the call in flight when it was drawn.
	l.log.mu.Lock()
	defer l.log.mu.Unlock()

	// A fault while the fixture restarts fails the fixture, not the engine.
	if l.log.paused {
		return 0, 0, nil
	}

	d, tear, err := l.inner.At(op, key)
	if tear != 0 || err != nil {
		l.log.fired = append(l.log.fired, firedFault{
			side: l.side,
			op:   op,
			key:  string(key),
			tear: tear,
			err:  err,
			call: l.log.call,
			desc: l.log.desc,
		})
	}

	return d, tear, err
}

func (l *faultLog) set(call int, desc string, paused bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.call, l.desc, l.paused = call, desc, paused
}

// since is every fault fired after the first n.
func (l *faultLog) since(n int) []firedFault {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]firedFault(nil), l.fired[n:]...)
}

func (l *faultLog) String() string {
	var b strings.Builder
	for i, f := range l.since(0) {
		in := "no client call in flight"
		if f.call != 0 {
			in = fmt.Sprintf("in call %d, %s", f.call, f.desc)
		}

		fmt.Fprintf(
			&b,
			"  %d. %s %s %s: tear %d, err %v, %s\n",
			i+1,
			f.side,
			f.op,
			f.key,
			f.tear,
			f.err,
			in,
		)
	}

	return b.String()
}

// lostRecord reports whether f made a journal record not durable: a lost
// write, or one torn with no error.
func (f firedFault) lostRecord() bool {
	return f.side == "journal" && f.op == kit.OpWrite && (f.tear < 0 || f.tear > 0 && f.err == nil)
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
}

func runSoak(t *testing.T, seed uint64) {
	r := &soakRun{
		t:    t,
		seed: seed,
		c:    compose(t),
		rng:  rand.New(rand.NewPCG(seed, 0)),
		log:  &faultLog{},
		m:    newSoakModel(),
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
		h, _ := r.c.File(name)
		ctx, cancel := r.bounded()
		attr, err := r.c.fs.GetAttr(ctx, r.c.admin, h)
		cancel()
		if err != nil {
			t.Fatalf("seed %d: GetAttr of the new %s: %v", seed, name, err)
		}

		r.files = append(r.files, h)
		r.m.file(name, attr.Modify)
	}

	journal, err := sim.NewSchedule(seed, r.c.Clock, soakJournalRules...)
	if err != nil {
		t.Fatal(err)
	}

	store, err := sim.NewSchedule(seed, r.c.Clock, soakStoreRules...)
	if err != nil {
		t.Fatal(err)
	}

	r.c.Seam.SetFault(loggedFault{log: r.log, side: "journal", inner: journal})
	t.Cleanup(func() {
		r.c.Seam.SetFault(nil)
	})
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
	for _, f := range r.log.since(0) {
		sides[f.side]++
	}

	if sides["journal"] == 0 || sides["store"] == 0 {
		r.fail(
			"the schedules fired %d journal and %d store faults; a seed must fire at least one of each",
			sides["journal"],
			sides["store"],
		)
	}
}

// startProcess counts a new process and opens every file through a new SMB
// client of it, with the faults paused.
func (r *soakRun) startProcess() {
	r.log.set(0, "", true)
	defer r.log.set(0, "", false)

	r.proc++
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
		r.c.Crash(kit.Reordered(r.rng.Uint64()))
	default:
		r.c.Crash(kit.Torn(1 + r.rng.IntN(16)))
	}

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
		// file's next stability point, which a failed window fails once.
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
		var res vfs.WriteResult
		res, err = r.c.fs.Write(ctx, r.c.admin, vfs.Anonymous(h), off, data, how)
		op = &soakOp{
			kind:     opWrite,
			off:      off,
			data:     data,
			proc:     r.proc,
			done:     err == nil,
			verifier: res.Verifier,
		}
		f.add(op, stable)
		if err == nil {
			r.observe(res.Verifier)
		}
	case k < 62:
		commit = true
		desc = fmt.Sprintf("commit %s", f.name)
		r.log.set(r.calls, desc, false)
		var v vfs.Verifier
		v, err = r.c.fs.Commit(ctx, r.c.admin, vfs.Anonymous(h), 0, 0)
		record = func() {
			f.commit(r.proc, 0, 0, v, err)
			if err == nil {
				r.observe(v)
			}
		}
	case k < 74:
		commit = true
		desc = fmt.Sprintf("commit %s through an SMB open", f.name)
		r.log.set(r.calls, desc, false)
		var v vfs.Verifier
		v, err = r.c.fs.Commit(ctx, r.smb, r.refs[i], 0, 0)
		record = func() {
			if ferr := r.m.flush(r.opens[i], v, err); ferr != nil {
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
		grow := size >= f.leaves()
		_, err = r.c.fs.SetAttr(
			ctx,
			r.c.admin,
			vfs.Target{H: h},
			vfs.AttrChange{Attrs: entity.Attrs{}.WithSize(size)},
		)
		op = f.add(
			&soakOp{kind: opTruncate, off: size, proc: r.proc, done: err == nil, grow: grow},
			false,
		)
	default:
		off, n := r.rng.Int64N(soakSpan), 1+r.rng.Int64N(soakMaxWrite)
		desc = fmt.Sprintf("deallocate %s [%d, %d)", f.name, off, off+n)
		r.log.set(r.calls, desc, false)
		_, err = r.c.fs.Deallocate(ctx, r.c.admin, vfs.Anonymous(h), off, n)
		op = f.add(
			&soakOp{kind: opDeallocate, off: off, n: n, proc: r.proc, done: err == nil},
			false,
		)
	}

	r.log.set(0, "", false)
	r.slow(desc, start)
	faulted := r.attribute(op)
	if record != nil {
		record()
	}

	// A failed sync window, or a loss a restart found, may fail the file's
	// next Commit once, through an SMB open as ErrLost; the one after owes
	// nothing.
	owed := commit && f.owed()
	if !r.excused(err, faulted || owed) {
		r.fail("call %d, %s: %v", r.calls, desc, err)
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
		failed = failed || f.err != nil
		if f.side == "journal" && f.op == kit.OpSync && f.err != nil {
			r.m.syncFailed()
		}

		switch {
		case !f.lostRecord():
		case f.call == 0:
			r.m.unattributed++
		case len(own) > 0 && own[0] != nil && own[0].kind == opWrite && f.call == r.calls:
			own[0].faulted = true
		default:
			r.m.credit()
		}
	}

	return failed
}

// excused reports whether a call may answer err: success; ErrDelay, the
// retry a failed window or a refusal answers; or any other error when a fault
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

// bounded is a context cut off after soakCallBound, for one call of the
// soak's own; the caller cancels it when the call returns.
func (r *soakRun) bounded() (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.t.Context(), soakCallBound)
}

// slow fails a call that took soakSlowCall or longer from start. The clock
// moves only between calls, so a call parked on it comes back only at the
// backstop, and with a sync fault pending it comes back with the owed
// failure, which excused() would take for a fault's answer.
func (r *soakRun) slow(desc string, start time.Time) {
	if d := time.Since(start); d >= soakSlowCall {
		r.fail("%s took %v, parked until it was cut off", desc, d)
	}
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
			v, err := r.c.fs.Commit(cctx, r.smb, r.refs[i], 0, 0)
			ccancel()
			r.slow("the flush of "+f.name+" after its loss", start)
			// Attribute first, so a record lost in this Commit credits the
			// writes it has not acknowledged yet.
			faulted, owed := r.attribute(), f.owed()
			if ferr := r.m.flush(r.opens[i], v, err); ferr != nil {
				r.fail("process %d: %v", r.proc, ferr)
			}

			if !r.excused(err, faulted || owed) {
				r.fail("process %d: the flush of %s after its loss: %v", r.proc, f.name, err)
			}
		}
	}

	vctx, vcancel := r.bounded()
	start := time.Now()
	v, err := r.c.fs.Commit(vctx, r.c.admin, vfs.Anonymous(r.files[0]), 0, 0)
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
	r := &soakRun{log: &faultLog{}, m: newSoakModel()}
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
