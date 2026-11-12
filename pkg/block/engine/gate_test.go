package engine_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// unbind unbinds the unit's share.
func (u *unit) unbind(t *testing.T) {
	t.Helper()

	if err := u.e.Unbind(t.Context(), u.share.ID, false); err != nil {
		t.Fatalf("Unbind: %v", err)
	}
}

// joins answers a channel that receives what each join of the engine waits
// for, once it has to wait.
func joins(e *engine.Engine) <-chan string {
	out := make(chan string, 8)

	e.OnJoining(func(what string) {
		select {
		case out <- what:
		default:
		}
	})

	return out
}

// unbinding runs Unbind of the unit's share in a goroutine and answers its
// result.
func (u *unit) unbinding(t *testing.T) <-chan error {
	t.Helper()

	out := make(chan error, 1)

	go func() {
		out <- u.e.Unbind(t.Context(), u.share.ID, false)
	}()

	return out
}

// isStale reports whether err is ErrStale and not a Delay.
func isStale(err error) bool {
	var d *vfserr.Delay

	return errors.Is(err, vfserr.ErrStale) && !errors.As(err, &d)
}

// Once a share's gate is shut, a unit is refused with ErrStale, never with
// DelayFrozen or ErrDelay, and the units entered before it stay counted.
func TestEnterIsRefusedWithStaleOnceTheGateIsShut(t *testing.T) {
	u := newUnit(t)
	other := u.bindShare(t, "other", entity.NamespaceID{})

	leave, err := u.e.EnterForTest(u.share.ID)
	if err != nil {
		t.Fatalf("enter on an open gate: %v", err)
	}

	joined := joins(u.e)
	unbound := u.unbinding(t)

	select {
	case <-joined:
	case err := <-unbound:
		t.Fatalf("Unbind = %v with a unit still entered, want it to wait", err)
	}

	if _, err := u.e.EnterForTest(u.share.ID); !isStale(err) {
		t.Errorf("enter on a shut gate = %v, want exactly ErrStale", err)
	}

	if got := u.e.InFlightForTest(u.share.ID); got != 1 {
		t.Errorf("units entered after a refused enter = %d, want 1", got)
	}

	leaveOther, err := u.e.EnterForTest(other)
	if err != nil {
		t.Errorf("enter on another share: %v", err)
	} else {
		leaveOther()
	}

	leave()

	if err := <-unbound; err != nil {
		t.Errorf("Unbind after the unit left = %v, want nil", err)
	}
}

// A call that enters two shares and is refused by the second leaves nothing
// registered on the first, whichever of the two is being removed.
func TestATwoShareRegistrationWithOneShareRemovingLeavesNothingOnTheOther(t *testing.T) {
	u := newUnit(t)
	removing := u.bindShare(t, "removing", entity.NamespaceID{})
	src, dst := entity.NewFileID(), entity.NewFileID()
	x := vfserr.Extent{Len: 1}

	if err := u.e.Unbind(t.Context(), removing, false); err != nil {
		t.Fatalf("Unbind: %v", err)
	}

	t.Run("the destination is removing", func(t *testing.T) {
		err := u.e.Clone(t.Context(), u.share.ID, src, removing, dst, x)
		if !isStale(err) {
			t.Errorf("clone into a removing share = %v, want exactly ErrStale", err)
		}

		if got := u.e.InFlightForTest(u.share.ID); got != 0 {
			t.Errorf("units left on the source share = %d, want 0", got)
		}

		u.unbind(t)
	})

	t.Run("the source is removing", func(t *testing.T) {
		err := u.e.Clone(t.Context(), removing, src, u.share.ID, dst, x)
		if !isStale(err) {
			t.Errorf("clone from a removing share = %v, want exactly ErrStale", err)
		}

		if got := u.e.InFlightForTest(u.share.ID); got != 0 {
			t.Errorf("units left on the destination share = %d, want 0", got)
		}
	})
}

// A call that only checks, Draw, registers nothing: its share is idle after
// it, and a shut share refuses it.
func TestACheckOnlyCallRegistersNothing(t *testing.T) {
	u := newUnit(t)
	f := u.create(t, "f")

	if _, err := u.e.Draw(t.Context(), u.share.ID, f); err != nil {
		t.Fatalf("draw on an open gate: %v", err)
	}

	if got := u.e.InFlightForTest(u.share.ID); got != 0 {
		t.Errorf("units entered after a draw = %d, want 0", got)
	}

	u.unbind(t)

	if _, err := u.e.Draw(t.Context(), u.share.ID, f); !isStale(err) {
		t.Errorf("draw on a shut share = %v, want exactly ErrStale", err)
	}
}

// contentCall is one content call on the unit's share, named, made on file f
// and, for a clone, g.
type contentCall struct {
	name string
	call func(f, g entity.FileID) error
}

