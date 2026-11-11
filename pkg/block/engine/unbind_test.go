package engine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// heldCall is a Write held inside its handle's WriteAt, after the engine
// admitted it: the call a join has to wait for.
type heldCall struct {
	u       *unit
	h       *heldWrite
	release func()
	wrote   chan error
}

// holdAWrite starts a Write that stays inside the journal until released.
func holdAWrite(t *testing.T) *heldCall {
	t.Helper()

	u, h := newHeldUnit(t)
	f := u.create(t, "held")
	h.staged, h.release = make(chan struct{}), make(chan struct{})

	c := &heldCall{u: u, h: h, wrote: make(chan error, 1)}

	var released bool

	c.release = func() {
		if !released {
			released = true

			close(h.release)
		}
	}

	t.Cleanup(c.release)

	go func() {
		_, err := u.e.Write(
			context.Background(),
			u.share.ID,
			f,
			0,
			[]byte("x"),
			engine.WriteOpts{Done: func() {}},
		)
		c.wrote <- err
	}()

	<-h.staged

	return c
}

// writeTo writes one byte to f and answers the call's error.
func (u *unit) writeTo(t *testing.T, f entity.FileID) error {
	t.Helper()

	_, err := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("x"), engine.WriteOpts{})

	return err
}

// A refused keepHolds changes nothing: the share still serves, and a later
// Unbind is not told to retry.
func TestUnbindRefusesKeepHoldsAndChangesNothing(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")

	if err := u.e.Unbind(t.Context(), u.share.ID, true); !errors.Is(err, vfserr.ErrInvalid) {
		t.Fatalf("Unbind with keepHolds = %v, want ErrInvalid", err)
	}

	if err := u.writeTo(t, f); err != nil {
		t.Errorf("Write after the refused Unbind = %v, want nil", err)
	}

	if err := u.e.Unbind(t.Context(), u.share.ID, false); err != nil {
		t.Errorf("Unbind after the refused one = %v, want nil", err)
	}
}

// An Unbind waits for a call admitted before it, and refuses a new one
// meanwhile: the admitted call is not refused.
func TestUnbindWaitsForACallAdmittedBeforeIt(t *testing.T) {
	c := holdAWrite(t)
	u := c.u
	joined := joins(u.e)
	unbound := u.unbinding(t)

	select {
	case what := <-joined:
		if what != "work in flight" {
			t.Errorf("Unbind waits for %q, want %q", what, "work in flight")
		}
	case err := <-unbound:
		t.Fatalf("Unbind = %v with a call in flight, want it to wait", err)
	}

	f := u.create(t, "second")

	if err := u.writeTo(t, f); !isStale(err) {
		t.Errorf("Write while Unbind waits = %v, want exactly ErrStale", err)
	}

	c.release()

	if err := <-c.wrote; err != nil {
		t.Errorf("the admitted Write = %v, want nil", err)
	}

	if err := <-unbound; err != nil {
		t.Errorf("Unbind = %v, want nil", err)
	}
}

// While an Unbind joins, another share binds and serves, a second lifecycle
// call on the share is told to retry, and the lifecycle mutex is free.
func TestUnbindKeepsTheLifecycleMutexFreeWhileItJoins(t *testing.T) {
	c := holdAWrite(t)
	u := c.u
	joined := joins(u.e)
	unbound := u.unbinding(t)

	select {
	case <-joined:
	case err := <-unbound:
		t.Fatalf("Unbind = %v with a call in flight, want it to wait", err)
	}

	if !u.e.LifecycleLockFree() {
		t.Error("the lifecycle mutex is held while Unbind joins")
	}

	other := u.bindShare(t, "other", entity.NamespaceID{})

	if _, err := u.e.Draw(t.Context(), other, entity.NewFileID()); err != nil {
		t.Errorf("a call on another share while Unbind joins = %v, want nil", err)
	}

	var d *vfserr.Delay

	if err := u.e.Unbind(t.Context(), u.share.ID, false); !errors.As(err, &d) ||
		d.Cause != vfserr.DelayConflict {
		t.Errorf("a second Unbind of the share = %v, want ErrDelay(DelayConflict)", err)
	}

	if err := u.e.Bind(t.Context(), u.share); !errors.As(err, &d) ||
		d.Cause != vfserr.DelayConflict {
		t.Errorf("Bind of the share = %v, want ErrDelay(DelayConflict)", err)
	}

	// Stats must return while Unbind joins; a Stats that waited for the share
	// would hang the test here.
	_ = u.e.Stats()

	c.release()

	if err := <-unbound; err != nil {
		t.Errorf("Unbind = %v, want nil", err)
	}
}

