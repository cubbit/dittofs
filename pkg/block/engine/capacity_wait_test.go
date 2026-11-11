package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// endsAtCtx is a context whose deadline is an hour after the test clock's now,
// so a refused call waits, and that ends with context.DeadlineExceeded when
// the test closes done.
type endsAtCtx struct {
	context.Context
	now  time.Time
	done chan struct{}
}

func (c *endsAtCtx) Deadline() (time.Time, bool) {
	return c.now.Add(time.Hour), true
}

func (c *endsAtCtx) Done() <-chan struct{} {
	return c.done
}

func (c *endsAtCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// atLimit is a started engine whose first journal refuses every version by the
// version limit while its raise is held at the store gate g. The engine draws
// versions until the raise falls due and reaches the gate; then the fake
// journal is put at its limit, so no call of a test depends on an ended
// context or a passed deadline to get there.
func atLimit(t *testing.T, setup ...func(s *starter)) (*starter, *gate) {
	t.Helper()

	s := newRaiser(t, time.Second, withOpenState())
	s.serve(t)

	for _, f := range setup {
		f(s)
	}

	g := newGate(t)
	s.lc.hold.Store(&heldRaise{g: g})

	// The held raise ends before the store closes. Nothing issues a version
	// here but a write waiting for this raise, which a test has ended or
	// whose raise this releases.
	t.Cleanup(func() {
		g.open()
		s.e.JoinRaisesForTest()
	})

	for left(s) >= testBlock/2 {
		if _, err := s.e.Draw(t.Context(), s.one.ID, drawFile); err != nil {
			t.Fatalf("a draw below the trigger: %v", err)
		}
	}

	<-g.entered

	// The journal now refuses every version, while the raise is held.
	next := s.j.Stats().NextVersion
	if err := s.j.SetVersionLimit(next); err != nil {
		t.Fatal(err)
	}

	return s, g
}

// waiter is a write that the version limit refuses, and what it did.
type waiter struct {
	parked chan struct{}
	done   chan error
	served atomic.Int64
	file   entity.FileID
}

// startWrite starts a write of a new file of s's first share under ctx, with
// the hook that tells when it is waiting.
func startWrite(t *testing.T, s *starter, ctx context.Context, file ...entity.FileID) *waiter {
	t.Helper()

	w := armWaiter(s)

	return w.write(s, ctx, file...)
}

// armWaiter is a waiter whose hook is set on s's engine: parked closes when a
// call is about to block on the raise.
func armWaiter(s *starter) *waiter {
	w := &waiter{
		parked: make(chan struct{}),
		done:   make(chan error, 1),
	}

	var once sync.Once

	s.e.OnRefused(func() {
		once.Do(func() {
			close(w.parked)
		})
	})

	return w
}

// write starts a write of a file of s's first share under ctx.
func (w *waiter) write(s *starter, ctx context.Context, file ...entity.FileID) *waiter {
	w.file = entity.NewFileID()

	if len(file) > 0 {
		w.file = file[0]
	}

	go func() {
		_, err := s.e.Write(ctx, s.one.ID, w.file, 0, []byte("x"), engine.WriteOpts{
			Done: func() {
				w.served.Add(1)
			},
		})
		w.done <- err
	}()

	return w
}

// await receives from ch. There is no timer: a wait that correct code never
// reaches ends at go test's timeout.
func await[T any](ch <-chan T) T {
	return <-ch
}

// mustPark returns once the call is about to block on the raise, and fails t
// when the call answers first.
func (w *waiter) mustPark(t *testing.T) {
	t.Helper()

	select {
	case <-w.parked:
	case err := <-w.done:
		t.Fatalf("the call answered %v instead of waiting for the raise", err)
	}
}

// notDone fails t when the waiter has already answered.
func (w *waiter) notDone(t *testing.T) {
	t.Helper()

	select {
	case err := <-w.done:
		t.Fatalf("the write answered %v while the raise was held", err)
	default:
	}
}

// commitRaise lets the held raise commit, waits for it to end, and checks that
// the channel the waiting calls hold, taken before, is closed: if it is still
// open the committed raise did not wake them, which fails t at once and ends
// their wait with cancel. Only then does it serve them, lifting the refusals
// the test armed for calls first.
//
// The journal's handles hold every WriteAt, Truncate and Reserve from the
// start, so a call the commit woke stays in front of the journal until the
// check is done: it does not look at the raise, which has ended, before the
// check. Nothing issues a version while the join runs, so no raise starts
// and Adds to the group while the join Waits.
func commitRaise(
	t *testing.T,
	s *starter,
	g *gate,
	cancel context.CancelFunc,
	calls ...enginetest.Call,
) {
	t.Helper()

	release := s.j.hold(t)
	waiting := s.e.VersionLimitWatch(s.one.ID)

	g.open()
	s.e.JoinRaisesForTest()

	select {
	case <-waiting:
	default:
		cancel()
		t.Fatal("the committed raise did not wake the waiter")
	}

	for _, c := range calls {
		if err := s.j.Refuse(c, nil); err != nil {
			t.Fatal(err)
		}
	}

	release()
}

// A write the version limit refuses waits for the raise: it is still waiting
// while the raise is held, and the caller's context ending answers ErrDelay
// with DelayCommit, never no space and never the context's error.
func TestVersionRefusalHeldUntilTheContextEnds(t *testing.T) {
	s, _ := atLimit(t)
	ctx, cancel := context.WithCancel(t.Context())
	w := startWrite(t, s, ctx)

	w.mustPark(t)
	w.notDone(t)
	cancel()

	err := await(w.done)
	if !isDelay(err, vfserr.DelayCommit) {
		t.Errorf("a write held at the version limit, canceled: %v, want ErrDelay(DelayCommit)", err)
	}

	if errors.Is(err, vfserr.ErrNoSpace) || errors.Is(err, context.Canceled) {
		t.Errorf("the answer %v is no space or the context's error", err)
	}
}

// A deadline, not a cancel, that ends the wait gives the same answer.
func TestVersionRefusalHeldUntilTheDeadlineEnds(t *testing.T) {
	s, _ := atLimit(t)
	ctx := &endsAtCtx{Context: t.Context(), now: s.clock.Now(), done: make(chan struct{})}
	w := startWrite(t, s, ctx)

	w.mustPark(t)
	w.notDone(t)
	close(ctx.done)

	err := await(w.done)
	if !isDelay(err, vfserr.DelayCommit) {
		t.Errorf("a write held at the version limit, deadline: %v, want ErrDelay(DelayCommit)", err)
	}

	if errors.Is(err, vfserr.ErrNoSpace) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the answer %v is no space or the context's error", err)
	}
}

