package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// logTo makes the engine log to buf.
func logTo(buf *lockedBuffer) func(*engine.Deps) {
	return func(d *engine.Deps) {
		d.Logger = slog.New(slog.NewTextHandler(buf, nil))
	}
}

// forgetFault is a journal whose Forget fails with err.
type forgetFault struct {
	engine.Journal
	err error
}

func (f forgetFault) Forget(context.Context, journal.ShareTag) error {
	return f.err
}

// A Drop whose Forget the journal refuses for its headroom is retry-later, as
// the table answers it, and the refusal is no failed journal call.
func TestADropRefusedForTheHeadroomIsRetryLater(t *testing.T) {
	var logged lockedBuffer

	u := newUnit(t, logTo(&logged), func(d *engine.Deps) {
		open := d.Open

		d.Open = func(
			ctx context.Context,
			dir string,
			floor journal.Version,
			id journal.JournalID,
		) (engine.Journal, error) {
			j, err := open(ctx, dir, floor, id)

			return forgetFault{Journal: j, err: refusalErr(journal.LimitHeadroom)}, err
		}
	})

	u.unbind(t)

	err := u.e.Drop(t.Context(), u.share.ID)
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a Drop whose Forget the headroom refused: %v, want ErrDelay(DelayCapacity)", err)
	}

	if strings.Contains(logged.String(), "journal call failed") {
		t.Errorf("a headroom refusal was logged as a failed journal call: %q", logged.String())
	}
}

// The removal loop's clone step answers a capacity refusal of CloneTarget at
// once: no repack, no refusal counted, no warning and no journal_full.
func TestTheLoopsCloneTargetRefusalIsAnsweredAtOnce(t *testing.T) {
	var logged lockedBuffer

	var rf *repackFault

	u, _ := clonedHookedUnit(t, logTo(&logged), trackedRepack(nil, false, &rf))
	src := u.committedFile(t, "src", fill(mib, 's'))
	dst := u.create(t, "dst")

	if err := u.j.Refuse(enginetest.CallCloneTarget, refusalErr(journal.LimitHeadroom)); err != nil {
		t.Fatal(err)
	}

	err := u.answered(t, "the refused clone", u.startClone(t.Context(), src, dst, vfserr.Extent{Len: mib}))
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a clone whose CloneTarget the headroom refused: %v, want ErrDelay(DelayCapacity)", err)
	}

	if n := rf.calls.Load(); n != 0 {
		t.Errorf("%d repacks for the loop's refusal, want 0", n)
	}

	if n := u.e.Stats().WriteRefusals; n != 0 {
		t.Errorf("%d refusals counted, want 0: the loop's refusal answers no client", n)
	}

	if strings.Contains(logged.String(), "write refused") {
		t.Errorf("the loop's refusal was warned: %q", logged.String())
	}

	if c := fullConds(u.e); len(c) != 0 {
		t.Errorf("the loop's refusal raised %d journal_full conditions, want none", len(c))
	}
}

// An Unbind does not wait behind a write waiting for the raise: the shut of
// its gate wakes the write, which answers retry-later, and the Unbind ends
// while the raise is still held. The waits are bounded in real time, as no
// clock is injected into a gate.
func TestUnbindWakesAWriteWaitingForTheRaise(t *testing.T) {
	s, _ := atLimit(t)
	w := startWrite(t, s, t.Context())

	w.mustPark(t)

	unbound := make(chan error, 1)

	go func() {
		unbound <- s.e.Unbind(t.Context(), s.one.ID, false)
	}()

	if err := within(t, "the write's answer once the gate was shut", w.done); !isDelay(err, vfserr.DelayCommit) {
		t.Errorf("a write woken by an Unbind: %v, want ErrDelay(DelayCommit)", err)
	}

	if err := within(t, "the Unbind", unbound); err != nil {
		t.Errorf("Unbind behind a waiting write: %v, want nil", err)
	}
}

// A share being dropped, whose handle is closed before its binding is marked
// removed, does not fail another share's write: the scope sync skips it.
func TestAScopeSyncSkipsAShareBeingDropped(t *testing.T) {
	s := newSegUnit(t, 1<<20, 0)
	fa, fb := entity.NewFileID(), entity.NewFileID()
	s.stream(fb, 1)
	s.j.HoldSyncTimer(true)
	s.overwritten(t, s.share, fb)
	s.overwritten(t, s.a, fa)
	s.refuse(t, journal.LimitHeadroom)

	if err := s.h.Close(); err != nil {
		t.Fatal(err)
	}

	err := s.put(t.Context(), s.a, fa, 100, 1, 'c')
	if !errors.Is(err, vfserr.ErrNoSpace) {
		t.Errorf("A's refusal while B's handle is closed: %v, want ErrNoSpace, not B's closed handle", err)
	}
}

// A repack that finds the journal closed answers a Delete unavailable once:
// the answer is not mapped again, which would log a failed journal call.
func TestAClosedJournalAtTheRepackIsLoggedOnce(t *testing.T) {
	var logged lockedBuffer

	s := newSegUnit(t, 1<<20, 0, faultyRepack(journal.ErrClosed, false), logTo(&logged))
	f := s.create(t, "f")

	s.must(t, s.share, f, 0, 10, 'a')

	if err := s.j.Refuse(enginetest.CallDelete, refusalErr(journal.LimitHeadroom)); err != nil {
		t.Fatal(err)
	}

	_, err := s.e.Delete(t.Context(), s.share.ID, f)
	if !errors.Is(err, vfserr.ErrUnavailable) {
		t.Fatalf("a Delete over a repack that found the journal closed: %v, want ErrUnavailable", err)
	}

	if strings.Contains(logged.String(), "journal call failed") {
		t.Errorf("the closed journal was mapped twice: %q", logged.String())
	}
}

