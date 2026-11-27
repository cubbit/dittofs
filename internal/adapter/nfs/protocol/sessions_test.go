package protocol

import (
	"bytes"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
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

// ramp is a source of random bytes the test can foresee: 1, 2, 3 and on,
// wrapping past 255.
type ramp struct {
	next byte
}

func (r *ramp) Read(p []byte) (int, error) {
	for i := range p {
		r.next++
		p[i] = r.next
	}

	return len(p), nil
}

// The costs the tests count by, written out as RFC 24 section 4.5 gives them
// so no test reads them back from the code: the session's, a slot's, and
// one slot of a channel caching 8 KiB.
const (
	wantSessionCost = 4 << 10
	wantSlotCost    = 256
	wantSlot8K      = wantSlotCost + 8<<10
	maxSize         = 1<<20 + 4<<10
)

// registry is a registry over a budget of ceiling bytes and a ramp.
func registry(t *testing.T, ceiling int64) (*Sessions, *resources.Budget) {
	t.Helper()

	b := &resources.Budgets{Connection: resources.NewBudget("t8.4-"+t.Name(), ceiling)}

	return NewSessions(b, &fakeClock{}, &ramp{}), b.Connection
}

// ask is a channel asked for with the values of the table.
func ask(slots, size, cached, ops uint32) Channel {
	return Channel{
		Slots:       slots,
		MaxRequest:  size,
		MaxResponse: size,
		MaxCached:   cached,
		MaxOps:      ops,
	}
}

func sysCallback() *Callback {
	return &Callback{Minor: 1, Program: 0x40000000, Flavour: 1, Cred: []byte{1}}
}

func mustCreate(t *testing.T, s *Sessions, cl vfs.ClientID, sp Spec) *Session {
	t.Helper()

	sess, err := s.Create(cl, sp)
	if err != nil {
		t.Fatalf("Create for client %d refused: %v", cl, err)
	}

	return sess
}

// Decisions on one client exclude each other: many callers creating sessions
// for one client at once leave every session in the registry and on the
// client's list, none lost to a decision that ran beside another.
func TestCreateExcludesDecisionsOnOneClient(t *testing.T) {
	const (
		callers = 32
		each    = 8
	)

	// ramp wraps after sixteen IDs, so the IDs are drawn from the system.
	b := &resources.Budgets{Connection: resources.NewBudget("t8.4-exclude", 64<<20)}
	s := NewSessions(b, &fakeClock{}, rand.Reader)

	var wg sync.WaitGroup

	for range callers {
		wg.Go(func() {
			for range each {
				if _, err := s.Create(7, Spec{Fore: ask(1, 4<<10, 0, 1)}); err != nil {
					t.Errorf("Create refused: %v", err)
				}
			}
		})
	}

	// A second client is created and ended at the same time: the ends must
	// exclude the creates of the registry's one lock too.
	for range callers {
		wg.Go(func() {
			for range each {
				if _, err := s.Create(8, Spec{Fore: ask(1, 4<<10, 0, 1)}); err != nil {
					t.Errorf("Create refused: %v", err)
				}

				s.EndClient(8)
			}
		})
	}

	wg.Wait()

	s.EndClient(8)

	s.mu.Lock()

	defer s.mu.Unlock()

	if got := len(s.byClient[8]); got != 0 {
		t.Errorf("the ended client lists %d sessions, want 0", got)
	}

	if got := len(s.byClient[7]); got != callers*each {
		t.Errorf("the client lists %d sessions, want %d", got, callers*each)
	}

	if got := len(s.sessions); got != callers*each {
		t.Errorf("the registry holds %d sessions, want %d", got, callers*each)
	}
}

// A session asking far past every cap is granted the caps: 64 fore slots, 16
// back slots, 8 KiB cached, 128 operations and 1 MiB and 4 KiB for each size,
// and charged the session's cost with every slot and its cache (RFC 21
// section 3.1, RFC 24 section 4.5).
func TestCreateGrantsTheCaps(t *testing.T) {
	s, pool := registry(t, 8<<20)

	sess := mustCreate(t, s, 1, Spec{
		Fore:     ask(128, 1<<30, 64<<10, 1000),
		Back:     ask(128, 1<<30, 64<<10, 1000),
		Callback: sysCallback(),
	})

	want := Channel{
		Slots:       64,
		MaxRequest:  maxSize,
		MaxResponse: maxSize,
		MaxCached:   8 << 10,
		MaxOps:      128,
	}
	if sess.Fore != want {
		t.Errorf("Fore = %+v, want %+v", sess.Fore, want)
	}

	want.Slots = 16
	if sess.BackAttrs != want {
		t.Errorf("BackAttrs = %+v, want %+v", sess.BackAttrs, want)
	}

	if sess.Back == nil {
		t.Fatal("Back = nil, want the back channel")
	}

	charged := int64(wantSessionCost + (64+16)*wantSlot8K)
	if got := pool.Reserved(); got != charged {
		t.Errorf(
			"Reserved() = %d, want %d: the session, 80 slots and their 8 KiB caches",
			got,
			charged,
		)
	}
}

// A session asking for less is granted what it asked for, and a channel asking
// for no slot is granted one.
func TestCreateGrantsTheLesser(t *testing.T) {
	s, _ := registry(t, 8<<20)

	sess := mustCreate(t, s, 1, Spec{Fore: ask(1, 4096, 100, 1)})

	if want := ask(1, 4096, 100, 1); sess.Fore != want {
		t.Errorf("Fore = %+v, want what was asked, %+v", sess.Fore, want)
	}

	none := mustCreate(t, s, 2, Spec{Fore: ask(0, 4096, 0, 1)})
	if none.Fore.Slots != 1 {
		t.Errorf("a fore channel asking 0 slots is granted %d, want 1", none.Fore.Slots)
	}
}

// A pool with room for the session and three slots grants three slots out of
// the 64 asked, and every byte of the three is charged; with a back channel
// asked too, the back channel gives way first.
func TestCreateGrantsWhatThePoolCharges(t *testing.T) {
	s, pool := registry(t, wantSessionCost+3*wantSlot8K)

	sess := mustCreate(t, s, 1, Spec{Fore: ask(64, 4096, 8<<10, 8)})
	if sess.Fore.Slots != 3 {
		t.Errorf("Fore.Slots = %d, want 3, all the pool holds", sess.Fore.Slots)
	}

	if got, want := pool.Reserved(), int64(wantSessionCost+3*wantSlot8K); got != want {
		t.Errorf("Reserved() = %d, want %d", got, want)
	}

	s, _ = registry(t, wantSessionCost+5*wantSlot8K)

	both := mustCreate(t, s, 1, Spec{
		Fore:     ask(4, 4096, 8<<10, 8),
		Back:     ask(4, 4096, 8<<10, 8),
		Callback: sysCallback(),
	})
	if both.Fore.Slots != 4 || both.Back == nil || both.BackAttrs.Slots != 1 {
		t.Errorf("fore %d slots, back %v with %d, want 4 and a back channel of 1",
			both.Fore.Slots, both.Back, both.BackAttrs.Slots)
	}
}

// A pool that cannot charge the session and one slot refuses with a delay of
// resources and keeps nothing; a pool that holds the session and one slot
// leaves the back channel out and grants the fore one slot.
func TestCreateRefusesWhenNoSlotFits(t *testing.T) {
	s, pool := registry(t, wantSessionCost+wantSlot8K-1)

	_, err := s.Create(1, Spec{Fore: ask(8, 4096, 8<<10, 8)})

	var d *vfs.Delay
	if !errors.As(err, &d) || d.Cause != vfs.DelayResources {
		t.Fatalf("Create = %v, want a delay of resources", err)
	}

	if got := pool.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d after a refusal, want 0", got)
	}

	if s.Holds(1) {
		t.Error("Holds(1) after a refusal, want false")
	}

	s, _ = registry(t, wantSessionCost+wantSlot8K)

	sess := mustCreate(t, s, 1, Spec{
		Fore:     ask(8, 4096, 8<<10, 8),
		Back:     ask(8, 4096, 8<<10, 8),
		Callback: sysCallback(),
	})
	if sess.Fore.Slots != 1 || sess.Back != nil {
		t.Errorf(
			"fore %d slots, back %v, want 1 slot and no back channel",
			sess.Fore.Slots,
			sess.Back,
		)
	}
}

