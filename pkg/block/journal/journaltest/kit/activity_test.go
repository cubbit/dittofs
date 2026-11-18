package kit

import (
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/sim"
)

// Activity reads a call parked at a hold as not acting and a call stalled
// by a fault's delay as acting, and every call's start and end as a move.
func TestActivityTellsParkedCallsFromActingOnes(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "a")

	idle, acting := s.Activity()
	if acting {
		t.Fatalf("Activity on an idle seam: acting = true, want false")
	}
	if again, _ := s.Activity(); again != idle {
		t.Fatalf("Activity on an idle seam: moves went from %d to %d, want no move", idle, again)
	}

	held := s.Hold(t, OpSync, "a")
	synced := make(chan error, 1)
	go func() { synced <- f.Sync() }()
	<-held.Reached()
	parked, acting := s.Activity()
	if acting {
		t.Fatalf("Activity with the only call parked at a hold: acting = true, want false")
	}
	if parked == idle {
		t.Fatalf("Activity after a call started: moves = %d, unchanged, want a move", parked)
	}
	held.Release()
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	if ended, _ := s.Activity(); ended == parked {
		t.Fatalf("Activity after the held call ended: moves = %d, unchanged, want a move", ended)
	}

	c := sim.NewTestClock(time.Unix(0, 0))
	sched, err := sim.NewSchedule(1, c, sim.Rule{Op: OpSync, Key: "a", Action: sim.FaultAction{Delay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	s.SetFault(sched)
	go func() { synced <- f.Sync() }()
	waitPending(t, c)
	if _, acting := s.Activity(); !acting {
		t.Fatalf("Activity with a call stalled by a delay: acting = false, want true")
	}
	c.Advance(time.Second)
	if err := <-synced; err != nil {
		t.Fatal(err)
	}
	if _, acting := s.Activity(); acting {
		t.Fatalf("Activity once the delayed call ended: acting = true, want false")
	}
}

func TestActivityCountsAParkedLink(t *testing.T) {
	s := newSeam(t)
	f := create(t, s, "a")
	syncFile(t, f)

	idle, _ := s.Activity()

	held := s.Hold(t, OpLink, "a")
	linked := make(chan error, 1)

	go func() {
		linked <- s.Link("a", "b")
	}()

	<-held.Reached()

	parked, acting := s.Activity()
	if acting {
		t.Fatalf("Activity with a Link parked at a hold: acting = true, want false")
	}

	if parked == idle {
		t.Fatalf("Activity after a Link started: moves = %d, unchanged, want a move", parked)
	}

	held.Release()

	if err := <-linked; err != nil {
		t.Fatal(err)
	}

	if ended, acting := s.Activity(); ended == parked || acting {
		t.Fatalf(
			"Activity after the held Link: moves %d (was %d), acting %v; want a move, not acting",
			ended,
			parked,
			acting,
		)
	}
}
