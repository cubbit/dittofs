//go:build engine_conformance

package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// tryWrite writes a byte at the start of id, answering what Write answers.
func tryWrite(f *enginetest.Fixture, id entity.FileID) (vfserr.Verifier, error) {
	return f.E.Write(context.Background(), f.Share.ID, id, 0, []byte("x"), engine.WriteOpts{Done: func() {}})
}

// isFenced reports that err is the self-fence's answer.
func isFenced(err error) bool {
	var d *vfserr.Delay
	return errors.As(err, &d) && d.Cause == vfserr.DelayCommit
}

// nodeFenced reports whether Stats raises node_fenced.
func nodeFenced(f *enginetest.Fixture) bool {
	for _, c := range f.E.Stats().Node.Conditions {
		if c.Name == "node_fenced" {
			return true
		}
	}
	return false
}

// wait fails the test unless ch closes within five seconds.
func wait(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("after 5 s: %s", what)
	}
}

// §15.2, self-fence threshold: the store stalled 10 s, writes are
// acknowledged; 31 s, writes and stability points answer ErrDelay, reads are
// served and a condition names the store; once a write commits, writes resume
// under the same verifier.
func TestRFC8B_SelfFenceThreshold(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	v := write(t, f, id, 0, []byte("x"))
	in := f.HoldNextWrite(40*time.Second, nil)
	f.Clock.Advance(enginetest.Settings.ExistenceAge) // the write's existence falls due
	wait(t, "the existence commit reaches the store", in)

	f.Clock.Advance(10 * time.Second)
	if _, err := tryWrite(f, id); err != nil {
		t.Fatalf("a write 10s into the stall: %v, want acknowledged", err)
	}
	f.Clock.Advance(21 * time.Second)
	if _, err := tryWrite(f, id); !isFenced(err) {
		t.Errorf("a write 31s into the stall: %v, want ErrDelay(DelayCommit)", err)
	}
	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); !isFenced(err) {
		t.Errorf("a stability point 31s into the stall: %v, want ErrDelay(DelayCommit)", err)
	}
	if got := read(t, f, id, 0, 1); string(got) != "x" {
		t.Errorf("a read while fenced: %q, want the byte written", got)
	}
	if !nodeFenced(f) {
		t.Error("no condition names the store while fenced")
	}

	f.Clock.Advance(9 * time.Second) // the stall ends and its transaction commits
	eventually(t, "a write acknowledged once the stall committed", func() bool { _, err := tryWrite(f, id); return err == nil })
	if got := write(t, f, id, 0, []byte("x")); got != v {
		t.Error("the verifier changed across the fence")
	}
}

