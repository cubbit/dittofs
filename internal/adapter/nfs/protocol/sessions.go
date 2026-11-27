package protocol

import (
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
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
// own bytes. An NFSv4.1 confirm is checked through the record's stored sequence
// ID, which Confirm's low 32 bits made, so nothing reads Confirm there; the
// NFSv4.0 confirm, once it is served, compares its verifier with it.
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
// Clock for each entry's lease and drawing confirms from its random source,
// and the node's sessions, by ID and by client.
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

	sessions map[SessionID]*Session
	byClient map[vfs.ClientID][]*Session
	refs     atomic.Uint64
}

// NewSessions builds an empty pending table, charging every entry to b's
// connection-state budget and drawing its confirms from rnd.
func NewSessions(b *resources.Budgets, clk Clock, rnd io.Reader) *Sessions {
	return &Sessions{
		table:    make(map[vfs.ClientID]*pendingEntry),
		byName:   make(map[string]vfs.ClientID),
		budget:   b.Connection,
		clk:      clk,
		rnd:      rnd,
		sessions: make(map[SessionID]*Session),
		byClient: make(map[vfs.ClientID][]*Session),
	}
}

// NewConfirm draws the adapter's own confirm: eight bytes. For NFSv4.1 only
// their low 32 bits matter, the sequence ID the confirming CREATE_SESSION must
// carry; the NFSv4.0 confirm, once it is served, compares all eight.
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
// Propose, Drop and Create sweep first, so an idle node holds its expired
// entries until its next call of any of them.
func (s *Sessions) Sweep() {
	s.mu.Lock()

	defer s.mu.Unlock()

	s.sweep()
}

// sweep drops every entry past its lease, releasing its charge. Propose and
// Drop run it before they change the table, and Create before it charges the
// pool; Confirm never does, since the sweep alone may drop an entry a delayed
// confirm comes back for.
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

// The caps a session is granted (RFC 21 section 3.1 and section 11.5), and
// what the connection-state pool charges for it.
//
// ponytail: 8 KiB per cached reply covers every non-idempotent operation's
// reply with its attributes, and bounds a session at 512 KiB however many
// clients ask for more. A client that wants a cached READ reply is told it is
// too big and asks again uncached. Raise the cap when a supported client is
// shown asking to cache replies larger than 8 KiB for operations that are not
// idempotent.
//
// ponytail: 64 slots caps one client at 64 requests in flight per session. A
// client that needs more opens a second session or uses several nodes at once
// (RFC 21 section 3.3). Raise the cap, or size it from a memory budget, when
// a single-client benchmark is limited by slots.
const (
	maxForeSlots = 64
	maxBackSlots = 16
	maxCached    = 8 << 10
	maxOps       = 128 // the compound cap of RFC 21 section 11.5
)

// The two fixed costs of RFC 24 section 4.5, proposed at 4 KiB for a session
// and 256 bytes for a slot and settled by its measurement M2. A slot's cache
// is charged beside its cost, whatever the client sends.
const (
	sessionCost int64 = 4 << 10
	slotCost    int64 = 256
)

// SessionID is a session's ID on the wire: 16 bytes drawn from the
// registry's random source.
type SessionID [16]byte

// Channel is one channel's attributes as granted.
type Channel struct {
	Slots, MaxRequest, MaxResponse, MaxCached, MaxOps uint32
}

// Callback is what a back channel is called with: the program and minor
// version the client named and the credential it asked to be called under,
// AUTH_NONE or AUTH_SYS.
type Callback struct {
	Minor, Program, Flavour uint32
	Cred                    []byte
}

// Spec is what CREATE_SESSION asks of Create: the channels as the client asked
// for them, the back channel's callback (nil: none can be called), and the
// client record's protection and bitmaps, which the session keeps.
type Spec struct {
	Fore, Back  Channel
	Callback    *Callback
	Protect     bool
	MustEnforce []uint32
	MustAllow   []uint32
}

// BackChannel is one session's back channel on this node: how the client is
// called and the connections it is bound to, in the order bound. The latest
// bound is the one called on.
type BackChannel struct {
	ID      SessionID
	Minor   uint32
	Program uint32
	Flavour uint32
	Cred    []byte
	Slots   uint32

	mu    sync.Mutex
	conns []*core.Conn
}

// NewBackChannel builds the back channel of session id, called as program prog
// of minor version minor under flavour and cred, with slots slots.
func NewBackChannel(
	id SessionID,
	minor, prog, flavour uint32,
	cred []byte,
	slots uint32,
) *BackChannel {
	return &BackChannel{
		ID:      id,
		Minor:   minor,
		Program: prog,
		Flavour: flavour,
		Cred:    cred,
		Slots:   slots,
	}
}

// Bind makes c the connection calls go on, moving it to the end when it was
// bound already.
func (b *BackChannel) Bind(c *core.Conn) {
	b.mu.Lock()

	defer b.mu.Unlock()

	b.conns = append(slices.DeleteFunc(b.conns, isConn(c)), c)
}

