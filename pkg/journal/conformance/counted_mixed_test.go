//go:build journal_conformance

package conformance

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/journal/journaltest/kit"
)

// This file is the mixed-durability check's round protocol as one code path
// with two drivers. TestCounted_MixedDurability drives it against a fixture's
// journal; TestCounted_MixedChoreography drives the same runMixedRounds
// against fakes that answer the ways the designs RFC 1 §5.2 permits do. What
// each design measures, the feeds it is handed, the teardown that ends the
// run — all of it is this code, so a protocol bug fails the choreography
// against a fake it can see, not a stubbed journal it cannot.

// mixedTB is the testing handle the protocol needs: the check hands it its
// *testing.T, and the choreography hands a catcher to designs expected to
// fail, which records the Fatalf instead of ending the run.
type mixedTB interface {
	Helper()
	Cleanup(func())
	Fatalf(string, ...any)
	Errorf(string, ...any)
}

// stubCall fails the run when err is the journal's ErrNotImplemented, so a
// check breaking on a call the stub journal never wrote never reads as
// failing its own assertion. The run's caller may be the check's *testing.T
// or the choreography's catcher — any mixedTB does.
func stubCall(t mixedTB, call string, err error) {
	if errors.Is(err, journal.ErrNotImplemented) {
		t.Fatalf("stub: %s", call)
	}
}

// mixedEnv is what runMixedRounds needs from the journal side. The check
// implements it over a fixture's journal and seam; the choreography
// implements it over a fake that answers as one journal design would.
//
// The env's calls run on the protocol's goroutines exactly as the journal's
// would: Append on the stream goroutine, SyncSmall on the measured sync's
// own goroutine, the rest on the run's. No method fails the test itself: it
// reports its error, and the protocol reads it where it can fail the run.
type mixedEnv interface {
	// Install makes the given gate the facility that parks the journal's
	// segment syncs. The check sets it as the seam's fault, so a real
	// journal's fsyncs park at storage; a fake parks on it itself from its
	// own sync paths.
	Install(gate *kit.SimGate)

	// Append lands the stream's next interval on the stream goroutine and
	// reports whether the stream has another one to land. The interval that
	// ends a file runs the file's closing sync inside Append — as the
	// journal's own stream goroutine would — before returning.
	Append() (more bool, err error)

	// EndsFile reports whether interval i, counting the stream's intervals
	// from 0, is the last of its file.
	EndsFile(i int) bool

	// FileStarts lists the interval each streamed file starts at, counting
	// the stream's intervals from 0, in the files' order: the interval
	// after a file's last one starts the next file.
	FileStarts() []int

	// Intervals is how many intervals the stream lands in all. The protocol
	// consumes exactly one per round plus whatever it feeds, and never runs
	// a round the stream cannot open.
	Intervals() int

	// SyncStream runs the journal's own sync of the streamed file, on the
	// run's goroutine, ungated: a sync the rules must answer by flushing
	// everything the stream has written, so nothing of the stream's is
	// pending into the measured window.
	SyncStream() error

	// WriteSmall lands the small writer's record after SyncStream emptied
	// the stream's segment, so it stays pending into the measured window:
	// the measured Sync has a record of its own to answer for.
	WriteSmall() error

	// SyncSmall runs the small writer's measured Sync.
	SyncSmall() error

	// HoldNextAppend makes the stream's next interval stop in flight, at its
	// first segment write, before anything of it lands: reached closes once
	// it is stopped, and release lets it go on. Nothing else is held.
	HoldNextAppend() (reached <-chan struct{}, release func())

	// Queue tells the env one more interval is waiting to be written by the
	// stream, before the protocol hands it over: the stream's backlog. A
	// journal sees an interval only when its write reaches it, so the check
	// does nothing here; a fake whose design waits on the backlog counts it.
	Queue()

	// WaitedTotal is the write bytes successful syncs waited for over the
	// segment files: what each found unsynced as it arrived at storage,
	// before anything parked it. The window's measure is its difference.
	WaitedTotal() int64

	// DurableTotal is the write bytes successful syncs have made durable
	// over the segment files: what the floor checks are read from.
	DurableTotal() int64

	// SegmentNames lists the segment file names now, so a round can tell the
	// name set changed while its window was open.
	SegmentNames() []string

	// WindowBound is what one measured window's syncs may wait for: the
	// checked sync's own record, one stream interval — record bytes — and
	// the header of a segment a roll just took. The env supplies its own record
	// sizes, so a fake's window is bounded by the same arithmetic at its own
	// scale.
	WindowBound() int64

	// Written so far, in record bytes, is what every successful SyncStream
	// must have made durable already: the intervals the stream landed and
	// the small records the protocol wrote. A sync that returns success
	// with less has not answered for what it was handed.
	DurableFloor() int64
}