// §15.2, self-fence on an erroring store: every write transaction failing at
// once while reads succeed fences after 30 s and resumes once a write commits:
// a trigger on "no transaction completed" never fires.
func TestRFC8B_SelfFenceFailing(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	write(t, f, id, 0, []byte("x"))
	in := f.FailWrites()
	f.Clock.Advance(enginetest.Settings.ExistenceAge)
	wait(t, "the existence commit reaches the store", in)
	for range 31 { // in steps, so the group commit retries throughout, each attempt failing
		f.Clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	if _, err := tryWrite(f, id); !isFenced(err) {
		t.Errorf("a write 31s into the failures: %v, want ErrDelay(DelayCommit)", err)
	}
	if got := read(t, f, id, 0, 1); string(got) != "x" {
		t.Errorf("a read while fenced: %q", got)
	}
	f.Heal()
	eventually(t, "a write acknowledged once one commits", func() bool {
		f.Clock.Advance(time.Second)
		_, err := tryWrite(f, id)
		return err == nil
	})
}

// §15.2, self-fence on an erroring store: a node idle for 60 s with no write
// does not fence.
func TestRFC8B_SelfFenceIdle(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	f.Clock.Advance(time.Minute)
	if _, err := tryWrite(f, id); err != nil {
		t.Errorf("a write after an idle minute: %v", err)
	}
}

// §15.2, self-fence on an erroring store: a one-shot write transaction that
// failed, then 60 s idle: no fence. One that keeps a one-shot failure as
// failing fires here.
func TestRFC8B_SelfFenceOneShotFailed(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	f.FailWrites()
	if err := f.E.Bind(t.Context(), f.Second); err == nil {
		t.Fatal("Bind over a failing store succeeded")
	}
	f.Heal()
	f.Clock.Advance(time.Minute)
	if _, err := tryWrite(f, id); err != nil {
		t.Errorf("a write an idle minute after the failed one-shot: %v", err)
	}
}

// §15.2, self-fence on an erroring store: a one-shot call stalled past 30 s
// fences the node; once it fails, the fence lifts. One that keeps a one-shot
// failure as failing never lifts.
func TestRFC8B_SelfFenceOneShotStalled(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	in := f.HoldNextWrite(40*time.Second, errors.New("a stalled one-shot fails"))
	bound := make(chan error, 1)
	go func() { bound <- f.E.Bind(context.Background(), f.Second) }()
	wait(t, "Bind's raise reaches the store", in)
	f.Clock.Advance(31 * time.Second)
	if _, err := tryWrite(f, id); !isFenced(err) {
		t.Errorf("a write with a one-shot stalled 31s: %v, want ErrDelay(DelayCommit)", err)
	}
	f.Clock.Advance(9 * time.Second)
	if err := <-bound; err == nil {
		t.Fatal("the stalled Bind succeeded")
	}
	if _, err := tryWrite(f, id); err != nil {
		t.Errorf("a write once the one-shot failed: %v, want acknowledged", err)
	}
}

// §15.2, self-fence on an erroring store: every commit returning only per-file
// errors for 31 s does not fence. One that counts a file's own failure as the
// store's fires here.
func TestRFC8B_SelfFencePerFileErrors(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	_, other := f.File("b")
	write(t, f, id, 0, []byte("x"))
	in := f.FailFile(id)
	f.Clock.Advance(enginetest.Settings.ExistenceAge)
	wait(t, "the existence commit reaches the store", in)
	for range 31 {
		f.Clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	if _, err := tryWrite(f, other); err != nil {
		t.Errorf("a write after 31s of per-file errors: %v, want acknowledged", err)
	}
}

// §15.2, self-fence on an erroring store: 60 s idle, then a write's existence
// commit held for 1 s: a second write in that second is acknowledged. One
// timed from the last commit alone fires here.
func TestRFC8B_SelfFenceHeldAfterIdle(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	_, other := f.File("b")
	f.Clock.Advance(time.Minute)
	write(t, f, id, 0, []byte("x"))
	in := f.HoldNextWrite(time.Second, nil)
	f.Clock.Advance(enginetest.Settings.ExistenceAge)
	wait(t, "the existence commit reaches the store", in)
	if _, err := tryWrite(f, other); err != nil {
		t.Errorf("a second write while the first's commit is held: %v, want acknowledged", err)
	}
	f.Clock.Advance(time.Second)
	if n := f.E.Stats().SelfFences; n != 0 {
		t.Errorf("engine_self_fences_total %d, want 0", n)
	}
}

// The version limit's raise failing at once for 31 s with no write pending
// fences the node, and its commit clears the fence: the raise is a retrying
// owner whatever view method it calls. The draws bring the raise due.
func TestRFC8_SelfFenceVersionLimitRaise(t *testing.T) {
	f := enginetest.New(t)
	_, id := f.File("a")
	f.FailWrites()
	eventually(t, "the failing raise fences the node", func() bool {
		if _, err := f.E.Draw(t.Context(), f.Share.ID, id); err != nil && !isFenced(err) {
			t.Fatalf("Draw: %v", err)
		}
		f.Clock.Advance(time.Second)
		return nodeFenced(f)
	})
	f.Heal()
	eventually(t, "the raise commits and clears the fence", func() bool {
		f.Clock.Advance(time.Second)
		return !nodeFenced(f)
	})
}