// Unbind forgets c and reports whether no connection is left.
func (b *BackChannel) Unbind(c *core.Conn) (last bool) {
	b.mu.Lock()

	defer b.mu.Unlock()

	b.conns = slices.DeleteFunc(b.conns, isConn(c))

	return len(b.conns) == 0
}

// Bound is one connection's list of the sessions it is bound to, in the order
// bound, under its own mutex: the one place a close and a bind meet (RFC 21
// section 3.4's list). The zero value is an open, empty list.
//
// decision: nothing calls Close, nor Unbind on the back channels, when a
// connection ends: no OnClose stage is set yet, so a closed connection stays on
// the sessions it was bound to until the client's lease ends and EndClient
// drops them. Wiring the stage needs a registry call that takes the connection
// off each session and back channel, which the code that serves a connection's
// bind after SEQUENCE adds. Overturned by that code.
type Bound struct {
	mu     sync.Mutex
	closed bool
	ids    []SessionID
}

// Close marks the list closed, so a bind after it binds nothing: the first
// step of the connection's close.
func (b *Bound) Close() {
	b.mu.Lock()

	defer b.mu.Unlock()

	b.closed = true
}

// IDs is the sessions the connection is bound to, in the order bound.
func (b *Bound) IDs() []SessionID {
	b.mu.Lock()

	defer b.mu.Unlock()

	return slices.Clone(b.ids)
}

// drop takes id off the list.
func (b *Bound) drop(id SessionID) {
	b.mu.Lock()

	defer b.mu.Unlock()

	b.ids = slices.DeleteFunc(b.ids, isID(id))
}

// Session is one session of a client on this node: its channels as granted,
// the client record's protection and bitmaps as they were at CREATE_SESSION,
// and the pool charge it holds until it ends.
type Session struct {
	ID        SessionID
	Ref       vfs.SessionRef
	Client    vfs.ClientID
	Fore      Channel
	BackAttrs Channel      // the back channel's attributes as granted, bound or not
	Back      *BackChannel // nil: none the client can be called on

	Protect     bool
	MustEnforce []uint32
	MustAllow   []uint32

	// Guarded by Sessions.mu.
	ended  bool
	conns  []*Bound
	charge resources.Reservation
}

// grant is ch as granted: the lesser of what was asked and the caps, at least
// one slot.
func grant(ch Channel, slots uint32) Channel {
	return Channel{
		Slots:       max(1, min(ch.Slots, slots)),
		MaxRequest:  min(ch.MaxRequest, wire.MaxRecord),
		MaxResponse: min(ch.MaxResponse, wire.MaxRecord),
		MaxCached:   min(ch.MaxCached, maxCached),
		MaxOps:      min(ch.MaxOps, maxOps),
	}
}

// cost is the pool charge of n slots of a channel caching cached bytes.
func cost(n, cached uint32) int64 {
	return int64(n) * (slotCost + int64(cached))
}

// Create registers a session of client cl asked for as sp says, granting the
// lesser of each value asked and its cap (RFC 21 section 3.1), and charges it
// to the connection-state pool: the session's cost, and each slot's with its
// cache. Where the pool cannot charge the slots asked, the back channel gives
// way first, down to none, then the fore channel down to one slot; where it
// cannot charge that, the error is a delay of resources and nothing is kept
// (RFC 24 section 4.5).
//
// decision: a back channel given way to none still answers the call OK, with
// CONN_BACK_CHAN clear and no callbacks registered, where a client that asked
// to be called may expect an error. RFC 21 section 3.1 has a session granted
// fewer slots where the node cannot charge them, and the client sees the flag.
// Overturned if a client is seen to rely on the callback it asked for.
//
// Create takes the whole spec, and draws the session's ID itself, since the
// back channel is named by the ID it is built under.
//
// ponytail: a client may hold any number of sessions: the pool is the only
// bound. Add a cap of 16 sessions per source address, with its error row,
// when a client is seen to exhaust the pool on its own (RFC 21 section 3.3).
//
// ponytail: one mutex guards every decision of every client, where the
// service's own step is per client. Split it per client ID when a profile of
// 64 sessions shows its contention.
func (s *Sessions) Create(cl vfs.ClientID, sp Spec) (*Session, error) {
	s.mu.Lock()

	defer s.mu.Unlock()

	s.sweep() // an expired proposal's charge is not a reason to delay

	fore, back := grant(sp.Fore, maxForeSlots), grant(sp.Back, maxBackSlots)

	backSlots := uint32(0)
	if sp.Callback != nil {
		backSlots = back.Slots
	}

	foreSlots := fore.Slots

	res, err := s.charge(&foreSlots, &backSlots, fore.MaxCached, back.MaxCached)
	if err != nil {
		return nil, err
	}

	fore.Slots = foreSlots
	if sp.Callback != nil {
		// The reply's attributes keep the one slot of grant, charged to
		// nothing here: a back channel the pool left no slot for is none, which
		// the unset CONN_BACK_CHAN flag and a nil Back say, and a client reads
		// no attributes of a channel it was not given.
		back.Slots = max(backSlots, 1) // a back channel with no slot is none
	}

	sess := &Session{
		Client:      cl,
		Fore:        fore,
		BackAttrs:   back,
		Protect:     sp.Protect,
		MustEnforce: slices.Clone(sp.MustEnforce),
		MustAllow:   slices.Clone(sp.MustAllow),
		charge:      res,
	}

	if _, err := io.ReadFull(s.rnd, sess.ID[:]); err != nil {
		res.Release()

		return nil, fmt.Errorf("draw a session id: %w", err)
	}

	if _, dup := s.sessions[sess.ID]; dup {
		res.Release()

		return nil, fmt.Errorf("session id %x drawn twice", sess.ID)
	}

	sess.Ref = vfs.SessionRef{ID: s.refs.Add(1)}

	if sp.Callback != nil && backSlots > 0 {
		c := sp.Callback
		sess.Back = NewBackChannel(sess.ID, c.Minor, c.Program, c.Flavour, c.Cred, backSlots)
	}

	s.sessions[sess.ID] = sess
	s.byClient[cl] = append(s.byClient[cl], sess)

	return sess, nil
}