// Create sweeps first, as Propose and Drop do: the charge of a proposal past
// its lease does not make a CREATE_SESSION wait for the next EXCHANGE_ID. A
// proposal inside its lease keeps its charge, and the same call is a delay.
func TestCreateSweepsExpiredProposals(t *testing.T) {
	clk := &fakeClock{}
	room := pendingCost + wantSessionCost + wantSlot8K - 1
	b := &resources.Budgets{Connection: resources.NewBudget("t8.4-createsweep", room)}
	s := NewSessions(b, clk, &ramp{})
	propose(t, s, "old", Pending{Proposal: vfs.Proposal{Client: 1}, Minor: 1})

	spec := Spec{Fore: ask(1, 4096, 8<<10, 8)}

	_, err := s.Create(2, spec)

	var d *vfs.Delay
	if !errors.As(err, &d) || d.Cause != vfs.DelayResources {
		t.Fatalf("Create inside the proposal's lease = %v, want a delay of resources", err)
	}

	clk.advance(leasePeriod + time.Second)

	if _, err := s.Create(2, spec); err != nil {
		t.Fatalf("Create past the proposal's lease = %v, want the session built", err)
	}

	if _, ok := s.Confirm(1); ok {
		t.Error("the expired proposal is still held after Create")
	}
}