// endedCtxHandle is a handle whose WriteAt answers a context that has ended, as the
// journal does.
type endedCtxHandle struct{ engine.Handle }

func (h endedCtxHandle) WriteAt(
	ctx context.Context,
	id journal.FileID,
	off int64,
	p []byte,
	mtime int64,
) (journal.Version, error) {
	if err := ctx.Err(); err != nil {
		return journal.Version{}, err
	}

	return h.Handle.WriteAt(ctx, id, off, p, mtime)
}

// cancelRepack is a journal whose Repack repacks and then ends the caller's
// context.
type cancelRepack struct {
	engine.Journal
	cancel *context.CancelFunc
}

func (c cancelRepack) Repack(
	ctx context.Context,
	budget int64,
	scope journal.RepackScope,
) (int64, error) {
	freed, err := c.Journal.Repack(ctx, budget, scope)

	(*c.cancel)()

	return freed, err
}

// A caller's context that ends while its refusal's repack runs answers
// retry-later with the refusal's cause, not the context's error, which the
// retry would meet at the journal.
func TestAContextEndedByTheRepackAnswersRetryLater(t *testing.T) {
	var cancel context.CancelFunc

	u := newUnitWith(t, unitConfig{
		wrap: func(h engine.Handle) engine.Handle { return endedCtxHandle{h} },
		deps: []func(*engine.Deps){func(d *engine.Deps) {
			open := d.Open

			d.Open = func(
				ctx context.Context,
				dir string,
				floor journal.Version,
				id journal.JournalID,
			) (engine.Journal, error) {
				j, err := open(ctx, dir, floor, id)

				return cancelRepack{Journal: j, cancel: &cancel}, err
			}
		}},
	})

	u.j.SegmentCap(segBytes, 0)

	var ctx context.Context

	ctx, cancel = context.WithCancel(t.Context())

	defer cancel()

	if err := u.j.Refuse(enginetest.CallWriteAt, refusalErr(journal.LimitHeadroom)); err != nil {
		t.Fatal(err)
	}

	_, err := u.e.Write(ctx, u.share.ID, entity.NewFileID(), 0, []byte("x"), engine.WriteOpts{Done: func() {}})
	if !isDelay(err, vfserr.DelayCapacity) {
		t.Errorf("a write whose context ended in the repack: %v, want ErrDelay(DelayCapacity)", err)
	}
}

// A write that waited for the raise lands with the time the journal took it,
// not the time it arrived.
func TestAWaitedWriteTakesTheTimeItWasServed(t *testing.T) {
	s, g := atLimit(t)
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	w := startWrite(t, s, ctx)

	w.mustPark(t)
	s.clock.Advance(time.Hour)
	commitRaise(t, s, g, cancel)

	if err := await(w.done); err != nil {
		t.Fatalf("the write after the raise: %v, want served", err)
	}

	o, pending, err := s.e.Overlay(t.Context(), s.one.ID, w.file)
	if err != nil || !pending || !o.Mtime.Equal(s.clock.Now()) {
		t.Errorf("overlay mtime %v pending %v, %v; want %v, the time it was served",
			o.Mtime, pending, err, s.clock.Now())
	}
}

// A write accepted while journal_full is being raised for the headroom never
// leaves the condition up with nothing to clear it: whichever order the two
// run in, the condition is down, or the headroom is named and the next
// accepted write clears it.
func TestJournalFullRaisedBesideAnAcceptedWriteCanBeCleared(t *testing.T) {
	u := newUnit(t)

	for range 300 {
		var wg sync.WaitGroup

		wg.Add(2)

		go func() {
			defer wg.Done()
			u.e.RaiseFullForTest(u.share.ID)
		}()

		go func() {
			defer wg.Done()

			// Starts once the flag is set, the moment the old order left the
			// condition not yet raised.
			for !u.e.HeadroomNamedForTest(u.share.ID) {
				runtime.Gosched()
			}

			u.e.AcceptedWriteForTest(u.share.ID)
		}()

		wg.Wait()

		if len(fullConds(u.e)) != 0 && !u.e.HeadroomNamedForTest(u.share.ID) {
			t.Fatal("journal_full stays up with the headroom not named: no accepted write clears it")
		}

		u.e.AcceptedWriteForTest(u.share.ID)
		u.e.AcceptedWriteForTest(u.share.ID)
	}
}

// The refusal path answers by the journal's UnreclaimedEndedBytes once its
// Repack answers, and the journal sets the figure only when Repack seals. This
// holds the pair together: while the real journal's Repack is not built it
// answers ErrNotImplemented, which the path reads as "no figure yet" and
// answers retry-later; the day it does anything else, this test fails, so the
// path is changed to read the figure before every capacity refusal over the
// real journal answers no space by a figure of 0. The call runs on a journal
// that was never opened, which a built Repack cannot serve, so the test needs
// no segments on disk.
func TestTheRealJournalsRepackIsNotBuiltWhileItsEndedFigureIsZero(t *testing.T) {
	const built = "the real journal's Repack is built: withdraw the ponytail in relieve and " +
		"make this test assert a non-zero UnreclaimedEndedBytes after a Repack that seals"

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s (it panicked on a journal never opened: %v)", built, r)
		}
	}()

	var j journal.Journal

	if _, err := j.Repack(t.Context(), 0, journal.RepackScope{}); !errors.Is(err, journal.ErrNotImplemented) {
		t.Fatalf("%s (it answered %v)", built, err)
	}

	if n := j.Stats().UnreclaimedEndedBytes; n != 0 {
		t.Errorf("UnreclaimedEndedBytes = %d with Repack not built, want 0", n)
	}
}
