package protocol

import (
	"io"
	"sync"
	"time"

	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// leasePeriod is one client lease, open state's own: a pending proposal is
// held for one, and dropped by the sweep past it.
const leasePeriod = 90 * time.Second

// pendingCost is the fixed charge of one pending entry to the
// connection-state budget: the client owner's name at its wire cap, 1 KiB,
// with the entry's own bytes inside it.
const pendingCost int64 = 1 << 10

// Pending is one proposal waiting for its confirm: the client ID and confirm
// the service's Propose answered, the minor version that proposed, and the
// ClientInfo the confirm's Connect registers, which carries the adapter's
// own bytes.
type Pending struct {
	Proposal vfs.Proposal // handed to Connect only
	Minor    uint32
	Confirm  [8]byte // the adapter's own
	Info     vfs.ClientInfo
}

// pendingEntry is one held proposal with its lease and pool charge.
type pendingEntry struct {
	p       Pending
	name    string
	created time.Time
	charge  resources.Reservation
}

// Sessions is the node's table of clients pending confirmation, keyed by the
// client ID the proposal answered, with one entry per owner name, reading
// Clock for each entry's lease and drawing confirms from its random source.
//
// decision: the table is volatile memory of the node that answered, and the
// confirm names only the client ID, so the entry is held here with the
// service's proposal handed whole to Connect, which drops the service's own
// (RFC 14 section 2.9): a proposal no confirm reaches dies with its lease
// here, and the client proposes again. Overturned by RFC 14 section 2.9's
// own trigger, a proposal that outlives the node that answered it.
type Sessions struct {
	mu     sync.Mutex
	table  map[vfs.ClientID]*pendingEntry
	byName map[string]vfs.ClientID
	budget *resources.Budget
	clk    Clock
	rnd    io.Reader
}

// NewSessions builds an empty pending table, charging every entry to b's
// connection-state budget and drawing its confirms from rnd.
func NewSessions(b *resources.Budgets, clk Clock, rnd io.Reader) *Sessions {
	return &Sessions{
		table:  make(map[vfs.ClientID]*pendingEntry),
		byName: make(map[string]vfs.ClientID),
		budget: b.Connection,
		clk:    clk,
		rnd:    rnd,
	}
}

// NewConfirm draws the adapter's own confirm: eight bytes the confirming
// CREATE_SESSION is checked against.
//
// decision: the confirm is drawn here and not taken from the service's
// proposal, because open state stores the adapter bytes Propose is handed and
// draws its own Confirm only after it has them, so nothing derived from that
// Confirm can reach the record the confirm writes. Overturned if RFC 14's
// Connect comes to write the adapter bytes it is handed.
func (s *Sessions) NewConfirm() (c [8]byte, err error) {
	s.mu.Lock()

	defer s.mu.Unlock()

	_, err = io.ReadFull(s.rnd, c[:])

	return c, err
}

// Propose holds p under its owner name, replacing the name's earlier proposal.
// A full connection-state budget is ErrDelay of DelayResources, the refusal
// the request resends against.
func (s *Sessions) Propose(name []byte, p Pending) error {
	s.mu.Lock()

	defer s.mu.Unlock()

	s.sweep()

	cl := p.Proposal.Client

	old := s.entry(string(name), cl)
	if old == nil {
		res, err := s.budget.Reserve(pendingCost)
		if err != nil {
			return &vfs.Delay{Cause: vfs.DelayResources}
		}

		s.hold(cl, string(name), p, res)

		return nil
	}

	// Every entry costs the same, so the replaced one keeps its charge.
	delete(s.table, old.p.Proposal.Client)
	delete(s.byName, old.name)
	s.hold(cl, string(name), p, old.charge)

	return nil
}

// entry is the proposal the name or the client ID already holds, releasing
// any second entry the other key held, since one proposal answers both.
func (s *Sessions) entry(name string, cl vfs.ClientID) *pendingEntry {
	id, hasName := s.byName[name]
	byID, hasID := s.table[cl]

	switch {
	case hasName && hasID && s.table[id] != byID:
		// Two entries for one proposal: the id's is the stale one.
		delete(s.byName, byID.name)
		delete(s.table, cl)
		byID.charge.Release()

		return s.table[id]
	case hasName:
		return s.table[id]
	case hasID:
		return byID
	}

	return nil
}

// hold stores p under cl and name with the given charge.
func (s *Sessions) hold(cl vfs.ClientID, name string, p Pending, res resources.Reservation) {
	s.table[cl] = &pendingEntry{p: p, name: name, created: s.clk.Now(), charge: res}
	s.byName[name] = cl
}

// Confirm answers the proposal the confirm's client ID names, false when none
// is held or its lease has passed. It releases no charge itself: a confirm
// answered NFS4ERR_DELAY finds the proposal again on its resend, and the
// sweep alone drops what its lease has passed.
func (s *Sessions) Confirm(cl vfs.ClientID) (Pending, bool) {
	s.mu.Lock()

	defer s.mu.Unlock()

	e, ok := s.table[cl]
	if !ok || !s.clk.Now().Before(e.created.Add(leasePeriod)) {
		return Pending{}, false
	}

	return e.p, true
}

// Drop forgets the proposal the confirm's client ID names and releases its
// charge: only the confirm's writer calls it, once its write has succeeded.
func (s *Sessions) Drop(cl vfs.ClientID) {
	s.mu.Lock()

	defer s.mu.Unlock()

	s.sweep()

	e, ok := s.table[cl]
	if !ok {
		return
	}

	delete(s.byName, e.name)
	delete(s.table, cl)
	e.charge.Release()
}

// Sweep drops every proposal past its lease and releases its charges.
// Propose and Drop sweep first, so an idle node holds its expired entries
// until its next call of either.
func (s *Sessions) Sweep() {
	s.mu.Lock()

	defer s.mu.Unlock()

	s.sweep()
}

// sweep drops every entry past its lease, releasing its charge. Propose and
// Drop run it before they change the table; Confirm never does, since the
// sweep alone may drop an entry a delayed confirm comes back for.
func (s *Sessions) sweep() {
	now := s.clk.Now()
	for cl, e := range s.table {
		if now.Before(e.created.Add(leasePeriod)) {
			continue
		}

		delete(s.byName, e.name)
		delete(s.table, cl)
		e.charge.Release()
	}
}