// End returns the session's whole charge and says whether it was the client's
// last on the node; a second End of the same session changes nothing.
func TestEndReturnsTheChargeAndSaysLast(t *testing.T) {
	s, pool := registry(t, 8<<20)
	a := mustCreate(t, s, 1, Spec{Fore: ask(2, 4096, 8<<10, 8)})
	b := mustCreate(t, s, 1, Spec{Fore: ask(2, 4096, 8<<10, 8)})
	other := mustCreate(t, s, 2, Spec{Fore: ask(2, 4096, 8<<10, 8)})

	if s.End(a.ID) {
		t.Error("End of one of two sessions of a client said last")
	}

	if _, ok := s.Get(a.ID); ok {
		t.Error("Get finds a session after its End")
	}

	if !s.End(b.ID) {
		t.Error("End of the client's last session did not say last")
	}

	if s.Holds(1) || !s.Holds(2) {
		t.Errorf("Holds(1) = %v, Holds(2) = %v, want false and true", s.Holds(1), s.Holds(2))
	}

	if s.End(b.ID) {
		t.Error("a second End said last")
	}

	if !s.End(other.ID) {
		t.Error("End of another client's only session did not say last")
	}

	if got := pool.Reserved(); got != 0 {
		t.Errorf("Reserved() = %d after every End, want 0", got)
	}
}

// EndClient ends every session of the client, returns every charge, and takes
// each ID off the lists of the connections bound, and touches no other
// client's session.
func TestEndClientEndsEverySessionOfTheClient(t *testing.T) {
	s, pool := registry(t, 8<<20)
	a := mustCreate(t, s, 1, Spec{Fore: ask(2, 4096, 8<<10, 8)})
	b := mustCreate(t, s, 1, Spec{Fore: ask(2, 4096, 8<<10, 8)})
	c := mustCreate(t, s, 1, Spec{Fore: ask(2, 4096, 8<<10, 8)})
	other := mustCreate(t, s, 2, Spec{Fore: ask(2, 4096, 8<<10, 8)})

	conn := new(Bound)
	for _, x := range []*Session{a, b, c, other} {
		if !s.Bind(x, conn) {
			t.Fatalf("Bind of session %x refused", x.ID)
		}
	}

	s.EndClient(1)

	if s.Holds(1) {
		t.Error("Holds(1) after EndClient(1)")
	}

	for _, x := range []*Session{a, b, c} {
		if _, ok := s.Get(x.ID); ok {
			t.Errorf("session %x of client 1 is held after EndClient(1), want it ended", x.ID)
		}
	}

	if _, ok := s.Get(other.ID); !ok {
		t.Error("EndClient(1) ended client 2's session")
	}

	if got := conn.IDs(); len(got) != 1 || got[0] != other.ID {
		t.Errorf("the connection's list is %x, want only %x", got, other.ID)
	}

	if got, want := pool.Reserved(), int64(wantSessionCost+2*wantSlot8K); got != want {
		t.Errorf("Reserved() = %d, want %d: client 2's session alone", got, want)
	}
}

