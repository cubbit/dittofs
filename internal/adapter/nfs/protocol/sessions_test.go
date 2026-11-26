package protocol

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// fakeClock is the pending table's clock as a test input: the test advances
// now instead of time passing.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

// propose holds p to s under name, refusing nothing: a test's own shorthand.
func propose(t *testing.T, s *Sessions, name string, p Pending) {
	t.Helper()

	if err := s.Propose([]byte(name), p); err != nil {
		t.Fatalf("Propose of %s refused: %v", name, err)
	}
}

func TestPendingExpires(t *testing.T) {
	clk := &fakeClock{}
	b := &resources.Budgets{Connection: resources.NewBudget("t8.3-expire", pendingCost*4)}
	s := NewSessions(b, clk, bytes.NewReader(nil))
	before := b.Connection.Reserved()
	name := []byte("client-a")
	p := Pending{
		Proposal: vfs.Proposal{Client: 7, Confirm: [8]byte{7}},
		Minor:    1,
		Confirm:  [8]byte{9},
		Info:     vfs.ClientInfo{Protocol: vfs.ProtocolNFSv4, Name: name, Boot: [8]byte{1}},
	}
	propose(t, s, "client-a", p)
	clk.advance(89 * time.Second)

	if _, ok := s.Confirm(7); !ok {
		t.Fatalf(
			"one lease less a second: the pending proposal is held, " +
				"Confirm answered false, want true",
		)
	}

	clk.advance(2 * time.Second)

	if _, ok := s.Confirm(7); ok {
		t.Fatalf("past one lease the pending proposal expired, Confirm answered true, want false")
	}

	if got, want := b.Connection.Reserved(), pendingCost; got != want {
		t.Fatalf(
			"the expired entry's charge is kept until the sweep: Reserved() = %d, want %d",
			got,
			want,
		)
	}

	s.Sweep()

	if got := b.Connection.Reserved(); got != before {
		t.Fatalf(
			"the sweep released the expired entry's charge: Reserved() = %d, want %d",
			got,
			before,
		)
	}
}

func TestPendingReplaceAndBounds(t *testing.T) {
	clk := &fakeClock{}
	b := &resources.Budgets{Connection: resources.NewBudget("t8.3-replace", pendingCost)}
	s := NewSessions(b, clk, bytes.NewReader(nil))
	name := []byte("client-b")
	propose(t, s, "client-b", Pending{
		Proposal: vfs.Proposal{Client: 1, Confirm: [8]byte{1}},
		Minor:    1,
		Confirm:  [8]byte{11},
		Info:     vfs.ClientInfo{Protocol: vfs.ProtocolNFSv4, Name: name, Boot: [8]byte{1}},
	})
	propose(t, s, "client-b", Pending{
		Proposal: vfs.Proposal{Client: 2, Confirm: [8]byte{2}},
		Minor:    1,
		Confirm:  [8]byte{22},
		Info:     vfs.ClientInfo{Protocol: vfs.ProtocolNFSv4, Name: name, Boot: [8]byte{2}},
	})

	if _, ok := s.Confirm(1); ok {
		t.Fatalf(
			"a name holds one proposal: the later replaced the first, " +
				"Confirm(1) answered true, want false",
		)
	}

	held, ok := s.Confirm(2)
	if !ok {
		t.Fatalf("the name's later proposal is held, Confirm(2) answered false, want true")
	}

	if held.Confirm != [8]byte{22} {
		t.Fatalf(
			"Confirm answered another entry's confirm: got %x, want %x",
			held.Confirm,
			[8]byte{22},
		)
	}

	if got, want := b.Connection.Reserved(), pendingCost; got != want {
		t.Fatalf("the replaced proposal's charge was released: Reserved() = %d, want %d", got, want)
	}

	// The pool is now full, one entry's cost held by the proposal above.
	err := s.Propose(
		[]byte("client-c"),
		Pending{Proposal: vfs.Proposal{Client: 3, Confirm: [8]byte{3}}, Minor: 1},
	)

	var delay *vfs.Delay
	if !errors.As(err, &delay) || delay.Cause != vfs.DelayResources {
		t.Fatalf(
			"a full connection-state pool: Propose answered %v, want ErrDelay of DelayResources",
			err,
		)
	}

	if _, ok := s.Confirm(3); ok {
		t.Fatalf("the refused proposal left no entry: Confirm(3) answered true, want false")
	}

	if _, ok := s.Confirm(2); !ok {
		t.Fatalf(
			"the full pool left the held proposal unchanged: Confirm(2) answered false, want true",
		)
	}
}

