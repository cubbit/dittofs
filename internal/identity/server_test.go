package identity_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cubbit/dittofs/internal/identity"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/sim"
)

// newStore opens an initialised in-memory metadata store, closed at the
// test's end before the leak check runs, since cleanups run last-in first-out
// and the check is registered first.
func newStore(t *testing.T) *kvstore.Store {
	t.Helper()
	ctx := context.Background()
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		t.Fatal(err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// newServer builds a server over a fresh store, under the leak check.
func newServer(t *testing.T) *identity.Server {
	t.Helper()
	prof.Watch(t, prof.Options{})
	s, err := identity.New(identity.Deps{Store: newStore(t), Clock: sim.NewTestClock(time.Unix(0, 0))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// state is a State of generation gen under the default settings.
func state(gen uint64) identity.State {
	return identity.State{Generation: gen, Settings: identity.DefaultSettings()}
}

func TestNewRefusesMissingDeps(t *testing.T) {
	clock := sim.NewTestClock(time.Unix(0, 0))
	for _, tc := range []struct {
		name string
		deps identity.Deps
	}{
		{"nil store", identity.Deps{Clock: clock}},
		{"typed nil store", identity.Deps{Store: (*kvstore.Store)(nil), Clock: clock}},
		{"nil clock", identity.Deps{Store: newStore(t)}},
	} {
		if s, err := identity.New(tc.deps); err == nil {
			t.Errorf("New with a %s = %v, nil; want an error", tc.name, s)
		}
	}
}

// New starts nothing, so a server built and stopped, never started, leaves
// nothing running.
func TestNewStartsNothing(t *testing.T) {
	s := newServer(t)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// The first Apply comes before Start: a Start with none is refused, and one
// after it succeeds.
func TestStartNeedsAnApply(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	if err := s.Start(ctx); !errors.Is(err, identity.ErrNotApplied) {
		t.Fatalf("Start before Apply = %v, want ErrNotApplied", err)
	}
	if err := s.Apply(state(1)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start after Apply = %v, want nil", err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// Stop drops the applied State, and leaves no goroutine Start started.
func TestStopDropsItsState(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	if err := s.Apply(state(1)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := s.Applied(); got != nil {
		t.Errorf("State after Stop = %+v, want none", *got)
	}
}

// Apply swaps a State in whole: concurrent Applies of matched Generation and
// Settings pairs, each naming its Generation in NetBIOSName, and a reader that
// sees only matched pairs. A State written field by field shows the reader one
// Apply's Generation beside another's Settings, with or without -race.
func TestApplySwapsTheStateWhole(t *testing.T) {
	s := newServer(t)
	pair := func(g uint64) identity.State {
		st := state(g)
		st.Settings.NetBIOSName = fmt.Sprintf("G%d", g)
		return st
	}
	if err := s.Apply(pair(1)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	done := make(chan struct{})
	var appliers, reader sync.WaitGroup
	for w := range uint64(4) {
		appliers.Go(func() {
			for i := range uint64(2000) {
				if err := s.Apply(pair(w*10000 + i)); err != nil {
					t.Errorf("Apply: %v", err)
				}
			}
		})
	}
	reader.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			st := s.Applied()
			if st == nil {
				t.Error("no State applied while Applies run")
				return
			}
			// Copied once, so the message prints the pair that was compared.
			gen, name := st.Generation, st.Settings.NetBIOSName
			if want := fmt.Sprintf("G%d", gen); name != want {
				t.Errorf("read State with Generation %d and NetBIOSName %q, want %q: two Applies mixed", gen, name, want)
				return
			}
		}
	})
	appliers.Wait()
	close(done)
	reader.Wait()
}
