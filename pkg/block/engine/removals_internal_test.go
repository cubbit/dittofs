package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// syncBuffer is a log sink safe for the loop's goroutine and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// scripted runs a loop over a test clock with one item answering answers in
// turn, and returns a step that advances the clock by one backoff and waits
// until the item has run once more and is back on the loop, or finished.
func scripted(t *testing.T, answers ...answer) (l *removalLoop, f entity.FileID, step func(), log *syncBuffer) {
	t.Helper()
	clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	log = &syncBuffer{}
	e := &Engine{d: Deps{Clock: clock, Logger: slog.New(slog.NewTextHandler(log, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		}}}
	l = newRemovalLoop(e)
	go l.run()

	t.Cleanup(func() {
		l.halt(nil)
	})
	f = entity.NewFileID()
	var runs atomic.Int32
	l.enqueue(&loopItem{b: &binding{}, f: f, kind: entity.RemoveTruncate, run: func(context.Context) answer {
		n := runs.Add(1)
		return answers[n-1]
	}})
	steps := int32(0)
	settled := func(n int32) bool { // run n has finished and the item is back, or gone; an advance runs again at once
		k, _ := l.pending()
		r := runs.Load()
		return r > n || r == n && (k == 1 || answers[n-1] == itemDone)
	}
	wait := func(n int32) {
		deadline := time.Now().Add(5 * time.Second)
		for !settled(n) {
			if time.Now().After(deadline) {
				t.Fatalf("the item never finished run %d", n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	step = func() {
		t.Helper()
		steps++
		if steps > 1 {
			wait(steps - 1)
			// One backoff at a time, until the run starts: the loop may arm
			// its timer after an Advance, which that Advance then missed.
			deadline := time.Now().Add(5 * time.Second)
			for runs.Load() < steps {
				if time.Now().After(deadline) {
					t.Fatalf("the item never started run %d", steps)
				}
				clock.Advance(loopBackoff)
				time.Sleep(time.Millisecond)
			}
		}
		wait(steps)
	}
	return l, f, step, log
}

// A failed item runs again once exactly its backoff has passed on the
// loop's clock: not a nanosecond before, and with no further advance after.
func TestAFailedItemRunsAgainAtItsBackoff(t *testing.T) {
	clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	e := &Engine{d: Deps{Clock: clock, Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		}}}
	l := newRemovalLoop(e)
	var runs atomic.Int32
	answers := []answer{itemRetry, itemDone}
	l.enqueue(&loopItem{b: &binding{}, f: entity.NewFileID(), kind: entity.RemoveTruncate, run: func(context.Context) answer {
		return answers[runs.Add(1)-1]
	}})
	// The loop finds the item on its first look, so the enqueue's wake would
	// only arm a second timer: taken here, the loop arms exactly one.
	<-l.wake
	go l.run()
	t.Cleanup(func() { l.halt(nil) })
	until := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("never: %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}
	until("the failed run's item waits on the clock", func() bool {
		n, _ := l.pending()
		return runs.Load() == 1 && n == 1 && clock.Pending() == 1
	})
	clock.Advance(loopBackoff - time.Nanosecond)
	// A timer that fired leaves the clock at once, and the run that finishes
	// the item arms no other.
	if n := clock.Pending(); n != 1 {
		t.Fatalf("a nanosecond before the backoff the loop's timer fired: %d waits left, want 1", n)
	}
	clock.Advance(time.Nanosecond)
	until("the item runs again at its backoff", func() bool { return runs.Load() == 2 })
}

// An item is removal_stalled after its fifth failure in a row, not its
// fourth, and the run that finishes it clears the condition.
func TestAnItemStallsAfterFiveFailures(t *testing.T) {
	l, f, step, log := scripted(t, itemRetry, itemRetry, itemRetry, itemRetry, itemRetry, itemDone)
	for range 4 {
		step()
	}
	if l.stalledOn(f) {
		t.Fatal("stalled after four failures")
	}
	step()
	if !l.stalledOn(f) {
		t.Fatal("not stalled after five failures")
	}
	step()
	// The step returns once the finishing run has answered, and the loop logs
	// the clearing after that, on its own goroutine: wait for the log line.
	const cleared = "removal_stalled cleared"
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(log.String(), cleared); {
		if time.Now().After(deadline) {
			t.Fatalf("the run that finished it logged no clearing: %s", log.String())
		}

		time.Sleep(time.Millisecond)
	}
}

// A run that advanced resets the count: four failures, an advance and four
// more raise nothing, and a fifth failure in a row then stalls the item.
func TestAnAdvanceResetsTheCount(t *testing.T) {
	l, f, step, _ := scripted(t, itemRetry, itemRetry, itemRetry, itemRetry, itemAdvanced,
		itemRetry, itemRetry, itemRetry, itemRetry, itemRetry, itemDone)
	for range 9 {
		step()
	}
	if l.stalledOn(f) {
		t.Fatal("stalled after four failures, an advance and four more")
	}
	step()
	if !l.stalledOn(f) {
		t.Fatal("not stalled after five failures in a row")
	}
}

// A run held by something outside the item counts toward no stall.
func TestAnUncountedRetryLeavesTheCount(t *testing.T) {
	answers := []answer{itemRetry, itemRetry, itemRetry, itemRetry}
	for range 6 {
		answers = append(answers, itemRetryUncounted)
	}
	l, f, step, _ := scripted(t, append(answers, itemDone)...)
	for range 10 {
		step()
	}
	if l.stalledOn(f) {
		t.Error("stalled after four counted failures and six uncounted")
	}
}

// A share dropped while one of its items runs ends that item when its run
// returns, rather than putting it back, and no item of the share is taken
// after.
func TestADroppedSharesRunningItemIsEnded(t *testing.T) {
	clock := sim.NewTestClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	e := &Engine{d: Deps{Clock: clock, Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		Tracer: func(ctx context.Context, _ string, _ ...slog.Attr) (context.Context, func(error)) {
			return ctx, func(error) {}
		}}}
	l := newRemovalLoop(e)
	go l.run()

	t.Cleanup(func() {
		l.halt(nil)
	})
	s := entity.NewShareID()
	b := &binding{share: Share{ID: s}}
	running, finish := make(chan struct{}), make(chan struct{})
	var ended atomic.Int32
	l.enqueue(&loopItem{b: b, f: entity.NewFileID(), kind: entity.RemoveTruncate,
		run: func(context.Context) answer { close(running); <-finish; return itemRetry },
		end: func() { ended.Add(1) }})
	<-running
	l.dropShare(s)
	close(finish)
	deadline := time.Now().Add(5 * time.Second)
	for ended.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the running item of the dropped share was never ended")
		}
		time.Sleep(time.Millisecond)
	}
	if n, _ := l.pending(); n != 0 {
		t.Errorf("%d items left on the loop", n)
	}
	l.enqueue(&loopItem{b: b, f: entity.NewFileID(), kind: entity.RemoveTruncate,
		run: func(context.Context) answer { return itemDone }, end: func() { ended.Add(1) }})
	if n := ended.Load(); n != 2 {
		t.Errorf("an item enqueued for the dropped share: ended %d times in all, want it ended at once", n)
	}
}

// Each phase-2 run is labelled in engine_removal_runs_total by its result:
// done once Resume and the prune both succeed, advanced when Resume moved the
// cursor whatever its error, conflict for a store conflict from either, and
// error for anything else; only advanced runs again at once.
func TestPhase2RunsAreLabelledByResult(t *testing.T) {
	failed := errors.New("a store failure")
	for _, c := range []struct {
		advanced bool
		err      error
		res      runResult
		a        answer
	}{
		{false, nil, runDone, itemDone},
		{true, failed, runAdvanced, itemAdvanced},
		{true, kvstore.ErrConflict, runAdvanced, itemAdvanced},
		{false, kvstore.ErrConflict, runConflict, itemRetry},
		{false, fmt.Errorf("wrapped: %w", kvstore.ErrConflict), runConflict, itemRetry},
		{false, failed, runError, itemRetry},
	} {
		res, a := phase2Result(c.advanced, c.err)
		if res != c.res || a != c.a {
			t.Errorf("a run advanced %v answering %v: %s, answer %d; want %s, answer %d",
				c.advanced, c.err, runResultNames[res], a, runResultNames[c.res], c.a)
		}
	}
}
