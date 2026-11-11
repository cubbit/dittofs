package engine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// refusedBubble is a bubble whose engine was started over a journal that
// refused its directory as unsuitable, with one share it names.
func refusedBubble(t *testing.T, fs *kvstoretest.FS) (*bubble, engine.Share) {
	t.Helper()

	return refusedBubbleWrapping(t, fs, nil)
}

// refusedBubbleWrapping is refusedBubble with every handle the fake journal
// opens passed through wrap, when it is set.
func refusedBubbleWrapping(
	t *testing.T,
	fs *kvstoretest.FS,
	wrap func(engine.Handle) engine.Handle,
) (*bubble, engine.Share) {
	t.Helper()

	b := buildBubble(t, fs, wrap)
	b.openFn = func(context.Context) (engine.Journal, error) {
		return nil, unsuitable()
	}

	s := bubbleShare(t, fs)

	if err := b.e.Start(context.Background(), []engine.Share{s}); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}

	// The retry arms its timer once it runs: wait for it to block.
	synctest.Wait()

	return b, s
}

// The retry fires at the 30th second, not before and not after: a timer of 20 s
// attaches the share at 29 s, and one of 40 s has not attached it at 30 s.
func TestTheRetryFiresAtThe30thSecond(t *testing.T) {
	fs, _ := bubbleFS(t)

	synctest.Test(t, func(t *testing.T) {
		b, s := refusedBubble(t, fs)
		b.openFn = nil

		b.clock.Advance(retryEvery - time.Second)
		synctest.Wait()

		if b.e.Stats().Shares[s.ID].Refused == nil {
			t.Error("attached before 30 s")
		}

		b.clock.Advance(time.Second)
		synctest.Wait()

		if err := b.e.Stats().Shares[s.ID].Refused; err != nil {
			t.Errorf("not attached at 30 s: %v", err)
		}

		if err := b.e.Close(context.Background()); err != nil {
			t.Errorf("Close = %v", err)
		}
	})
}

// A retry that Close overtook opens nothing: the 30 s that follow leave the
// share unattached and the journal unopened.
func TestCloseStopsTheRetryOfAnUnsuitableJournal(t *testing.T) {
	fs, _ := bubbleFS(t)

	synctest.Test(t, func(t *testing.T) {
		b, s := refusedBubble(t, fs)
		b.openFn = nil
		closed := closing(b.e, context.Background())

		synctest.Wait()

		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("Close = %v", err)
			}
		default:
			// A retry left waiting for its timer would hold Close: let it go.
			b.clock.Advance(retryEvery)

			if err := <-closed; err != nil {
				t.Errorf("Close = %v after the timer, want nil", err)
			}

			t.Fatal("Close waits for the retry's timer: it did not cancel the retry")
		}

		b.clock.Advance(retryEvery)
		synctest.Wait()

		if got := b.e.Stats().Shares[s.ID].Refused; got == nil {
			t.Error("the share is attached after Close: the retry opened the journal")
		}
	})
}

// A Start that fails after the open was refused leaves no retry behind: no
// timer is armed and the 30 s that follow open nothing.
func TestAFailedStartLeavesNoRetryOfAnUnsuitableJournal(t *testing.T) {
	fs, _ := bubbleFS(t)
	s := bubbleShare(t, fs)

	synctest.Test(t, func(t *testing.T) {
		// The raise at start is the one store call after a refused open, and
		// over no served shard the store runs no transaction a kv fault could
		// fail: the raise itself answers the error.
		b := buildBubble(t, fs, nil, func(d *engine.Deps) {
			d.Lifecycle = &raiseScript{Lifecycle: d.Lifecycle, answers: []error{errInjected}}
		})
		b.openFn = func(context.Context) (engine.Journal, error) {
			return nil, unsuitable()
		}

		if err := b.e.Start(context.Background(), []engine.Share{s}); !errors.Is(err, errInjected) {
			t.Fatalf("Start = %v, want the raise's error", err)
		}

		synctest.Wait()

		if got := b.clock.Pending(); got != 0 {
			t.Errorf("%d timers pending after a failed Start, want none", got)
		}

		b.openFn = nil
		b.clock.Advance(retryEvery)
		synctest.Wait()

		if err := b.e.BindingOf(s.ID); !isDelay(err, vfserr.DelayFrozen) {
			t.Errorf("the share answers %v after a failed Start, want it unattached", err)
		}
	})
}

// heldOpen makes the next Open of b wait at the returned gate, then answer
// the fake journal.
func heldOpen(b *bubble) (entered <-chan struct{}, release chan<- struct{}) {
	in := make(chan struct{})
	out := make(chan struct{})

	var once sync.Once

	b.openFn = func(context.Context) (engine.Journal, error) {
		once.Do(func() { close(in) })
		<-out

		return b.rec, nil
	}

	return in, out
}