// An Unbind whose context ends returns that error, leaves the share shut and
// frees its lifecycle entry; a retry joins the call again and succeeds once it
// returned.
func TestUnbindEndsWithItsContextAndFreesTheLifecycleEntry(t *testing.T) {
	c := holdAWrite(t)
	u := c.u
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	if err := u.e.Unbind(cancelled, u.share.ID, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("Unbind under a cancelled context = %v, want context.Canceled", err)
	}

	f := u.create(t, "after")

	if err := u.writeTo(t, f); !isStale(err) {
		t.Errorf("Write after the ended Unbind = %v, want exactly ErrStale", err)
	}

	joined := joins(u.e)
	retried := u.unbinding(t)

	select {
	case <-joined:
	case err := <-retried:
		t.Fatalf("the retried Unbind = %v with the call still in flight, want it to wait", err)
	}

	c.release()

	if err := <-retried; err != nil {
		t.Errorf("the retried Unbind = %v, want nil", err)
	}

	if err := u.e.Unbind(t.Context(), u.share.ID, false); err != nil {
		t.Errorf("a third Unbind = %v, want nil", err)
	}
}

// Close waits for an Unbind in progress: it closes nothing under it.
func TestCloseWaitsForAnUnbindAndADropInProgress(t *testing.T) {
	t.Run("an Unbind in its call join", func(t *testing.T) {
		c := holdAWrite(t)
		u := c.u
		joined := joins(u.e)
		unbound := u.unbinding(t)

		select {
		case <-joined:
		case err := <-unbound:
			t.Fatalf("Unbind = %v with a call in flight, want it to wait", err)
		}

		past, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()

		err := u.e.Close(past)
		if !unavailable(err) || !strings.Contains(err.Error(), fmt.Sprint(u.share.ID)) {
			t.Errorf("Close = %v, want ErrUnavailable naming the share", err)
		}

		if got := u.ev.list(); len(got) != 0 {
			t.Errorf("events %v while Unbind waits, want none", got)
		}

		c.release()

		if err := <-unbound; err != nil {
			t.Fatalf("Unbind = %v, want nil", err)
		}

		if err := u.e.Close(t.Context()); err != nil {
			t.Fatalf("second Close = %v, want nil", err)
		}

		want := []string{fmt.Sprintf("handle close %d", u.share.Tag), "journal close"}
		if got := u.ev.list(); !slices.Equal(got, want) {
			t.Errorf("events %v, want %v", got, want)
		}
	})
}

// After Close began, Unbind answers what a content call does, and changes
// nothing.
func TestUnbindAfterCloseAnswersClosed(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")

	if err := u.e.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	_, want := u.e.Write(t.Context(), u.share.ID, f, 0, []byte("x"), engine.WriteOpts{})
	got := u.e.Unbind(t.Context(), u.share.ID, false)

	if got == nil || want == nil || got.Error() != want.Error() || !unavailable(got) {
		t.Errorf(
			"Unbind after Close = %v, want what Write answers: %v, an ErrUnavailable",
			got,
			want,
		)
	}

	if errors.Is(got, engine.ErrUnbuilt) {
		t.Error("Unbind answers the stub's error")
	}
}

// A share with no binding, and one already removed, answer nil and touch no
// journal.
func TestUnbindOfAShareWithNoBindingAnswersNil(t *testing.T) {
	u := newUnit(t)

	if err := u.e.Unbind(t.Context(), entity.NewShareID(), false); err != nil {
		t.Errorf("Unbind of a share with no binding = %v, want nil", err)
	}
}