// A write waiting for the raise is served once the raise commits: the same
// call returns a version and calls Done once. Also when the raise commits
// between the refusal and the wait, which a signal taken after the call loses.
func TestVersionRefusalServedOnceTheRaiseCommits(t *testing.T) {
	t.Run("raise commits while it waits", func(t *testing.T) {
		s, g := atLimit(t)
		ctx, cancel := context.WithCancel(t.Context())

		defer cancel()

		w := startWrite(t, s, ctx)

		w.mustPark(t)
		commitRaise(t, s, g, cancel)

		if err := await(w.done); err != nil {
			t.Errorf("the write after the raise: %v, want served", err)
		}

		if n := len(s.e.Pending(w.file)); n != 1 {
			t.Errorf("%d pending operations after the served write, want 1", n)
		}
	})

	t.Run("raise commits inside the refused call", func(t *testing.T) {
		s, g := atLimit(t)
		ctx, cancel := context.WithCancel(t.Context())

		defer cancel()

		// The journal lets the raise commit before it answers the refusal, so
		// the limit has changed by the time the engine looks at it.
		s.j.AfterRefusal(func() {
			// Only this blocked write could issue a version, so no raise starts
			// while the join runs.
			g.open()
			s.e.JoinRaisesForTest()
		})

		w := armWaiter(s)
		s.e.OnRefused(func() {
			t.Error("the write waits for a raise that had already committed")
			cancel()
		})

		w.write(s, ctx)

		if err := await(w.done); err != nil {
			t.Errorf("the write after a raise that committed first: %v, want served", err)
		}

		if n := len(s.e.Pending(w.file)); n != 1 {
			t.Errorf("%d pending operations after the served write, want 1", n)
		}
	})
}

