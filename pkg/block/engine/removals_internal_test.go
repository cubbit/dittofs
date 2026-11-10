package engine

import (
	"bytes"
	"context"
	"errors"
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
	t.Cleanup(l.close)
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
	if !strings.Contains(log.String(), "removal_stalled cleared") {
		t.Errorf("the run that finished it logged no clearing: %s", log.String())
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
	t.Cleanup(l.close)
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

// resumeAnswer is one scripted phase-2 run: what Resume answers, and the
// prune's error after a Resume that finished.
type resumeAnswer struct {
	advanced      bool
	err, pruneErr error
}

// scriptedContent answers Resume and PruneRemovals from its script in turn.
type scriptedContent struct {
	Content
	script []resumeAnswer
	n      int
}

func (c *scriptedContent) Resume(context.Context, entity.ShareID, entity.FileID) (bool, error) {
	c.n++
	return c.script[c.n-1].advanced, c.script[c.n-1].err
}

func (c *scriptedContent) PruneRemovals(context.Context, entity.ShareID, entity.FileID, entity.JournalVersion) error {
	return c.script[c.n-1].pruneErr
}

// Each phase-2 run is counted once in engine_removal_runs_total, by its
// result: done once Resume and the prune both succeed, advanced when Resume
// moved the cursor whatever its error, conflict for a store conflict from
// either, and error for anything else.
func TestPhase2RunsAreCountedByResult(t *testing.T) {
	failed := errors.New("a store failure")
	c := &scriptedContent{script: []resumeAnswer{
		{},                            // done
		{advanced: true, err: failed}, // advanced
		{advanced: true, err: kvstore.ErrConflict}, // advanced
		{err: kvstore.ErrConflict},                 // conflict
		{pruneErr: kvstore.ErrConflict},            // conflict
		{err: failed},                              // error
		{pruneErr: failed},                         // error
	}}
	e := &Engine{d: Deps{Content: c}}
	e.conflictBound.Store(int64(time.Second))
	l := newRemovalLoop(e)
	it := e.phase2(&removal{b: &binding{gc: &groupCommit{removals: l}}, f: entity.NewFileID()})
	for range c.script {
		it.run(t.Context())
	}
	want := map[string]int64{"done": 1, "advanced": 2, "conflict": 2, "error": 2}
	for res, name := range runResultNames {
		if got := l.runs[res].Load(); got != want[name] {
			t.Errorf("engine_removal_runs_total{result=%q} = %d, want %d", name, got, want[name])
		}
	}
}