// A retried open that is inside Open when Close begins starts no loop: once
// Open returns, the retry sees the closed state, closes what it opened and
// publishes nothing, and Close, which joined it, returns.
func TestCloseOvertakesALateAttach(t *testing.T) {
	fs, _ := bubbleFS(t)

	synctest.Test(t, func(t *testing.T) {
		b, s := refusedBubble(t, fs)
		entered, release := heldOpen(b)

		b.clock.Advance(retryEvery)
		synctest.Wait()

		select {
		case <-entered:
		default:
			t.Fatal("the retried open never reached Open")
		}

		closed := closing(b.e, context.Background())

		synctest.Wait()

		select {
		case err := <-closed:
			t.Fatalf("Close = %v while the retried open is inside Open, want it to wait", err)
		default:
		}

		close(release)

		if err := <-closed; err != nil {
			t.Errorf("Close = %v, want nil", err)
		}

		// The order of the closes is the requirement, so the check reads what
		// the recording journal noted of its own calls and forwarded to the
		// fake journal, which RFC 29 §4.11 allows for a call that is itself
		// the requirement.
		if got := b.ev.list(); !slices.Equal(got, []string{handleClose(s), "journal close"}) {
			t.Errorf("events %v, want the retry's handle and journal closed, in that order", got)
		}

		if got := b.e.Stats().Shares[s.ID].Refused; got == nil {
			t.Error("the share is attached: the retry published a binding after Close began")
		}
	})
}

// A retried open that never returns does not hold Close past the stop bound:
// Close reports it, and leaves the journal the retry may yet open to it.
func TestCloseGivesUpOnARetriedOpenThatNeverReturns(t *testing.T) {
	fs, _ := bubbleFS(t)

	synctest.Test(t, func(t *testing.T) {
		b, _ := refusedBubble(t, fs)
		entered, release := heldOpen(b)

		b.clock.Advance(retryEvery)
		synctest.Wait()

		select {
		case <-entered:
		default:
			t.Fatal("the retried open never reached Open")
		}

		closed := closing(b.e, context.Background())

		synctest.Wait()
		b.clock.Advance(engine.StopBoundDefault - time.Nanosecond)
		synctest.Wait()

		select {
		case err := <-closed:
			t.Fatalf("Close = %v before the stop bound", err)
		default:
		}

		b.clock.Advance(time.Nanosecond)
		synctest.Wait()

		select {
		case err := <-closed:
			if !unavailable(err) {
				t.Errorf("Close = %v at the stop bound, want ErrUnavailable", err)
			}
		default:
			close(release)

			if err := <-closed; err != nil {
				t.Errorf("Close = %v after the release, want nil", err)
			}

			t.Fatal("Close did not give up on the retried open at the stop bound")
		}

		close(release)
	})
}

// A retried open that finds the journal missing or foreign is a lost device,
// not an unsuitable one: journal_lost replaces journal_unsuitable, the shares
// stay unattached, no timer is left armed and a later 30 s changes nothing.
// The calls on the shares then answer what a journal lost at Start answers.
func TestARetriedOpenThatFindsTheJournalLostEndsTheRetry(t *testing.T) {
	for name, cause := range map[string]error{
		"missing": journal.ErrNoJournal,
		"foreign": journal.ErrIdentityMismatch,
	} {
		t.Run(name, func(t *testing.T) {
			fs, _ := bubbleFS(t)
			spare := enginetest.AddShare(t, fs.Store, "spare", 1<<30)

			synctest.Test(t, func(t *testing.T) {
				b, s := refusedBubble(t, fs)
				b.openFn = func(context.Context) (engine.Journal, error) {
					return nil, cause
				}

				b.clock.Advance(retryEvery)
				synctest.Wait()

				for range 2 {
					st := b.e.Stats()

					if _, ok := bubbleCondition(st, "journal_lost"); !ok {
						t.Error("journal_lost is not raised")
					}

					if _, ok := bubbleCondition(st, "journal_unsuitable"); ok {
						t.Error("journal_unsuitable is still raised beside journal_lost")
					}

					if got := st.Shares[s.ID].Refused; !errors.Is(got, cause) {
						t.Errorf("share refused with %v, want the lost journal's %v", got, cause)
					}

					if got := b.clock.Pending(); got != 0 {
						t.Errorf("%d timers pending after a lost journal, want none", got)
					}

					b.clock.Advance(retryEvery)
					synctest.Wait()
				}

				err := b.e.Bind(context.Background(), spare)
				if !errors.Is(err, engine.ErrNotStarted) {
					t.Errorf("Bind = %v, want what a journal lost at Start answers", err)
				}

				if err := b.e.Unbind(context.Background(), s.ID, false); err != nil {
					t.Errorf("Unbind = %v, want nil as for a journal lost at Start", err)
				}

				if err := b.e.Drop(context.Background(), s.ID); err != nil {
					t.Errorf("Drop = %v, want nil as for a journal lost at Start", err)
				}
			})
		})
	}
}