// mixedMatch is the class of storage calls the gate parks: syncs of segment
// files, the fsyncs a journal makes durable through.
func mixedMatch(op string, key []byte) bool {
	return op == kit.OpSync && len(key) >= 4 && string(key[len(key)-4:]) == ".seg"
}

// mixedWait bounds, in real time, what the protocol waits for: the fixture's
// bound, with room for the teardown to drain two waits one after the other.
const mixedWait = 2 * time.Second

// settleParked waits for every parked call of the gate to leave, or fails
// the run: a window that ends with a still-parked call holds a goroutine the
// disarm was meant to free.
func settleParked(t mixedTB, gate *kit.SimGate) {
	t.Helper()
	deadline := time.Now().Add(mixedWait)
	for gate.Parked() != 0 || gate.Dropped() != 0 {
		time.Sleep(time.Millisecond)
		if time.Now().After(deadline) {
			if gate.Parked() != 0 {
				t.Fatalf("the window ended with %d gate calls still parked, freed by nothing", gate.Parked())
			}
			t.Fatalf("the gate dropped %d arrivals the window never drained", gate.Dropped())
		}
	}
}

// runMixedRounds drives the whole mixed-durability run: the stream
// goroutine, every round's plain sync and small write, each measured window
// with its feeds, and the teardown. It is the code both the check and the
// choreography run, so a protocol bug — a channel closed twice, a result
// received twice, a goroutine left parked, a release answered by nobody, a
// skip that fires every round — fails the choreography first, against fakes
// whose designs it can see.
//
// Per round:
//
//   - one opening interval is handed to the stream, and its landing waited
//     for, so the round begins with exactly one stream append in it;
//   - SyncStream runs, so nothing of the stream's stays pending into the
//     window, and the durable total must have reached everything written so
//     far: a design that returns success while holding the caller's writes
//     unsynced fails here;
//   - the small writer's record lands, pending into the window;
//   - one more interval is handed to the stream and held in flight at its
//     first segment write, so the stream is appending when the measured
//     Sync starts;
//   - the gate is armed around the measured Sync; its first arrival is
//     answered by letting the held interval land, every later one by a fed
//     interval, each released once its interval confirmed, and the cap
//     stops the feeds after mixedFeeds intervals; an arrival while an
//     interval is still landing is that write's own roll-seal sync,
//     released unfed;
//   - a sync that neither arrives nor returns while the interval is in
//     flight is handed the stream's backlog instead, each interval parked at
//     its first segment write when handed over and freed by the next arrival
//     or the next bounded wait, up to the same cap, so a design that waits
//     for the whole backlog arrives with it pending while a merely delayed
//     correct arrival measures nothing it never waited for;
//   - the gate is disarmed and everything it still holds is freed, and only
//     then is the measured Sync's result waited on — received once, in the
//     feeds or here, never both;
//   - when the window freed a fed write that had not confirmed yet, the run
//     waits for that confirmation before the next round reads any stream
//     state: an append still in flight would move the env's counters under
//     a round that thinks the stream is quiescent;
//   - the window's measure, the write bytes its syncs waited for as each
//     arrived, is compared against the bound, unless the segment name set
//     changed while the window was open: a fresh segment was taken and the
//     old one sealed, and its seal covered the stream's own records, bytes
//     no small-writer sync chose.
//
// The interval count is the stream's own: each interval is counted the
// moment its confirmation arrives — the opening one when the round opens,
// each fed one inside the window, the straggler freed by the disarm in the
// fed-wait — so the boundary rule reads the stream's real position, and a
// round that fed two intervals consumes two from the count. Rounds whose
// opening or any interval the window could feed ends a stream file never
// measure: the stream's own file-end sync would meet the armed gate and park
// behind a protocol that cannot answer it, and the skip is keyed on the
// stream's intervals, never the round number. The fakes' real-count cases
// hold that honest: a rule keyed anything else arms a round whose interval
// ends a file, and the watcher cases catch every closing sync that ran
// armed.
// runMixedRounds drives the mixed-durability protocol of RFC 1 section 9.7
// over env's stream and returns how many windows each streamed file
// measured, one entry per file in the stream's order; the run-wide teardown
// below fails a run that measured nothing at all, and whether an empty leg
// is a failure is the caller's, since the boundary rule can disarm a whole
// small file — the fakes' five-interval file never arms a window — and a
// check whose every leg is large enough to measure holds each to at least
// one window.
func runMixedRounds(t mixedTB, env mixedEnv) []int {
	t.Helper()

	gate := &kit.SimGate{Match: mixedMatch}
	env.Install(gate)
	t.Cleanup(gate.Disarm)

	// done is closed by the stream goroutine alone, when it has landed every
	// interval and run every file-end sync. quit is closed once — by
	// closeQuit, whoever runs first, the teardown or the cleanup after a
	// failure mid-run — so the same goroutine can stop without done ever
	// being closed twice.
	done := make(chan struct{})
	quit := make(chan struct{})
	closeQuit := sync.OnceFunc(func() { close(quit) })
	t.Cleanup(func() {
		closeQuit()
		select {
		case <-done:
		case <-time.After(mixedWait):
			t.Errorf("the streaming writer did not return within %v after the run was ended: a goroutine is left parked behind a call nobody freed", mixedWait)
		}
	})

	// proceed hands the stream its next interval, appended reports each one
	// landed — buffered to one turn, for the fed write whose confirmation
	// lands after its window closed — and errs carries the stream's first
	// failure out.
	proceed := make(chan struct{})
	appended := make(chan struct{}, 1)
	errs := newMixedErrs()

	// The stream goroutine: it lands an interval per turn it is handed and
	// stops the moment quit closes. It lands into env.Append, whose return
	// also runs a finished file's closing sync and tells whether any interval
	// remains; nothing else on the run's goroutine touches the stream.
	go func() {
		defer close(done)
		for {
			select {
			case <-proceed:
			case <-quit:
				return
			}
			more, err := env.Append()
			if err != nil {
				errs.send(err)
				return
			}
			if !more {
				return
			}
			select {
			case appended <- struct{}{}:
			case <-quit:
				return
			}
		}
	}()

	// measured counts the windows of the whole run for the vacuity guard:
	// a run whose every window was skipped proves nothing about any design.
	// fileMeasured counts them per streamed file, keyed on the interval each
	// file starts at: the interval that follows a file's last one starts
	// the next file, so a leg's count is settled the moment the next file
	// opens. Both files' counts are returned, so a check that runs legs
	// large enough to need it can hold each to at least one window.
	measured := 0
	fileStarts, measuredPerFile := env.FileStarts(), make([]int, len(env.FileStarts()))
	fileIdx, fileMeasured := 0, 0
	for interval := 0; interval < env.Intervals(); {
		// Hand the stream its opening interval and wait for it to land. A
		// stream that returns here has run every interval it was given and
		// every file-end sync: the run's rounds are over, and the interval it
		// just landed — the last of the last file — ends the count. No
		// confirmation may be left over from the last window: the fed-wait
		// below already drained the one a closed window could leave.
		landed := true
		select {
		case proceed <- struct{}{}:
		case <-done:
			landed = false
		case <-time.After(mixedWait):
			t.Fatalf("the stream did not take its opening interval within %v: the round never opened", mixedWait)
		}
		if !landed {
			break
		}
		confirmed := false
		select {
		case <-appended:
			confirmed = true
		case <-done:
			// The opening interval the stream just landed was the last of
			// the last file: Append ran its closing sync and returned no
			// more, without a confirmation to send.
			confirmed = false
		case err := <-errs.value:
			stubCall(t, "WriteAt", err)
			t.Fatalf("the streaming writer's opening write: %v", err)
		case <-time.After(mixedWait):
			t.Fatalf("the stream did not confirm its opening interval within %v: the round never opened", mixedWait)
		}
		interval++
		if !confirmed {
			break
		}
		// The opening interval just landed started a later file: the leg
		// before it is settled — it cannot measure again — and the count
		// moves to that file's own.
		for fileIdx+1 < len(fileStarts) && interval == fileStarts[fileIdx+1] {
			if measuredPerFile[fileIdx] != fileMeasured {
				t.Fatalf("per-file measurement bookkeeping drifted: counted %d windows for file %d, recorded %d", fileMeasured, fileIdx, measuredPerFile[fileIdx])
			}
			fileIdx++
			fileMeasured = 0
		}

		// The window's boundary rule: never measure a round whose opening
		// interval, or any interval the window could feed, ends a stream
		// file — the stream's file-end sync would meet the armed gate and
		// park behind a protocol that cannot answer it.
		armed := true
		for k := interval - 1; k < interval+mixedFeeds; k++ {
			if env.EndsFile(k) {
				armed = false
				break
			}
		}
		if !armed {
			continue
		}

		// The round's plain sync of the streamed file: the rules make it
		// flush everything the stream has written, so the opening interval
		// is durable and nothing of the stream's is pending into the window.
		if err := env.SyncStream(); err != nil {
			stubCall(t, "Sync", err)
			t.Fatalf("the round's plain Sync of the stream's file: %v", err)
		}
		// The sync that just returned success settled everything written so
		// far, or it did not answer for what it was handed.
		if floor := env.DurableFloor(); env.DurableTotal() < floor {
			t.Fatalf("a Sync that returned success left less durable than written: durable %d, want at least %d", env.DurableTotal(), floor)
		}

		// The small writer's own record for the round, written after the
		// plain sync so it stays pending into the measured window.
		if err := env.WriteSmall(); err != nil {
			stubCall(t, "WriteAt", err)
			t.Fatalf("the small write: %v", err)
		}

		// The measured window. The stream is handed one interval whose write
		// the env stops in flight, so an append of the stream is under way
		// when the small writer's Sync starts, and the gate is armed around
		// that Sync, run on its own goroutine, so the run's own waits stay
		// bounded.
		before := env.WaitedTotal()
		namesBefore := slices.Clone(env.SegmentNames())
		reached, releaseHeld := env.HoldNextAppend()
		// A run that fails while the interval is held lets it go before the
		// stream goroutine is waited for: the cleanup that joins it runs
		// after this one.
		t.Cleanup(releaseHeld)
		env.Queue()
		select {
		case proceed <- struct{}{}:
		case <-time.After(mixedWait):
			t.Fatalf("the stream did not take the window's interval within %v", mixedWait)
		}
		select {
		case <-reached:
		case err := <-errs.value:
			stubCall(t, "WriteAt", err)
			t.Fatalf("the streaming writer's in-flight write: %v", err)
		case <-time.After(mixedWait):
			t.Fatalf("the stream's interval did not reach its write within %v", mixedWait)
		}
		held := true
		gate.Arm()
		syncDone := make(chan error, 1)
		go func() { syncDone <- env.SyncSmall() }()

		// The feeds: each arrival the window's syncs announce is answered
		// with one stream interval, whose confirmation releases the call
		// that asked. The first is the interval already in flight: an append
		// landing while the sync waits is the one interval the row allows
		// it. An arrival while a fed write is still landing is that write's
		// own roll-seal sync: it is released unfed, and the write finishes.
		// A design that keeps asking is fed at most mixedFeeds intervals;
		// after that its arrivals are released without one.
		//
		// A sync that neither arrives nor returns while the interval is in
		// flight is waiting on the stream. It is then handed the stream's
		// backlog, mixedFeeds intervals in all with the one in flight. Each
		// backlog interval is handed over parked at its first segment write,
		// so none of its bytes are on the segment — and none in the seam's
		// waited totals — until it is freed: an arrival that answers it
		// measures nothing of it, exactly as the first hold's answer measures
		// nothing of what lands during its park. A design that waits for the
		// whole backlog without asking is made to land one interval per
		// bounded wait and so arrives, whenever it asks, with every interval
		// the waits landed pending against the bound's one.
		sent, backlog := 0, 0
		fedInFlight, answering := false, false
		returned := false
		// dripParked is whether one backlog interval now sits parked at its
		// first segment write, and dripRelease frees that park. While it is
		// parked nothing else is in flight — a park is only installed once
		// the previous fed write confirmed — so an arrival in that state can
		// only be a sync's own fsync, never a landing write's roll-seal.
		dripParked := false
		var dripRelease func()
	feedLoop:
		for {
			select {
			case <-gate.Arrivals:
				if dripParked {
					// An arrival while a backlog interval sits parked at its
					// write: answer it with that interval, the shape of the
					// first held answer. A delayed-but-correct sync is handed
					// bytes it never waited for otherwise — its measure would
					// read the next backlog interval's landing as its own
					// wait.
					dripRelease()
					dripParked = false
					fedInFlight, answering = true, true
					continue
				}
				if fedInFlight {
					gate.Release()
					continue
				}
				if held {
					releaseHeld()
					held = false
					fedInFlight, answering = true, true
					continue
				}
				select {
				case proceed <- struct{}{}:
					fedInFlight, answering = true, true
				case <-done:
					break feedLoop
				case <-time.After(mixedWait):
					t.Fatalf("the stream did not take its fed interval within %v: the asking sync is left stalled", mixedWait)
				}
			case <-appended:
				if !fedInFlight {
					continue
				}
				// A fed interval confirmed is a stream interval landed: the
				// count follows the stream, not the rounds, and a fed write
				// is as much part of the stream as the opening one.
				fedInFlight = false
				if answering {
					answering = false
					gate.Release()
				}
				sent++
				interval++
				if sent >= mixedFeeds {
					// The cap: no further feeds. Free every arrival already
					// announced without one.
					for takeArrival(gate) {
						gate.Release()
					}
					break feedLoop
				}
			case err := <-syncDone:
				if err != nil {
					stubCall(t, "Sync", err)
					t.Fatalf("the small writer's measured Sync: %v", err)
				}
				returned = true
				break feedLoop
			case err := <-errs.value:
				stubCall(t, "WriteAt", err)
				t.Fatalf("the streaming writer's fed write: %v", err)
			case <-time.After(mixedWait):
				// A bounded wait, the one this check makes, and the only
				// thing that tells a sync waiting on the stream apart: a
				// sync that waits on nothing arrives or returns at once, and
				// one that waits for what the stream has under way does
				// neither, which no event announces. The waits hand the
				// backlog over parked — one wait to park the next interval at
				// its write, the following one to land it if no arrival
				// came — so a sync that waits for the interval in flight
				// alone arrives to find the next interval parked, not
				// landed, and its measure counts only what landed before it,
				// while a sync that waits for the whole backlog has it
				// landed around it and arrives with all of it pending. The
				// ceiling: a sync delayed past two waits is counted against
				// the one parked interval the earlier wait released — one
				// interval, never more, since each wait lands one at most.
				switch {
				case held:
					backlog = mixedFeeds - 1
					for range backlog {
						env.Queue()
					}
					releaseHeld()
					held = false
					fedInFlight = true
				case dripParked:
					// No arrival asked for the parked interval within the
					// wait: land it. Its bytes then enter only arrivals that
					// come after this release, and the arrival branch above
					// answers none of them with it.
					dripRelease()
					dripParked = false
				case backlog > 0 && !fedInFlight:
					// Hand the next backlog interval over parked at its
					// first segment write: its bytes reach the segment only
					// once an arrival asks for it or a wait runs out, both
					// below.
					backlog--
					reached, release := env.HoldNextAppend()
					dripRelease = release
					t.Cleanup(release)
					select {
					case proceed <- struct{}{}:
					case <-done:
						break feedLoop
					case <-time.After(mixedWait):
						t.Fatalf("the stream did not take its backlog's next interval within %v", mixedWait)
					}
					select {
					case <-reached:
					case err := <-errs.value:
						stubCall(t, "WriteAt", err)
						t.Fatalf("the streaming writer's backlog write: %v", err)
					case <-time.After(mixedWait):
						t.Fatalf("the backlog's next interval did not reach its write within %v", mixedWait)
					}
					dripParked, fedInFlight = true, true
				default:
					break feedLoop
				}
			}
		}
		if held {
			// The sync returned with the interval still in flight: let it
			// land, and wait for it below as for any fed write.
			releaseHeld()
			fedInFlight = true
		}
		if dripParked {
			// The window closed with a backlog interval still parked at its
			// write: a write nobody will answer would hang the confirmation
			// wait below. Let it land, and that wait drains it as any fed
			// write's.
			dripRelease()
		}

		// Close the window: disarm frees everything still parked — the
		// measured sync included — and every announcement left in the queue
		// is released so no call waits on nobody. Parked must reach zero:
		// a call left parked holds a goroutine nothing will free.
		gate.Disarm()
		for takeArrival(gate) {
			gate.Release()
		}
		settleParked(t, gate)
		if fedInFlight {
			// The window freed a fed write whose confirmation had not
			// arrived: wait for it here, bounded and failing loudly, before
			// the next round reads the stream's state — an append still in
			// flight would move the env's counters under a protocol that
			// believes the stream is quiescent. The disarm freed whatever
			// held it.
			select {
			case <-appended:
				interval++
			case <-done:
				// The fed write closed the stream: it ran the last file's
				// closing sync and returned no more, without a
				// confirmation to send.
			case <-time.After(mixedWait):
				t.Fatalf("the fed write was not confirmed within %v after the window was freed: an append is left in flight", mixedWait)
			}
			fedInFlight = false
		}
		if !returned {
			select {
			case err := <-syncDone:
				if err != nil {
					stubCall(t, "Sync", err)
					t.Fatalf("the small writer's measured Sync: %v", err)
				}
			case <-time.After(mixedWait):
				t.Fatalf("the small writer's measured Sync did not return within %v after the window was freed", mixedWait)
			}
		}

		// The measure: the write bytes the window's syncs waited for, each
		// what was unsynced as it arrived. A window in which the segment
		// name set changed took a fresh segment: the seal covered the
		// stream's own records, bytes no small-writer sync chose.
		waited := env.WaitedTotal() - before
		if slices.Equal(namesBefore, env.SegmentNames()) {
			if waited > env.WindowBound() {
				t.Fatalf("a small writer's Sync waited for %d bytes in one window - its own record, one stream interval and a roll header leave no room for more - at interval %d, bound %d", waited, interval, env.WindowBound())
			}
			measured++
			fileMeasured++
			measuredPerFile[fileIdx]++
		}
	}

	// The stream ran out of intervals. End it, wait for its goroutine, and
	// read its first failure if one came.
	closeQuit()
	select {
	case <-done:
	case <-time.After(mixedWait):
		t.Fatalf("the streaming writer did not return within %v after the last round", mixedWait)
	}
	if err := errs.take(); err != nil {
		stubCall(t, "WriteAt/Sync", err)
		t.Fatalf("the streaming writer's last call: %v", err)
	}

	// The vacuity guard: a run whose every window was skipped — by the
	// name-set rule or by the file-end boundary rule, whichever disarmed
	// every round — proves nothing about any design, and says so loudly
	// rather than passing over an empty set of measures.
	if measured == 0 {
		t.Fatalf("every measured window of the run was skipped, by the segment-name rule or the file-end boundary rule: the check proved nothing")
	}

	// The run's own syncs settled everything its writers wrote. A design
	// that reports success while leaving any of it unsynced ends the run
	// short of it.
	if floor := env.DurableFloor(); env.DurableTotal() < floor {
		t.Fatalf("the run ended with durable %d, want at least the %d its writes took", env.DurableTotal(), floor)
	}

	// The last leg's counts were never settled by a next file opening, so
	// they are folded in here; measuredPerFile has now counted every
	// window of every file the stream landed.
	if measuredPerFile[fileIdx] != fileMeasured {
		t.Fatalf("per-file measurement bookkeeping drifted: counted %d windows for the last file %d, recorded %d", fileMeasured, fileIdx, measuredPerFile[fileIdx])
	}
	total := 0
	for _, n := range measuredPerFile {
		total += n
	}
	if total != measured {
		t.Fatalf("the files measured %d windows between them and the run measured %d, want the same count", total, measured)
	}
	return measuredPerFile
}

// takeArrival reports whether an armed call's arrival is still queued, and
// takes it.
func takeArrival(gate *kit.SimGate) bool {
	select {
	case <-gate.Arrivals:
		return true
	default:
	}
	return false
}