// Drop forgets a proposal and returns its charge, whether the entry was held
// at once or took over a name's earlier one: the confirm's writer calls it
// once its write has succeeded, and a charge or a name index it left would
// grow the table for ever.
func TestPendingDropReleasesItsCharge(t *testing.T) {
	clk := &fakeClock{}
	b := &resources.Budgets{Connection: resources.NewBudget("t8.3-drop", 2*pendingCost)}
	s := NewSessions(b, clk, bytes.NewReader(nil))
	name := []byte("client-d")
	pending := func(id vfs.ClientID) Pending {
		return Pending{
			Proposal: vfs.Proposal{Client: id},
			Minor:    1,
			Info:     vfs.ClientInfo{Protocol: vfs.ProtocolNFSv4, Name: name},
		}
	}
	empty := func(what string) {
		t.Helper()

		if got := b.Connection.Reserved(); got != 0 {
			t.Errorf("%s left Reserved() = %d, want 0", what, got)
		}

		if got := len(s.table); got != 0 {
			t.Errorf("%s left %d entries in the table, want 0", what, got)
		}

		if got := len(s.byName); got != 0 {
			t.Errorf("%s left %d names in the name index, want 0", what, got)
		}
	}

	propose(t, s, "client-d", pending(1))
	s.Drop(1)

	if _, ok := s.Confirm(1); ok {
		t.Errorf("Confirm(1) answered true after Drop, want false")
	}

	empty("Drop")

	propose(t, s, "client-d", pending(2))
	propose(t, s, "client-d", pending(3))
	s.Drop(3)
	empty("Drop of a replacing proposal")
}

// Drop sweeps first, as Propose does: an entry past its lease goes when any
// other proposal is dropped, so an idle node's expired entries do not wait
// for the next Propose.
func TestPendingDropSweepsExpired(t *testing.T) {
	clk := &fakeClock{}
	b := &resources.Budgets{Connection: resources.NewBudget("t8.3-dropsweep", 2*pendingCost)}
	s := NewSessions(b, clk, bytes.NewReader(nil))
	propose(t, s, "old", Pending{Proposal: vfs.Proposal{Client: 1}, Minor: 1})
	clk.advance(leasePeriod + time.Second)
	s.Drop(2)

	if got := b.Connection.Reserved(); got != 0 {
		t.Errorf("Drop left an expired entry's charge: Reserved() = %d, want 0", got)
	}
}

// A proposal whose name and client ID each name a different held entry
// leaves one entry, charged once: the client ID's stale entry returns its
// charge.
func TestPendingNameAndIDOnTwoEntries(t *testing.T) {
	clk := &fakeClock{}
	b := &resources.Budgets{Connection: resources.NewBudget("t8.3-two", 2*pendingCost)}
	s := NewSessions(b, clk, bytes.NewReader(nil))
	propose(t, s, "a", Pending{Proposal: vfs.Proposal{Client: 1}, Minor: 1})
	propose(t, s, "b", Pending{Proposal: vfs.Proposal{Client: 2}, Minor: 1})
	propose(t, s, "a", Pending{Proposal: vfs.Proposal{Client: 2}, Minor: 1})

	if got := b.Connection.Reserved(); got != pendingCost {
		t.Errorf("two entries for one proposal: Reserved() = %d, want %d", got, pendingCost)
	}

	if got := len(s.table); got != 1 {
		t.Errorf("table holds %d entries, want 1", got)
	}

	if got := len(s.byName); got != 1 {
		t.Errorf("name index holds %d names, want 1", got)
	}
}