// bubbleCondition is the condition named name that st raises for the bubble's
// device, and whether there is one.
func bubbleCondition(st engine.Stats, name string) (engine.Condition, bool) {
	for _, c := range st.Journals[bubbleDevice].Conditions {
		if c.Name == name {
			return c, true
		}
	}

	return engine.Condition{}, false
}

// A retry that Close overtook closes what it opened without holding the
// lifecycle mutex: while the handle's close is held, the mutex is free (RFC 8
// §2.3: never held across I/O).
func TestALateAttachThatCloseOvertookClosesWithoutTheLifecycleMutex(t *testing.T) {
	fs, _ := bubbleFS(t)

	synctest.Test(t, func(t *testing.T) {
		hc := &heldClose{entered: make(chan struct{}), release: make(chan struct{})}
		wrap := func(h engine.Handle) engine.Handle {
			hc.Handle = h

			return hc
		}
		b, _ := refusedBubbleWrapping(t, fs, wrap)
		entered, release := heldOpen(b)

		b.clock.Advance(retryEvery)
		synctest.Wait()

		select {
		case <-entered:
		default:
			t.Fatal("the retried open never reached Open")
		}

		closed := closing(b.e, context.Background())

		synctest.Wait()
		close(release)
		synctest.Wait()

		select {
		case <-hc.entered:
		default:
			t.Fatal("the retry did not close the handle it opened")
		}

		if !b.e.LifecycleMutexFree() {
			t.Error("the lifecycle mutex is held while the retry closes its handle")
		}

		hc.open()

		if err := <-closed; err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
	})
}

// A retried open that fails after Close began leaves the cause as it was: an
// error that comes after the stop is the Close's, not the journal's, so Stats
// and journal_unsuitable still name the unsuitable storage, and Close returns.
func TestAnOpenThatFailsAfterCloseBeganLeavesTheCauseAsItWas(t *testing.T) {
	fs, _ := bubbleFS(t)

	synctest.Test(t, func(t *testing.T) {
		b, s := refusedBubble(t, fs)
		entered := make(chan struct{})
		release := make(chan struct{})
		b.openFn = func(context.Context) (engine.Journal, error) {
			close(entered)
			<-release

			return nil, errors.New("io error after the stop")
		}

		b.clock.Advance(retryEvery)
		synctest.Wait()

		select {
		case <-entered:
		default:
			t.Fatal("the retried open never reached Open")
		}

		closed := closing(b.e, context.Background())

		synctest.Wait()
		close(release)

		if err := <-closed; err != nil {
			t.Errorf("Close = %v, want nil", err)
		}

		st := b.e.Stats()
		if got := st.Shares[s.ID].Refused; !errors.Is(got, journal.ErrUnsuitableStorage) {
			t.Errorf("share refused with %v after Close, want ErrUnsuitableStorage", got)
		}

		c, ok := bubbleCondition(st, "journal_unsuitable")
		if !ok || !errors.Is(c.Cause, journal.ErrUnsuitableStorage) {
			t.Errorf("journal_unsuitable = %+v (raised %v), want its cause unchanged", c, ok)
		}
	})
}

// A Close that meets the retry's timer already fired opens nothing: the retry
// sees the closed state before it opens the directory, whichever of the timer
// and the cancelled context it wakes on. The bubble makes the order the same
// each time, so a few runs are enough; a retry that opened would leave the journal
// it opened, and closed, in the events.
func TestACloseThatMeetsTheFiredRetryTimerOpensNothing(t *testing.T) {
	for i := range 3 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			fs, _ := bubbleFS(t)

			synctest.Test(t, func(t *testing.T) {
				b, s := refusedBubble(t, fs)
				b.openFn = nil

				b.clock.Advance(retryEvery)

				if err := b.e.Close(context.Background()); err != nil {
					t.Fatalf("Close = %v", err)
				}

				if got := b.ev.list(); len(got) != 0 {
					t.Errorf("events %v after Close, want none: the retry opened the journal", got)
				}

				if got := b.e.Stats().Shares[s.ID].Refused; got == nil {
					t.Error("the share is attached after Close")
				}
			})
		})
	}
}
