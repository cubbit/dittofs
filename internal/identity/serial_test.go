package identity_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/identity"
	pid "github.com/cubbit/dittofs/pkg/identity"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv/kvtest"
	"github.com/cubbit/dittofs/pkg/sim"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

func apply(t *testing.T, s *identity.Server, st identity.State) uint64 {
	t.Helper()
	if err := s.Apply(st); err != nil {
		t.Fatalf("Apply(%+v): %v", st, err)
	}
	return s.Current()
}

// The serial moves with the State, not with each Apply: an equal State keeps
// it, whatever Serial its caller passed, and a differing one raises it, after
// a Stop too.
func TestApplyRaisesTheSerialOnlyForADifferingState(t *testing.T) {
	s := newServer(t)
	if got := s.Current(); got != 0 {
		t.Fatalf("Current before any Apply = %d, want 0", got)
	}
	first := apply(t, s, identity.State{Generation: 1})
	if first == 0 {
		t.Fatalf("Current after the first Apply = 0, want a serial")
	}
	if got := apply(t, s, identity.State{Generation: 1}); got != first {
		t.Errorf("Current after an equal Apply = %d, want %d kept", got, first)
	}
	if got := apply(t, s, identity.State{Generation: 1, Serial: first + 7}); got != first {
		t.Errorf("Current after an equal Apply passing its own Serial = %d, want %d kept", got, first)
	}
	raised := apply(t, s, identity.State{Generation: 2})
	if raised <= first {
		t.Errorf("Current after a raised generation = %d, want above %d", raised, first)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := apply(t, s, identity.State{Generation: 2}); got <= raised {
		t.Errorf("Current after Stop and an Apply = %d, want above %d: a restarted server repeated a serial", got, raised)
	}
}

// Two differing States applied at once each get a serial of their own, and
// the one left current holds the greater: over many rounds, every round
// raises Current by exactly two. A load, compare and store outside one lock
// gives the two one serial, or leaves the lesser current.
func TestConcurrentAppliesNeverShareASerial(t *testing.T) {
	s := newServer(t)
	for round := range uint64(10000) {
		base := 3 * round
		before := apply(t, s, identity.State{Generation: base})
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, g := range []uint64{base + 1, base + 2} {
			wg.Go(func() {
				<-start
				if err := s.Apply(identity.State{Generation: g}); err != nil {
					t.Errorf("Apply: %v", err)
				}
			})
		}
		close(start)
		wg.Wait()
		if got := s.Current(); got != before+2 {
			t.Fatalf("round %d: Current after two differing concurrent Applies = %d, want %d", round, got, before+2)
		}
		if got := s.Applied().Serial; got != before+2 {
			t.Fatalf("round %d: the current State's Serial = %d, want %d", round, got, before+2)
		}
	}
}

// A Resolve stamps its caller with the State it loaded, and a pass an Apply
// overtook runs once more: an Apply after the first pass's load gives the
// second pass's serial, and an Apply after the second pass's load is served
// anyway, still stamped with the State that pass loaded.
func TestResolveStampsTheStateItLoaded(t *testing.T) {
	r := newResolving(t)
	alice := entity.User{Principal: entity.NewUser(), Name: "alice", IDs: fileIDs("1000")}
	gen := r.user(t, alice, 0)
	apply(t, r.srv, identity.State{Generation: gen})
	var second uint64
	passes := 0
	r.srv.OnStateLoaded(func() {
		passes++
		switch passes {
		case 1:
			second = apply(t, r.srv, identity.State{Generation: gen + 1})
		case 2:
			apply(t, r.srv, identity.State{Generation: gen + 2})
		}
	})
	c, err := r.srv.Resolve(context.Background(), ticket(alice.Principal, alice.Name), entity.SharePolicy{}, pid.ReadOnlyNotNarrowed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Built != second {
		t.Errorf("Caller.Built = %d, want %d, the serial the second pass loaded (Current %d)", c.Built, second, r.srv.Current())
	}
	if passes != 2 {
		t.Errorf("Resolve ran %d passes, want 2", passes)
	}
}

// RFC 18's in-flight disable, resolution's half: bob's record is read enabled,
// from a view begun before the disable, and held there while bob is disabled
// and the Apply carrying it lands. Released, the call is refused: the stale
// pass resolved once more and read the disable. A State loaded after the
// store reads, or a stale caller answered, serves bob.
func TestResolveOvertakenByADisableIsRefused(t *testing.T) {
	r := newResolving(t)
	bob := entity.User{Principal: entity.NewUser(), Name: "bob", IDs: fileIDs("1001")}
	gen := r.user(t, bob, 0)
	apply(t, r.srv, identity.State{Generation: gen})

	clock := sim.NewTestClock(time.Unix(0, 0))
	hold, err := sim.NewSchedule(1, clock, sim.Rule{Op: kvtest.OpGet, Key: "U‖*", Count: 1, Action: sim.FaultAction{Delay: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	r.fault.SetFault(hold)

	type result struct {
		c   *pid.Caller
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := r.srv.Resolve(context.Background(), ticket(bob.Principal, bob.Name), entity.SharePolicy{}, pid.ReadOnlyNotNarrowed)
		done <- result{c, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for clock.Pending() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("bob's user read never reached the hold")
		}
		runtime.Gosched()
	}

	bob.Disabled = true
	apply(t, r.srv, identity.State{Generation: r.user(t, bob, gen)})
	clock.Advance(time.Hour)

	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Resolve never returned after the hold was released")
	}
	if !errors.Is(got.err, vfserr.ErrAccess) || got.c != nil {
		t.Fatalf("Resolve overtaken by bob's disable = %+v, %v; want nil, ErrAccess", got.c, got.err)
	}
}