// A refusal whose deadline has already passed answers at once: it does not
// wait for the raise, which is held.
func TestVersionRefusalPastTheDeadlineDoesNotWait(t *testing.T) {
	s, _ := atLimit(t)
	ctx := clockCtx{Context: t.Context(), now: s.clock.Now()}
	w := startWrite(t, s, ctx)

	select {
	case <-w.parked:
		t.Fatal("the write waited for the raise past its deadline")
	case err := <-w.done:
		if !isDelay(err, vfserr.DelayCommit) {
			t.Errorf("a write past its deadline at the limit: %v, want ErrDelay(DelayCommit)", err)
		}
	}
}

// A version refusal with no raise in flight answers retry-later at once: no
// raise is coming to wake a wait.
func TestVersionRefusalWithNoRaiseInFlightDoesNotWait(t *testing.T) {
	s, g := atLimit(t)

	g.open()
	s.e.JoinRaisesForTest()

	if err := s.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitVersion)); err != nil {
		t.Fatal(err)
	}

	w := startWrite(t, s, t.Context())

	select {
	case <-w.parked:
		t.Fatal("the write waited for a raise that is not in flight")
	case err := <-w.done:
		if !isDelay(err, vfserr.DelayCommit) {
			t.Errorf("no raise in flight: %v, want ErrDelay(DelayCommit)", err)
		}
	}
}

// A raise in flight that ends because the trigger no longer holds wakes a
// write waiting for it, which then finds no raise in flight and answers
// retry-later: it does not stay waiting.
func TestVersionRefusalWokenByARaiseFoundNotDue(t *testing.T) {
	s := newRaiser(t, time.Second)

	if err := s.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitVersion)); err != nil {
		t.Fatal(err)
	}

	free, err := s.e.HoldRaiseLockForTest(t.Context(), s.one.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The raise is in flight, waiting for the lock the test holds.
	s.e.WakeRaiseForTest(s.one.ID)

	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	w := startWrite(t, s, ctx)

	w.mustPark(t)

	// The woken write finds no raise in flight, so it must not wait again.
	failIfWaits(t, s.e, cancel)

	waiting := s.e.VersionLimitWatch(s.one.ID)

	free()
	s.e.JoinRaisesForTest()

	select {
	case <-waiting:
	default:
		cancel()
		t.Fatal("the raise found not due did not wake the waiter")
	}

	if err := await(w.done); !isDelay(err, vfserr.DelayCommit) {
		t.Errorf("a write woken by a raise found not due: %v, want ErrDelay(DelayCommit)", err)
	}
}

// A journal that refused its limit answers retry-later at once, with no wait:
// no raise will come.
func TestVersionRefusalOfARefusedLimitDoesNotWait(t *testing.T) {
	s, g := atLimit(t)
	s.j.mu.Lock()
	s.j.fail = map[string]error{"limit": journal.ErrUnknownFormat}
	s.j.mu.Unlock()
	g.open()
	s.e.JoinRaisesForTest()

	w := startWrite(t, s, t.Context())

	select {
	case <-w.parked:
		t.Fatal("the write waited for a raise that will not come")
	case err := <-w.done:
		if !isDelay(err, vfserr.DelayCommit) {
			t.Errorf("a write at a refused limit: %v, want ErrDelay(DelayCommit)", err)
		}
	}
}