// Every session has an ID of its own, drawn from the registry's random
// source, and a reference numbered from 1 with no node.
func TestSessionIDsAreDrawnAndUnique(t *testing.T) {
	s, _ := registry(t, 8<<20)
	a := mustCreate(t, s, 1, Spec{Fore: ask(1, 4096, 0, 1)})
	b := mustCreate(t, s, 1, Spec{Fore: ask(1, 4096, 0, 1)})

	wantA := SessionID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if a.ID != wantA || a.ID == b.ID {
		t.Errorf(
			"IDs %x and %x, want the first 16 bytes of the source and a different second",
			a.ID,
			b.ID,
		)
	}

	if a.Ref != (vfs.SessionRef{ID: 1}) || b.Ref != (vfs.SessionRef{ID: 2}) {
		t.Errorf("Refs %+v and %+v, want IDs 1 and 2 with no node", a.Ref, b.Ref)
	}

	if got, ok := s.Get(b.ID); !ok || got != b {
		t.Error("Get does not find the session by its ID")
	}

	same := NewSessions(
		&resources.Budgets{Connection: resources.NewBudget("t8.4-same", 8<<20)},
		&fakeClock{},
		bytes.NewReader(bytes.Repeat([]byte{7}, 32)),
	)
	mustCreate(t, same, 1, Spec{Fore: ask(1, 4096, 0, 1)})

	if _, err := same.Create(1, Spec{Fore: ask(1, 4096, 0, 1)}); err == nil {
		t.Error("a second session drawn the same ID was granted, want an error")
	}
}

// A session keeps the client's protection and bitmaps it was created with.
func TestSessionKeepsTheRecordsProtection(t *testing.T) {
	s, _ := registry(t, 8<<20)
	sess := mustCreate(t, s, 1, Spec{
		Fore:        ask(1, 4096, 0, 1),
		Protect:     true,
		MustEnforce: []uint32{0x10},
		MustAllow:   []uint32{0x20, 0x40},
	})

	if !sess.Protect || len(sess.MustEnforce) != 1 || len(sess.MustAllow) != 2 {
		t.Errorf("Protect %v, enforce %x, allow %x, want true, [10], [20 40]",
			sess.Protect, sess.MustEnforce, sess.MustAllow)
	}
}

// A connection's list takes the session's ID when it binds and not once it is
// closed; a bind to an ended session binds nothing.
func TestBindHonoursTheListsClose(t *testing.T) {
	s, _ := registry(t, 8<<20)
	sess := mustCreate(t, s, 1, Spec{Fore: ask(1, 4096, 0, 1)})

	open := new(Bound)
	if !s.Bind(sess, open) || len(open.IDs()) != 1 {
		t.Fatalf("an open list: Bind refused or IDs %x, want the session's ID", open.IDs())
	}

	closed := new(Bound)
	closed.Close()

	if s.Bind(sess, closed) || len(closed.IDs()) != 0 {
		t.Errorf("a closed list: Bind accepted or IDs %x, want nothing bound", closed.IDs())
	}

	s.End(sess.ID)

	if s.Bind(sess, new(Bound)) {
		t.Error("Bind to an ended session was accepted")
	}

	if got := open.IDs(); len(got) != 0 {
		t.Errorf("the open list holds %x after the session's End, want none", got)
	}
}

// calledOn is the connection a back channel calls on: the one bound last, nil
// with none.
func calledOn(b *BackChannel) *core.Conn {
	if len(b.conns) == 0 {
		return nil
	}

	return b.conns[len(b.conns)-1]
}

// A back channel calls on the connection bound last, and says it is the last
// when no connection is left.
func TestBackChannelBindUnbind(t *testing.T) {
	b := NewBackChannel(SessionID{1}, 1, 0x40000000, 1, []byte{1}, 4)
	one, two := &core.Conn{}, &core.Conn{}
	b.Bind(one)
	b.Bind(two)

	if got := calledOn(b); got != two {
		t.Errorf("the connection called on is %p, want the one bound last, %p", got, two)
	}

	if b.Unbind(two) {
		t.Error("Unbind of one of two connections said last")
	}

	if got := calledOn(b); got != one {
		t.Errorf("after the Unbind the connection called on is %p, want %p", got, one)
	}

	if !b.Unbind(one) {
		t.Error("Unbind of the last connection did not say last")
	}
}