// contentCalls are the calls that enter the unit's share's gate, each made on
// the share and, for the clone, on other.
func (u *unit) contentCalls(ctx context.Context, other entity.ShareID) []contentCall {
	id := u.share.ID
	x := vfserr.Extent{Len: 1}

	return []contentCall{
		{"Write", func(f, g entity.FileID) error {
			_, err := u.e.Write(ctx, id, f, 0, []byte("x"), engine.WriteOpts{})

			return err
		}},
		{"Read", func(f, g entity.FileID) error {
			_, err := u.e.Read(ctx, id, f, 0, make([]byte, 1))

			return err
		}},
		{"Commit", func(f, g entity.FileID) error {
			_, _, err := u.e.Commit(ctx, id, f, 0, 1)

			return err
		}},
		{"Truncate", func(f, g entity.FileID) error {
			return u.e.Truncate(ctx, id, f, 0)
		}},
		{"Deallocate", func(f, g entity.FileID) error {
			return u.e.Deallocate(ctx, id, f, 0, 1)
		}},
		{"Delete", func(f, g entity.FileID) error {
			_, err := u.e.Delete(ctx, id, f)

			return err
		}},
		{"Draw", func(f, g entity.FileID) error {
			_, err := u.e.Draw(ctx, id, f)

			return err
		}},
		{"Allocation", func(f, g entity.FileID) error {
			_, err := u.e.Allocation(ctx, id, f, 0)

			return err
		}},
		{"Overlay", func(f, g entity.FileID) error {
			_, _, err := u.e.Overlay(ctx, id, f)

			return err
		}},
		{"Clone as source", func(f, g entity.FileID) error {
			return u.e.Clone(ctx, id, f, other, g, x)
		}},
		{"Clone as destination", func(f, g entity.FileID) error {
			return u.e.Clone(ctx, other, f, id, g, x)
		}},
	}
}

// Every content call that answers an error answers exactly ErrStale once the
// share's gate is shut, registers nothing, and leaves no clone registration.
func TestAShutShareMakesEveryContentCallAnswerStale(t *testing.T) {
	u := newUnit(t)
	other := u.bindShare(t, "other", entity.NamespaceID{})
	id := u.share.ID
	ctx := t.Context()

	u.unbind(t)

	calls := u.contentCalls(ctx, other)

	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			f, g := u.create(t, c.name), entity.NewFileID()

			if err := c.call(f, g); !isStale(err) {
				t.Errorf("%s on a shut share = %v, want exactly ErrStale", c.name, err)
			}

			if got := u.e.InFlightForTest(id); got != 0 {
				t.Errorf("units entered after %s = %d, want 0", c.name, got)
			}

			if u.e.HasEntry(f) || u.e.HasEntry(g) {
				t.Errorf("%s left an entry or a clone registration", c.name)
			}
		})
	}

	shares := u.rec.shares()

	if err := u.e.Bind(ctx, u.share); !isStale(err) {
		t.Errorf("Bind of a shut share = %v, want exactly ErrStale", err)
	}

	if got := u.rec.shares(); got != shares {
		t.Errorf("Bind opened a handle: %d share calls, want %d", got, shares)
	}
}

// Every enter that Unbind's shut refuses answers exactly ErrStale, never a
// delay, however close to the shut it was made.
func TestEveryEnterRefusedByARacingUnbindAnswersStale(t *testing.T) {
	const (
		rounds  = 200
		workers = 8
	)

	u := newUnit(t)

	for round := range rounds {
		id := u.bindShare(t, fmt.Sprintf("race%d", round), entity.NamespaceID{})

		var (
			wg    sync.WaitGroup
			wrong atomic.Value
		)

		for range workers {
			wg.Add(1)

			go func() {
				defer wg.Done()

				for {
					leave, err := u.e.EnterForTest(id)
					if err != nil {
						if !isStale(err) {
							wrong.Store(err)
						}

						return
					}

					leave()
				}
			}()
		}

		if err := u.e.Unbind(t.Context(), id, false); err != nil {
			t.Fatalf("round %d: Unbind: %v", round, err)
		}

		wg.Wait()

		if err := wrong.Load(); err != nil {
			t.Fatalf("round %d: a refused enter answered %v, want exactly ErrStale", round, err)
		}
	}
}

// Every content call that enters an open share's gate leaves it again, whether
// it succeeds or is refused for another reason: nothing is left counted, so a
// later Unbind has nothing to wait for.
func TestAContentCallOnAnOpenShareLeavesItsGate(t *testing.T) {
	u := newUnit(t)
	other := u.bindShare(t, "other", entity.NamespaceID{})
	id := u.share.ID

	if err := u.e.Apply(engine.Settings{
		ExistenceAge:        time.Minute,
		GroupCommitMaxFiles: 1,
		CloneMaxLen:         1 << 30,
	}); err != nil {
		t.Fatal(err)
	}

	u.e.SetAfterRegister(func() error { return engine.ErrStopped }) // a clone past its gates stops, its files being none the store holds

	for _, c := range u.contentCalls(t.Context(), other) {
		t.Run(c.name, func(t *testing.T) {
			f, g := u.create(t, c.name), entity.NewFileID()

			u.write(t, f, 0, []byte("x"))

			if err := c.call(f, g); isStale(err) {
				t.Errorf("%s on an open share = %v, want anything but ErrStale", c.name, err)
			}

			if got := u.e.InFlightForTest(id); got != 0 {
				t.Errorf("units entered after %s = %d, want 0", c.name, got)
			}

			if got := u.e.InFlightForTest(other); got != 0 {
				t.Errorf("units entered on the other share after %s = %d, want 0", c.name, got)
			}
		})
	}
}