// charge reserves the session's cost and its slots, lowering back then fore
// until the pool takes them: the one reservation of the session.
func (s *Sessions) charge(fore, back *uint32, foreCached, backCached uint32) (
	resources.Reservation,
	error,
) {
	for {
		n := sessionCost + cost(*fore, foreCached) + cost(*back, backCached)

		res, err := s.budget.Reserve(n)
		if err == nil {
			return res, nil
		}

		switch {
		case *back > 0:
			*back--
		case *fore > 1:
			*fore--
		default:
			return resources.Reservation{}, &vfs.Delay{Cause: vfs.DelayResources}
		}
	}
}

// Get is the session id names.
func (s *Sessions) Get(id SessionID) (*Session, bool) {
	s.mu.Lock()

	defer s.mu.Unlock()

	sess, ok := s.sessions[id]

	return sess, ok
}

// Holds reports whether the node holds a session of cl: the decision a
// client's first and last session are made under.
func (s *Sessions) Holds(cl vfs.ClientID) bool {
	s.mu.Lock()

	defer s.mu.Unlock()

	return len(s.byClient[cl]) > 0
}

// Bind binds the connection b to sess's fore channel, listing sess on it,
// and reports whether it did: not on a closed list, nor on an ended session.
// The connection's mutex is taken before the registry's, the order End keeps.
func (s *Sessions) Bind(sess *Session, b *Bound) bool {
	b.mu.Lock()

	defer b.mu.Unlock()

	if b.closed {
		return false
	}

	s.mu.Lock()

	defer s.mu.Unlock()

	if sess.ended {
		return false
	}

	b.ids = append(b.ids, sess.ID)
	sess.conns = append(sess.conns, b)

	return true
}

// End ends the session id names, returns its charge and takes its ID off the
// list of each connection bound, and reports whether it was the client's last
// on this node. A session already ended is not last.
func (s *Sessions) End(id SessionID) (last bool) {
	s.mu.Lock()

	sess, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()

		return false
	}

	s.endLocked(sess)

	last = len(s.byClient[sess.Client]) == 0
	conns := sess.conns
	s.mu.Unlock()

	// The registry's lock is released first: a connection's lock is always
	// taken before it, as Bind does.
	for _, b := range conns {
		b.drop(id)
	}

	return last
}

// EndClient ends every session of cl: the end of its lease.
func (s *Sessions) EndClient(cl vfs.ClientID) {
	s.mu.Lock()

	var ended []*Session

	for _, sess := range slices.Clone(s.byClient[cl]) {
		s.endLocked(sess)

		ended = append(ended, sess)
	}

	s.mu.Unlock()

	for _, sess := range ended {
		for _, b := range sess.conns {
			b.drop(sess.ID)
		}
	}
}

// endLocked takes sess out of the registry and releases its charge.
func (s *Sessions) endLocked(sess *Session) {
	sess.ended = true
	sess.charge.Release()

	delete(s.sessions, sess.ID)

	left := slices.DeleteFunc(s.byClient[sess.Client], isSession(sess))
	if len(left) == 0 {
		delete(s.byClient, sess.Client)

		return
	}

	s.byClient[sess.Client] = left
}

// isConn matches the connection c, for slices.DeleteFunc.
func isConn(c *core.Conn) func(*core.Conn) bool {
	return func(x *core.Conn) bool {
		return x == c
	}
}

// isID matches the session ID id, for slices.DeleteFunc.
func isID(id SessionID) func(SessionID) bool {
	return func(x SessionID) bool {
		return x == id
	}
}

// isSession matches the session sess, for slices.DeleteFunc.
func isSession(sess *Session) func(*Session) bool {
	return func(x *Session) bool {
		return x == sess
	}
}