// The journal refusing its limit while a write waits wakes it: the channel it
// waits on is closed once the refused raise has ended, and the write answers
// retry-later.
func TestVersionRefusalWaitingWhenTheLimitIsRefused(t *testing.T) {
	s, g := atLimit(t)
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	w := startWrite(t, s, ctx)

	w.mustPark(t)

	// No version is issued meanwhile, so the channel is the one the write waits
	// on and the join below races no start of a raise.
	waiting := s.e.VersionLimitWatch(s.one.ID)

	// The write waited once; a second wait, after the refusal woke it, means
	// it did not see that the journal refused its limit. The hook is set before
	// the wake so it cannot miss that wait.
	failIfWaits(t, s.e, cancel)

	s.j.mu.Lock()
	s.j.fail = map[string]error{"limit": journal.ErrUnknownFormat}
	s.j.mu.Unlock()
	g.open()
	s.e.JoinRaisesForTest()

	select {
	case <-waiting:
	default:
		cancel()
		t.Fatal("the refused raise did not wake the waiter")
	}

	err := await(w.done)
	if !isDelay(err, vfserr.DelayCommit) {
		t.Errorf("a write waiting when the limit was refused: %v, want ErrDelay(DelayCommit)", err)
	}
}

// A journal closed under the refusal answers unavailable on the retry, not a
// capacity cause.
func TestClosedJournalUnderARefusalIsUnavailable(t *testing.T) {
	s, g := atLimit(t)
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	w := startWrite(t, s, ctx)

	w.mustPark(t)

	if err := s.j.Close(); err != nil {
		t.Fatal(err)
	}

	commitRaise(t, s, g, cancel)

	err := await(w.done)
	if !errors.Is(err, vfserr.ErrUnavailable) || isDelay(err, vfserr.DelayCommit) {
		t.Errorf("a write over a journal closed under its refusal: %v, want ErrUnavailable", err)
	}
}

// A write waiting for the raise holds no registration: a truncate of the same
// file reaches the journal, and waits for the raise as well, instead of waiting
// for the write to leave. Both are served once the raise commits.
func TestAWaitingWriteBlocksNoRemovalOfItsFile(t *testing.T) {
	var f entity.FileID

	s, g := atLimit(t, func(s *starter) {
		f = createFile(t, s, "a")
	})
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	w := startWrite(t, s, ctx, f)

	w.mustPark(t)

	if n := s.e.Writing(f); n != 0 {
		t.Fatalf("%d writes counted admitted while one waits for the raise, want 0", n)
	}

	truncate := startCall(t, s, func() error {
		return s.e.Truncate(ctx, s.one.ID, f, 5)
	})

	truncate.mustPark(t)
	commitRaise(t, s, g, cancel)

	if err := await(truncate.done); err != nil {
		t.Errorf("the truncate after the raise: %v, want served", err)
	}

	if err := await(w.done); err != nil {
		t.Errorf("the write after the raise: %v, want served", err)
	}
}

// createFile creates an empty file of s's first share.
func createFile(t *testing.T, s *starter, name string) entity.FileID {
	t.Helper()

	_, made := s.fs.Create(t, s.fs.Admin, s.fs.Root, name, kvstore.Regular(0o644))

	return made.ID
}

// A wait that holds the removal guard, a clone's registrations, or stands for
// the filesystem's accept lock, is woken by the raise before any deadline:
// the raise takes none of them. The engine holds the first two in a unit test;
// the accept lock is the filesystem's, so a mutex the test holds stands for it.
func TestWaitHoldingTheLocksIsWokenByTheRaise(t *testing.T) {
	t.Run("a truncate holds the removal guard", func(t *testing.T) {
		var f entity.FileID

		s, g := atLimit(t, func(s *starter) {
			f = createFile(t, s, "a")
		})
		ctx, cancel := context.WithCancel(t.Context())

		defer cancel()

		w := startCall(t, s, func() error {
			return s.e.Truncate(ctx, s.one.ID, f, 5)
		})

		w.mustPark(t)

		if !s.e.GuardHeld(f) {
			t.Error("the waiting truncate does not hold the file's removal guard")
		}

		read := make(chan struct{})

		go func() {
			defer close(read)

			_, _, _ = s.e.Overlay(t.Context(), s.one.ID, f)
		}()

		await(read)

		commitRaise(t, s, g, cancel)

		if err := await(w.done); err != nil {
			t.Errorf("the truncate after the raise: %v, want served", err)
		}
	})

	t.Run("a clone holds its registrations", func(t *testing.T) {
		var src, dst entity.FileID

		s, g := atLimit(t, func(s *starter) {
			s.e.FakeCloneDone()

			src = createFile(t, s, "src")
			dst = createFile(t, s, "dst")

			_, err := s.e.Write(t.Context(), s.one.ID, src, 0, []byte("abcd"),
				engine.WriteOpts{Done: func() {}})
			if err != nil {
				t.Fatal(err)
			}
		})

		// The fake refuses a reservation by the version limit only when told to.
		if err := s.j.Refuse(enginetest.CallReserve, refusalErr(journal.LimitVersion)); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(t.Context())

		defer cancel()

		w := startCall(t, s, func() error {
			return s.e.Clone(ctx, s.one.ID, src, s.one.ID, dst, vfserr.Extent{Len: 4})
		})

		w.mustPark(t)

		if a, b := s.e.Registrations(src); a+b == 0 {
			t.Error("the waiting clone holds no registration")
		}

		commitRaise(t, s, g, cancel, enginetest.CallReserve)

		if err := await(w.done); err != nil {
			t.Errorf("the clone after the raise: %v, want done", err)
		}
	})

	t.Run("a write under the accept lock", func(t *testing.T) {
		s, g := atLimit(t)

		var accept sync.Mutex

		accept.Lock()

		ctx, cancel := context.WithCancel(t.Context())

		defer cancel()

		w := startWrite(t, s, ctx)

		w.mustPark(t)
		commitRaise(t, s, g, cancel)
		accept.Unlock()

		if err := await(w.done); err != nil {
			t.Errorf("the write after the raise: %v, want served", err)
		}
	})
}

// startCall runs call on a goroutine with the hook that tells when it waits.
func startCall(t *testing.T, s *starter, call func() error) *waiter {
	t.Helper()

	w := &waiter{
		parked: make(chan struct{}),
		done:   make(chan error, 1),
	}

	var once sync.Once

	s.e.OnRefused(func() {
		once.Do(func() {
			close(w.parked)
		})
	})

	go func() {
		w.done <- call()
	}()

	return w
}

// A raise the engine stops wakes the write waiting for it: the write answers
// retry-later and does not outlive the stop. The raise fails, then the stop
// ends it in its retry wait, which leaves with no commit and no refusal.
func TestVersionRefusalWokenByAStoppedRaise(t *testing.T) {
	s, g := atLimit(t, func(s *starter) {
		s.lc.failRaises(errors.New("store down"))
	})

	w := startWrite(t, s, t.Context())

	w.mustPark(t)
	w.notDone(t)

	s.e.StopRaisesForTest()
	g.open()
	s.e.JoinRaisesForTest()

	if err := await(w.done); !isDelay(err, vfserr.DelayCommit) {
		t.Errorf("a write waiting for a stopped raise: %v, want ErrDelay(DelayCommit)", err)
	}
}

// A raise that runs again, because a wake came while it ran, serves the write
// waiting for it when its first limit commits, not when the raise ends. The
// second pass is held at the raise lock the test queued for, so only the
// commit can have woken the write.
func TestVersionRefusalServedByTheFirstCommitOfARaise(t *testing.T) {
	s, g := atLimit(t)
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	w := startWrite(t, s, ctx)

	w.mustPark(t)

	// The raise is in the gate: this wake makes it run once more.
	s.e.WakeRaiseForTest(s.one.ID)

	held := make(chan func(), 1)

	go func() {
		free, err := s.e.HoldRaiseLockForTest(ctx, s.one.ID)
		if err != nil {
			t.Error(err)
			free = func() {}
		}

		held <- free
	}()

	waiting := s.e.VersionLimitWatch(s.one.ID)

	g.open()

	free := <-held

	defer free()

	// The lock is the test's, so the second pass has not run: the commit
	// closed this channel, and the write is served with the raise unfinished.
	<-waiting

	if err := await(w.done); err != nil {
		t.Errorf("the write after the first commit: %v, want served", err)
	}
}
