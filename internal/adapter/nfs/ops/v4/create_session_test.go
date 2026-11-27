package v4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsharness"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// The statuses and flags CREATE_SESSION answers (RFC 8881 section 15.1 and
// section 18.36.1), and the bytes of its reply, written out so no test reads
// them back from the code.
const (
	statusAccess        = 13
	statusTooSmall      = 10005
	statusStaleClient   = 10022
	statusSeqMisordered = 10063
	flagConnBackChan    = 2
	flagPersist         = 1
	offSession          = 8  // csr_sessionid, 16 bytes
	offSeqOut           = 24 // csr_sequence
	offFlagsOut         = 28
	offFore             = 32 // csr_fore_chan_attrs, 28 bytes
	offBack             = 60
	resultLen           = 88 // head, then the 80 bytes of CREATE_SESSION4resok
	slotsMax            = 64
	pendingBytes        = 1 << 10 // what the pool charges a proposal held
	sessionBytes        = 4 << 10
	slotBytes           = 256
	oneSlot             = slotBytes + 8<<10
	sequenceReply       = 80 // a lone SEQUENCE reply, summed from the XDR
)

// chanArgs is channel_attrs4 with no header padding and no RDMA.
func chanArgs(req, resp, cached, ops, reqs uint32) []byte {
	return words(0, req, resp, cached, ops, reqs, 0)
}

// callbackProgram is the cb_program every ask names.
const callbackProgram = 0x40000000

// secNone, secSys and secGSS are callback_sec_parms4 of each flavour.
func secNone() []byte {
	return words(0)
}

func secSys(uid uint32) []byte {
	b := words(1, 7)
	b = append(b, opaque([]byte("h"))...)

	return append(b, words(uid, 0, 0)...)
}

func secGSS() []byte {
	b := words(6, 1)
	b = append(b, opaque(nil)...)

	return append(b, opaque(nil)...)
}

// ask is CREATE_SESSION4args, built by hand from words.
type ask struct {
	client uint64
	seq    uint32
	flags  uint32
	fore   []byte
	back   []byte
	sec    [][]byte
}

// plainAsk asks one slot of each channel, for the client and sequence, with
// the room a SEQUENCE reply needs.
func plainAsk(cl vfs.ClientID, seq uint32) ask {
	return ask{
		client: uint64(cl),
		seq:    seq,
		fore:   chanArgs(1<<20, 1<<20, 4096, 8, 1),
		back:   chanArgs(4096, 4096, 0, 2, 1),
		sec:    [][]byte{secNone()},
	}
}

func (a ask) bytes() []byte {
	b := binary.BigEndian.AppendUint64(nil, a.client)
	b = append(b, words(a.seq, a.flags)...)
	b = append(b, a.fore...)
	b = append(b, a.back...)
	b = append(b, words(callbackProgram, uint32(len(a.sec)))...)

	return append(b, slices.Concat(a.sec...)...)
}

// node is one node's handlers over the runner's service, with a callbacks
// constructor.
type node struct {
	*exchange
}

// newNode is newExchange with a callbacks constructor.
func newNode(t *testing.T, rn *adaptertest.Runner, id uint64) *node {
	t.Helper()

	n := &node{exchange: newExchange(t, rn, id)}
	n.h.Callbacks = func(vfs.ClientID, vfs.Protocol) vfs.Callbacks {
		return &vfstest.Callbacks{}
	}

	return n
}

// withPool replaces the node's registry by one over a pool of ceiling bytes,
// which the test also holds, before any proposal.
func (n *node) withPool(ceiling int64) *resources.Budget {
	b := resources.NewBudget("t8.4", ceiling)
	n.sessions = protocol.NewSessions(&resources.Budgets{Connection: b}, &clock{}, &counter{})
	n.h.Sessions = n.sessions

	return b
}

// request is a COMPOUND's request on a connection, with the connection's
// session list.
func request(call vfs.Call, bound *protocol.Bound) *core.Request {
	return &core.Request{
		Call:    call,
		Version: 1,
		Conn:    &core.Conn{},
		State:   &protocol.State{Bound: bound},
	}
}

// create runs CREATE_SESSION as call over a connection with an open list.
func (n *node) create(t *testing.T, call vfs.Call, a ask, x adaptertest.Expect) []byte {
	t.Helper()

	return n.createOn(t, request(call, new(protocol.Bound)), a, x)
}

func (n *node) createOn(t *testing.T, r *core.Request, a ask, x adaptertest.Expect) []byte {
	t.Helper()

	return nfsharness.Member(t, n.rn, n.members, opCreateSession, r, a.bytes(), x)
}

var (
	builds = adaptertest.Expect{Completes: true}
	refuse = adaptertest.Expect{}
)

// propose runs EXCHANGE_ID as call and answers the client and the sequence
// ID its CREATE_SESSION must carry.
func (n *node) propose(t *testing.T, call vfs.Call, owner string) (vfs.ClientID, uint32) {
	t.Helper()

	got := n.do(t, call, args(owner, verifier2, 0, spNone()))
	wantStatus(t, got, statusOK)

	return clientID(got), field(got, offSeq)
}

// confirmed is a client the service holds confirmed, with the record's
// session sequence seq, as a CREATE_SESSION already made would leave it.
func (n *node) confirmed(t *testing.T, call vfs.Call, owner string, seq uint32) vfs.ClientID {
	t.Helper()

	return seed(t, n.rn, call, owner, boot2Minor1, recBytes(t, 1, seq))
}

func sessionOf(b []byte) protocol.SessionID {
	return protocol.SessionID(b[offSession : offSession+16])
}

// waitReached waits for the held call to arrive, and fails t, releasing it,
// if it does not within a few seconds.
func waitReached(t *testing.T, h *vfstest.Hold) {
	t.Helper()

	select {
	case <-h.Reached():
	case <-time.After(5 * time.Second):
		h.Release()
		t.Fatal("the held call never arrived")
	}
}

// wantCreated fails t unless got is a success of CREATE_SESSION, 88 bytes
// with its head.
func wantCreated(t *testing.T, got []byte) {
	t.Helper()

	wantStatus(t, got, statusOK)

	if len(got) != resultLen {
		t.Fatalf("CREATE_SESSION answered %d bytes, %x, want %d", len(got), got, resultLen)
	}
}

// sawOps fails t unless the service saw exactly ops as mutating calls.
func (n *node) sawOps(t *testing.T, want ...vfs.Op) {
	t.Helper()

	if got := n.rn.Rec.Ops(); !slices.Equal(got, want) {
		t.Errorf("the service saw %v, want %v", got, want)
	}
}

// A confirm asking for far more than every cap is granted the caps, in the
// bytes RFC 8881 section 18.36.1 lays out: 64 fore slots, 16 back slots, 8
// KiB cached, 128 operations and 1 MiB and 4 KiB for each size.
func TestCreateSessionGrantsTheCaps(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	a := plainAsk(cl, seq)
	a.fore = chanArgs(1<<30, 1<<30, 64<<10, 1000, 128)
	a.back = chanArgs(1<<30, 1<<30, 64<<10, 1000, 128)
	a.flags = flagConnBackChan
	a.sec = [][]byte{secSys(1000)}

	got := n.create(t, plain(rn), a, adaptertest.Expect{Completes: true, Call: vfs.OpConnect})
	wantCreated(t, got)

	want := words(0, 1<<20+4<<10, 1<<20+4<<10, 8192, 128, slotsMax, 0)
	if !bytes.Equal(got[offFore:offBack], want) {
		t.Errorf("the fore channel is %x, want %x", got[offFore:offBack], want)
	}

	want = words(0, 1<<20+4<<10, 1<<20+4<<10, 8192, 128, 16, 0)
	if !bytes.Equal(got[offBack:], want) || len(got) != resultLen {
		t.Errorf("the back channel is %x, want %x", got[offBack:], want)
	}

	if field(got, offSeqOut) != seq {
		t.Errorf("csr_sequence is %d, want the arguments' %d", field(got, offSeqOut), seq)
	}

	if f := field(got, offFlagsOut); f != flagConnBackChan {
		t.Errorf("csr_flags is %#x, want only CONN_BACK_CHAN, %#x", f, flagConnBackChan)
	}
}

// A confirm asking for little is granted what it asked for, and PERSIST,
// which a node that keeps no session on disk cannot honour, is never granted.
func TestCreateSessionGrantsTheLesser(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	a := plainAsk(cl, seq)
	a.flags = flagPersist
	a.fore = chanArgs(4096, 8192, 100, 1, 1)

	got := n.create(t, plain(rn), a, builds)
	wantCreated(t, got)

	if want := words(0, 4096, 8192, 100, 1, 1, 0); !bytes.Equal(got[offFore:offBack], want) {
		t.Errorf("the fore channel is %x, want what was asked, %x", got[offFore:offBack], want)
	}

	if f := field(got, offFlagsOut); f != 0 {
		t.Errorf("csr_flags is %#x, want none: PERSIST is not granted", f)
	}
}

// A pool with room for a proposal, the session and three slots grants three of
// the 64 slots asked, and charges every byte.
func TestCreateSessionChargesSlots(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	pool := n.withPool(pendingBytes + sessionBytes + 3*oneSlot)
	cl, seq := n.propose(t, plain(rn), "c1")

	a := plainAsk(cl, seq)
	a.fore = chanArgs(4096, 4096, 8192, 8, slotsMax)

	got := n.create(t, plain(rn), a, builds)
	wantCreated(t, got)

	if slots := field(got, offFore+20); slots != 3 {
		t.Errorf("the fore channel has %d slots, want 3, all the pool holds", slots)
	}

	if want := int64(sessionBytes + 3*oneSlot); pool.Reserved() != want {
		t.Errorf(
			"the pool holds %d bytes, want %d: the session, and no proposal",
			pool.Reserved(),
			want,
		)
	}
}

// A pool that cannot hold one slot answers DELAY before the client record is
// confirmed: the service saw no call and the proposal is still held, so a
// resend after the pool frees builds the session.
func TestCreateSessionDelaysWhenPoolIsFull(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	pool := n.withPool(pendingBytes + sessionBytes + oneSlot + 100)
	cl, seq := n.propose(t, plain(rn), "c1")

	hog, err := pool.Reserve(200)
	if err != nil {
		t.Fatal(err)
	}

	a := plainAsk(cl, seq)
	a.fore = chanArgs(4096, 4096, 8192, 8, 1)

	wantStatus(t, n.create(t, plain(rn), a, refuse), statusDelay)
	n.sawOps(t)

	if _, ok := n.sessions.Confirm(cl); !ok {
		t.Error("the proposal is gone after a refusal, want it held for the resend")
	}

	hog.Release()
	wantCreated(t, n.create(t, plain(rn), a, builds))
}

// A resend, whole, to another node answers the first node's reply byte for
// byte and writes nothing: the reply lives in the client record, not in the
// node's memory.
func TestCreateSessionReplayOnAnotherNode(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	a, b := newNode(t, rn, 7), newNode(t, rn, 8)
	cl, seq := a.propose(t, plain(rn), "c1")

	first := a.create(t, plain(rn), plainAsk(cl, seq), builds)
	wantCreated(t, first)

	again := b.create(t, plain(rn), plainAsk(cl, seq), refuse)
	if !bytes.Equal(again, first) {
		t.Errorf("node 8 answered %x, want node 7's %x", again, first)
	}

	b.sawOps(t)

	if b.sessions.Holds(cl) {
		t.Error("node 8 holds a session after a replay, want none")
	}
}

// Two nodes building one sequence at once end in one session: the node whose
// AddSession lost is told the record changed, ends its own session and
// answers the winner's reply.
func TestCreateSessionRaceBetweenNodes(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	a, b := newNode(t, rn, 7), newNode(t, rn, 8)
	cl, seq := a.propose(t, plain(rn), "c1")

	hold := rn.Faults.Hold(vfs.OpAddSession)

	done := make(chan outcome, 1)

	go func() {
		res, err := a.h.CreateSession(
			t.Context(),
			request(plain(rn), new(protocol.Bound)),
			decodeAsk(t, plainAsk(cl, seq)),
		)
		done <- outcome{res, err}
	}()

	waitReached(t, hold)

	won := b.create(t, plain(rn), plainAsk(cl, seq), builds)
	wantCreated(t, won)

	hold.Release()

	if got := (<-done).encode(t); !bytes.Equal(got, won[headLen:]) {
		t.Errorf("the loser answered %x, want the winner's %x", got, won[headLen:])
	}

	if a.sessions.Holds(cl) || !b.sessions.Holds(cl) {
		t.Errorf(
			"node 7 holds %v, node 8 %v, want only node 8",
			a.sessions.Holds(cl),
			b.sessions.Holds(cl),
		)
	}
}

// A failed AddSession leaves the client record confirmed with the sequence
// ID's predecessor, the pool as it was, and the proposal held; a resend builds
// exactly one session.
func TestCreateSessionRetryAfterAddSessionFails(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	pool := n.withPool(1 << 20)
	cl, seq := n.propose(t, plain(rn), "c1")
	before := pool.Reserved()

	rn.Faults.Fail(vfs.OpAddSession, &vfs.Delay{Cause: vfs.DelayResources})
	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, seq), builds), statusDelay)

	if pool.Reserved() != before {
		t.Errorf("the pool holds %d after the failure, want %d", pool.Reserved(), before)
	}

	rec, err := rn.Mem.Record(t.Context(), plain(rn), cl)
	if err != nil {
		t.Fatal(err)
	}

	d, err := wire.DecodeClientRec(rec.Adapter)
	if err != nil {
		t.Fatal(err)
	}

	if d.NFS4.SessionSeq != seq-1 {
		t.Errorf("the record's SessionSeq is %d, want %d", d.NFS4.SessionSeq, seq-1)
	}

	wantCreated(t, n.create(t, plain(rn), plainAsk(cl, seq), builds))

	if !n.sessions.Holds(cl) {
		t.Error("the resend built no session")
	}
}

// A client record that changes under every decision is read again at most
// three times: the third change is answered DELAY and the pool as it was, and
// two changes then a quiet read still build. The handler is called directly
// here: the harness allows a request one AddSession, which a second decision
// over a changed record exceeds by design.
func TestCreateSessionDecisionsAreBounded(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	pool := n.withPool(1 << 20)
	changed := &vfs.StateError{Kind: vfs.StateRecordChanged}

	cl, seq := n.propose(t, plain(rn), "c1")

	for range 2 {
		rn.Faults.Fail(vfs.OpAddSession, changed)
	}

	res, err := n.h.CreateSession(
		t.Context(),
		request(plain(rn), new(protocol.Bound)),
		decodeAsk(t, plainAsk(cl, seq)),
	)
	if err != nil || res.Ok == nil {
		t.Fatalf("two changes answered %+v, %v, want a session", res, err)
	}

	cl, seq = n.propose(t, plain(rn), "c2")
	before := pool.Reserved()

	for range 3 {
		rn.Faults.Fail(vfs.OpAddSession, changed)
	}

	_, err = n.h.CreateSession(
		t.Context(),
		request(plain(rn), new(protocol.Bound)),
		decodeAsk(t, plainAsk(cl, seq)),
	)

	var d *vfs.Delay
	if !errors.As(err, &d) || d.Cause != vfs.DelayConflict {
		t.Errorf("three changes answered %v, want a delay for a conflict", err)
	}

	if pool.Reserved() != before {
		t.Errorf("the pool holds %d after the delay, want %d", pool.Reserved(), before)
	}
}

// A Connect that meets a stale client, for a record that has none, is
// answered STALE_CLIENTID after exactly one Connect, and the proposal is
// dropped.
func TestCreateSessionStaleClientIsBounded(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	rn.Faults.Fail(vfs.OpConnect, &vfs.StateError{Kind: vfs.StateStaleClient})
	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, seq), builds), statusStaleClient)
	n.sawOps(t, vfs.OpConnect)

	if _, ok := n.sessions.Confirm(cl); ok {
		t.Error("the proposal is held after STALE_CLIENTID, want it dropped")
	}

	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, seq), refuse), statusStaleClient)
	n.sawOps(t)
}

// A stale Connect whose record read then fails for another reason is not a
// client that is gone: the error comes back and the proposal stays for the
// resend.
func TestCreateSessionRecordReadFailureKeepsTheProposal(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	rn.Faults.Fail(vfs.OpConnect, &vfs.StateError{Kind: vfs.StateStaleClient})
	// The first read is the decision's, which finds no record yet.
	rn.Faults.Fail(vfs.OpRecord, &vfs.StateError{Kind: vfs.StateStaleClient})
	rn.Faults.Fail(vfs.OpRecord, &vfs.Delay{Cause: vfs.DelayResources})
	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, seq), builds), statusDelay)
	n.sawOps(t, vfs.OpConnect)

	if _, ok := n.sessions.Confirm(cl); !ok {
		t.Error("the proposal is dropped after a failed record read, want it kept")
	}

	wantCreated(t, n.create(t, plain(rn), plainAsk(cl, seq), builds))
}

// Two confirms of one sequence on one node, the first held at Connect until
// the second finished: the first meets a stale client, reads the record the
// second wrote and answers the second's reply, never STALE_CLIENTID.
func TestCreateSessionSameNodeRace(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	hold := rn.Faults.Hold(vfs.OpConnect)

	done := make(chan outcome, 1)

	go func() {
		res, err := n.h.CreateSession(
			t.Context(),
			request(plain(rn), new(protocol.Bound)),
			decodeAsk(t, plainAsk(cl, seq)),
		)
		done <- outcome{res, err}
	}()

	waitReached(t, hold)

	won := n.create(t, plain(rn), plainAsk(cl, seq), builds)
	wantCreated(t, won)

	hold.Release()

	if got := (<-done).encode(t); !bytes.Equal(got, won[headLen:]) {
		t.Errorf("the first answered %x, want the second's %x", got, won[headLen:])
	}

	// The loser decides again over the record: it writes nothing more.
	if got := slices.Index(n.rn.Rec.Ops(), vfs.OpAddSession); got < 0 ||
		slices.Contains(n.rn.Rec.Ops()[got+1:], vfs.OpAddSession) {
		t.Errorf("the service saw %v, want one AddSession", n.rn.Rec.Ops())
	}
}

// A client record that is gone when the back channel registers its callbacks
// is recreated by that Connect with the bytes the call holds: the owner's next
// EXCHANGE_ID reads them, so they hold the record's sequence ID and reply.
func TestCreateSessionRecordGoneBeforeTheBackChannel(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl := n.confirmed(t, plain(rn), "c1", 5)

	a := plainAsk(cl, 6)
	a.flags = flagConnBackChan
	a.sec = [][]byte{secNone()}

	hold := rn.Faults.Hold(vfs.OpConnect)
	done := make(chan error, 1)

	go func() {
		_, err := n.h.CreateSession(
			t.Context(),
			request(plain(rn), new(protocol.Bound)),
			decodeAsk(t, a),
		)
		done <- err
	}()

	waitReached(t, hold)

	if err := rn.Mem.Store.Store.Clients().Delete(t.Context(), cl); err != nil {
		t.Fatal(err)
	}

	hold.Release()

	if err := <-done; err != nil {
		t.Fatalf("CREATE_SESSION answered %v, want the session", err)
	}

	got := n.do(t, plain(rn), args("c1", verifier2, 0, spNone()))
	wantStatus(t, got, statusOK)

	if f := field(got, offSeq); f != 7 {
		t.Errorf("EXCHANGE_ID answered sequence %d, want 7: the record's own", f)
	}

	rec, err := rn.Mem.Record(t.Context(), plain(rn), clientID(got))
	if err != nil {
		t.Fatal(err)
	}

	if d := decodeRecord(t, clientID(got), rec.Adapter); d.NFS4.SessionSeq != 6 ||
		len(d.NFS4.SessionReply) == 0 {
		t.Errorf("the recreated record holds %+v, want sequence 6 and its reply", d.NFS4)
	}
}

// A last Disconnect the service answers with a newer entry's Gen is sent
// again at that Gen, until the node's entry is gone: a listing made after the
// first one, which the node's own sessions ended before, is not left behind.
func TestCreateSessionLastDisconnectFollowsANewerEntry(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	added := rn.Faults.Hold(vfs.OpAddSession)
	done := make(chan error, 1)

	go func() {
		_, err := n.h.CreateSession(
			t.Context(),
			request(plain(rn), new(protocol.Bound)),
			decodeAsk(t, plainAsk(cl, seq)),
		)
		done <- err
	}()

	waitReached(t, added)
	n.sessions.EndClient(cl)

	left := rn.Faults.Hold(vfs.OpDisconnect)

	added.Release()
	waitReached(t, left)

	// A later listing of this node, while the first Disconnect is on its way.
	rec, err := rn.Mem.Record(t.Context(), plain(rn), cl)
	if err != nil {
		t.Fatal(err)
	}

	later, err := rn.Mem.AddSession(t.Context(), plain(rn), cl, rec.Adapter, rec.Adapter)
	if err != nil {
		t.Fatal(err)
	}

	left.Release()

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var sent []vfs.SessionEnd

	for _, m := range rn.Rec.Calls() {
		if m.Op != vfs.OpDisconnect {
			continue
		}

		if m.Session == (vfs.SessionRef{}) {
			t.Error("a Disconnect named no session, want the session's reference")
		}

		sent = append(sent, m.End)
	}

	if len(sent) != 2 || sent[1].Gen != later.Reach.Gen || !sent[1].Last {
		t.Errorf(
			"the service saw %+v, want a second Last at the later Gen, %d",
			sent,
			later.Reach.Gen,
		)
	}

	end, err := rn.Mem.Record(t.Context(), plain(rn), cl)
	if err != nil {
		t.Fatal(err)
	}

	if len(end.Reach.Sessions) != 0 {
		t.Errorf("the record lists %v, want no node", end.Reach.Sessions)
	}
}

// A lease that ends while CREATE_SESSION runs, and a service that already
// forgot the client when the last Disconnect arrives, still answer the
// session the record holds: the client is not told its client ID is stale.
func TestCreateSessionLastDisconnectOfAGoneClient(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	rn.Faults.Fail(vfs.OpDisconnect, &vfs.StateError{Kind: vfs.StateStaleClient})

	got := n.lastLeaseEnds(t, cl, seq)
	if got.err != nil {
		t.Fatalf("CREATE_SESSION answered %v, want the session", got.err)
	}

	if got.res.Ok == nil {
		t.Error("CREATE_SESSION answered no session")
	}
}

// A last Disconnect that fails for any other reason is not a client that is
// gone: its error comes back after one attempt. The repeat is the node's
// retrier's.
func TestCreateSessionLastDisconnectFailureComesBack(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	rn.Faults.Fail(vfs.OpDisconnect, &vfs.Delay{Cause: vfs.DelayResources})

	got := n.lastLeaseEnds(t, cl, seq)

	var delay *vfs.Delay
	if !errors.As(got.err, &delay) {
		t.Errorf("CREATE_SESSION answered %v, want the Disconnect's delay", got.err)
	}

	n.sawOps(t, vfs.OpConnect, vfs.OpAddSession, vfs.OpDisconnect)
}

// lastLeaseEnds runs CREATE_SESSION with the node's sessions of the client
// ended while its AddSession is held, so the call meets the last Disconnect.
func (n *node) lastLeaseEnds(t *testing.T, cl vfs.ClientID, seq uint32) outcome {
	t.Helper()

	hold := n.rn.Faults.Hold(vfs.OpAddSession)
	done := make(chan outcome, 1)

	go func() {
		res, err := n.h.CreateSession(
			t.Context(),
			request(plain(n.rn), new(protocol.Bound)),
			decodeAsk(t, plainAsk(cl, seq)),
		)
		done <- outcome{res, err}
	}()

	waitReached(t, hold)
	n.sessions.EndClient(cl)
	hold.Release()

	return <-done
}

// A back channel whose second Connect fails answers that error. The session
// stays built (the record holds it, so a resend replays it): decided in
// scope 14.8, asserted there only as the error.
func TestCreateSessionBackChannelConnectFailureComesBack(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl := n.confirmed(t, plain(rn), "c1", 5)

	a := plainAsk(cl, 6)
	a.flags = flagConnBackChan
	a.sec = [][]byte{secSys(1000)}

	rn.Faults.Fail(vfs.OpConnect, &vfs.Delay{Cause: vfs.DelayResources})

	got := n.create(t, plain(rn), a, builds)
	if len(got) < headLen || status(got) != statusDelay {
		t.Fatalf(
			"CREATE_SESSION answered %x with the back channel's Connect failing, want status %d",
			got,
			statusDelay,
		)
	}

	n.sawOps(t, vfs.OpAddSession, vfs.OpConnect)
}

// A client the service holds already, asking no back channel, still has its
// callbacks registered with Connect by the node that builds its session: a
// node restarted since the client's last CREATE_SESSION holds none, and
// Revoked would never end the session without it. A confirm over a proposal
// has made that Connect already, and makes no second one.
func TestCreateSessionRegistersTheCallbacksOfAKnownClient(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl := n.confirmed(t, plain(rn), "c1", 5)

	wantCreated(t, n.create(t, plain(rn), plainAsk(cl, 6), builds))
	n.sawOps(t, vfs.OpAddSession, vfs.OpConnect)
}

// The sequence rule over a record: the sequence the record stores with no
// reply, and two past it, are SEQ_MISORDERED; the one after it builds, and
// wraps past the highest 32-bit number.
func TestCreateSessionSequenceRule(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl := n.confirmed(t, plain(rn), "c1", 5)

	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, 5), refuse), statusSeqMisordered)
	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, 7), refuse), statusSeqMisordered)
	wantCreated(t, n.create(t, plain(rn), plainAsk(cl, 6), builds))

	// Over a proposal only the sequence ID EXCHANGE_ID answered builds.
	pcl, pseq := n.propose(t, plain(rn), "c3")

	wantStatus(t, n.create(t, plain(rn), plainAsk(pcl, pseq-1), refuse), statusSeqMisordered)
	wantStatus(t, n.create(t, plain(rn), plainAsk(pcl, pseq+1), refuse), statusSeqMisordered)
	wantCreated(t, n.create(t, plain(rn), plainAsk(pcl, pseq), builds))

	wrap := n.confirmed(t, plain(rn), "c2", 0xFFFFFFFF)
	wantCreated(t, n.create(t, plain(rn), plainAsk(wrap, 0), builds))
}

// A fore channel that cannot carry the lone SEQUENCE reply, or allows no
// operation, is NFS4ERR_TOOSMALL before anything is written; one that fits
// exactly builds.
func TestCreateSessionTooSmall(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl := n.confirmed(t, plain(rn), "c1", 5)

	a := plainAsk(cl, 6)
	a.fore = chanArgs(4096, 4096, 0, 0, 1)
	wantStatus(t, n.create(t, plain(rn), a, refuse), statusTooSmall)

	a.fore = chanArgs(4096, sequenceReply-1, 0, 1, 1)
	wantStatus(t, n.create(t, plain(rn), a, refuse), statusTooSmall)
	n.sawOps(t)

	a.fore = chanArgs(4096, sequenceReply, 0, 1, 1)
	wantCreated(t, n.create(t, plain(rn), a, builds))
}

// A record an NFSv4.0 client confirmed, or a proposal SETCLIENTID made, is
// a stale client to CREATE_SESSION, with nothing written.
func TestCreateSessionRefusesV40Clients(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)

	cl := seed(t, rn, plain(rn), "c40", boot1Minor0, recBytes(t, 0, 5))
	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, 6), refuse), statusStaleClient)

	info := vfs.ClientInfo{
		Protocol: vfs.ProtocolNFSv4,
		Name:     []byte("c40b"),
		Boot:     boot1Minor0,
		Recovery: recoverStatus,
	}

	p, err := rn.Mem.Propose(t.Context(), plain(rn), info)
	if err != nil {
		t.Fatal(err)
	}

	err = n.sessions.Propose(info.Name, protocol.Pending{
		Proposal: p,
		Minor:    0,
		Confirm:  p.Confirm,
		Info:     info,
	})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(
		t,
		n.create(t, plain(rn), plainAsk(p.Client, protocol.SequenceOf(p.Confirm)), refuse),
		statusStaleClient,
	)
	n.sawOps(t)
}

// The record a confirm leaves carries the NFSv4.1 recovery kind.
func TestCreateSessionRecordsRecovery(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	wantCreated(t, n.create(t, plain(rn), plainAsk(cl, seq), builds))

	rec, err := rn.Mem.Record(t.Context(), plain(rn), cl)
	if err != nil {
		t.Fatal(err)
	}

	if rec.Recovery != recoverStatus {
		t.Errorf("the record's recovery is %v, want %v", rec.Recovery, recoverStatus)
	}
}

// A record that asked for machine credential protection is not served to a
// caller that does not prove the machine, whether it builds or replays, and
// the service sees nothing.
func TestCreateSessionProtectedClient(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	owner := protected(rn, vfs.ProtectionIntegrity)

	got := n.do(t, owner, args("c1", verifier2, 0, spMach([]uint32{0}, []uint32{0})))
	wantStatus(t, got, statusOK)
	// got is EXCHANGE_ID's

	cl, seq := clientID(got), field(got, offSeq)

	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, seq), refuse), statusAccess)

	weak := protected(rn, vfs.ProtectionIntegrity)
	weak.Identity.Proof = vfs.ProofClaimed
	wantStatus(t, n.create(t, weak, plainAsk(cl, seq), refuse), statusAccess)
	n.sawOps(t)

	first := n.create(t, owner, plainAsk(cl, seq), builds)
	wantCreated(t, first)

	if sess, ok := n.sessions.Get(sessionOf(first)); !ok || !sess.Protect ||
		!slices.Equal(sess.MustEnforce, []uint32{0}) || !slices.Equal(sess.MustAllow, []uint32{0}) {
		t.Errorf("the session holds %+v, want the record's protection and lists", sess)
	}

	wantStatus(t, n.create(t, plain(rn), plainAsk(cl, seq), refuse), statusAccess)

	// a second session of the proved owner is served too: its Connect proves
	// the machine as the first call did
	wantCreated(t, n.create(t, owner, plainAsk(cl, seq+1), builds))
}

// A CREATE_SESSION is a mutation of the client record like EXCHANGE_ID's
// drain row: it is served under the table's rows only; its error rows map
// the pool's delay to NFS4ERR_DELAY.
func TestCreateSessionRows(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)

	op := n.members.Ops[opCreateSession]
	if op == nil || op.Versions != (core.Range{First: 1, Last: lastMinor}) {
		t.Fatalf("the row is %+v, want one serving minor versions 1 to %d", op, lastMinor)
	}

	for err, want := range map[error]uint32{
		&vfs.Delay{Cause: vfs.DelayResources}:       statusDelay,
		&vfs.StateError{Kind: vfs.StateStaleClient}: statusStaleClient,
		&vfs.StateError{Kind: vfs.StateClientInUse}: statusInUse,
		errSeqMisordered: statusSeqMisordered,
		errTooSmall:      statusTooSmall,
		errAccess:        statusAccess,
		&vfs.StateError{Kind: vfs.StateClientBusy}: 10074,
	} {
		s, _, _ := n.members.Errors.Map(err, op, 1)
		if uint32(s) != want {
			t.Errorf("%v maps to %d, want %d", err, s, want)
		}
	}
}

// With a back channel asked, a session whose client can be called on AUTH_SYS
// keeps it and Connect registers the callbacks again; one whose client names
// RPCSEC_GSS alone has none, and the flag is cleared.
func TestCreateSessionBackChannel(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)

	for _, tc := range []struct {
		name    string
		sec     [][]byte
		back    bool
		calls   int
		flavour uint32
	}{
		{"gss", [][]byte{secGSS()}, false, 1, 0},
		{"sys", [][]byte{secGSS(), secSys(1000)}, true, 2, flavourSys},
		{"none", [][]byte{secNone()}, true, 2, flavourNone},
	} {
		cl, seq := n.propose(t, plain(rn), tc.name)

		a := plainAsk(cl, seq)
		a.flags = flagConnBackChan
		a.sec = tc.sec

		x := adaptertest.Expect{Completes: true, Call: vfs.OpConnect}
		if tc.calls == 1 {
			x = builds
		}

		got := n.create(t, plain(rn), a, x)
		wantCreated(t, got)

		sess, _ := n.sessions.Get(sessionOf(got))
		if (sess.Back != nil) != tc.back {
			t.Errorf(
				"%s: the session's back channel is %v, want present %v",
				tc.name,
				sess.Back,
				tc.back,
			)
		}

		if tc.back && (sess.Back.Program != callbackProgram || sess.Back.Flavour != tc.flavour) {
			t.Errorf(
				"%s: the back channel calls program %d under flavour %d, want %d under %d",
				tc.name,
				sess.Back.Program,
				sess.Back.Flavour,
				callbackProgram,
				tc.flavour,
			)
		}

		if tc.back {
			// The call's connection is bound: a second one joins it, and
			// leaving again finds the first still listed.
			other := &core.Conn{}
			sess.Back.Bind(other)

			if sess.Back.Unbind(other) {
				t.Errorf("%s: the call's connection is not bound to the back channel", tc.name)
			}
		}

		if f := field(got, offFlagsOut)&flagConnBackChan != 0; f != tc.back {
			t.Errorf("%s: CONN_BACK_CHAN is %v in the reply, want %v", tc.name, f, tc.back)
		}

		connects := 0

		for _, op := range rn.Rec.Ops() {
			if op == vfs.OpConnect {
				connects++
			}
		}

		if connects != tc.calls {
			t.Errorf("%s: %d Connect calls, want %d", tc.name, connects, tc.calls)
		}
	}
}

// The record a build writes differs from the one read before in its session
// sequence and its stored reply alone: the client's bitmaps and the rest
// survive the rewrite.
func TestCreateSessionRecordBytes(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	owner := protected(rn, vfs.ProtectionIntegrity)

	first := n.do(t, owner, args("c1", verifier2, 0, spMach([]uint32{3, 5}, []uint32{7})))
	wantStatus(t, first, statusOK)

	cl, seq := clientID(first), field(first, offSeq)

	pending, ok := n.sessions.Confirm(cl)
	if !ok {
		t.Fatal("the proposal is not held")
	}

	before := decodeRecord(t, cl, pending.Info.Adapter)
	if len(before.NFS4.MustEnforce) == 0 || len(before.NFS4.MustAllow) == 0 {
		t.Fatalf("the record before holds no bitmaps: %+v", before)
	}

	got := n.create(t, owner, plainAsk(cl, seq), builds)
	wantCreated(t, got)
	n.sawOps(t, vfs.OpConnect, vfs.OpAddSession)

	stored, err := rn.Mem.Record(t.Context(), owner, cl)
	if err != nil {
		t.Fatal(err)
	}

	after := decodeRecord(t, cl, stored.Adapter)
	if after.NFS4.SessionSeq != seq || !bytes.Equal(after.NFS4.SessionReply, got[headLen:]) {
		t.Errorf("the record holds sequence %d and reply %x, want %d and %x",
			after.NFS4.SessionSeq, after.NFS4.SessionReply, seq, got[headLen:])
	}

	after.NFS4.SessionSeq = before.NFS4.SessionSeq
	after.NFS4.SessionReply = before.NFS4.SessionReply

	if !reflect.DeepEqual(after, before) {
		t.Errorf("the record is %+v, want %+v", after, before)
	}

	n.create(t, owner, plainAsk(cl, seq), refuse)
	n.sawOps(t)
}

// decodeRecord decodes the client's record bytes.
func decodeRecord(t *testing.T, cl vfs.ClientID, b []byte) wire.ClientRec {
	t.Helper()

	rec, err := protocol.ReadRecord(cl, b)
	if err != nil {
		t.Fatal(err)
	}

	return rec
}

// A connection whose list is closed gets the session built and bound to
// nothing: its list stays empty.
func TestCreateSessionClosedList(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	cl, seq := n.propose(t, plain(rn), "c1")

	closed := new(protocol.Bound)
	closed.Close()

	got := n.createOn(t, request(plain(rn), closed), plainAsk(cl, seq), builds)
	wantCreated(t, got)

	if ids := closed.IDs(); len(ids) != 0 {
		t.Errorf("the closed list holds %x, want none", ids)
	}

	cl2, seq2 := n.propose(t, plain(rn), "c2")
	bound := new(protocol.Bound)

	made := n.createOn(t, request(plain(rn), bound), plainAsk(cl2, seq2), builds)
	wantCreated(t, made)

	if ids := bound.IDs(); len(ids) != 1 || ids[0] != sessionOf(made) {
		t.Errorf("the connection's list holds %x, want the session built, %x", ids, sessionOf(made))
	}

	open := new(protocol.Bound)
	got = n.createOn(t, request(plain(rn), open), plainAsk(cl, seq), refuse)

	if ids := open.IDs(); len(ids) != 0 {
		t.Errorf("a replay bound %x, want none: only a build binds", ids)
	}

	_ = got
}

// Another principal's CREATE_SESSION on a proposal is CLID_INUSE, and leaves
// the proposal for its owner, the pool as it was and no session; the owner
// then builds.
func TestCreateSessionOtherPrincipal(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	n := newNode(t, rn, 7)
	pool := n.withPool(1 << 20)
	cl, seq := n.propose(t, plain(rn), "c1")
	before := pool.Reserved()

	wantStatus(t, n.create(t, alice(t, rn), plainAsk(cl, seq), builds), statusInUse)

	for _, op := range rn.Rec.Ops() {
		if op == vfs.OpAddSession {
			t.Error("AddSession was called for another principal")
		}
	}

	if _, ok := n.sessions.Confirm(cl); !ok {
		t.Error("the proposal is gone, want it kept for its owner")
	}

	if pool.Reserved() != before || n.sessions.Holds(cl) {
		t.Errorf(
			"pool %d (want %d), holds a session %v: want neither",
			pool.Reserved(),
			before,
			n.sessions.Holds(cl),
		)
	}

	wantCreated(t, n.create(t, plain(rn), plainAsk(cl, seq), builds))
}

// decodeAsk and encodeRes cross a handler call over bytes, for the tests that
// run two requests at once, which the runner's one-request record cannot.
func decodeAsk(t *testing.T, a ask) createSessionArgs {
	t.Helper()

	var out createSessionArgs
	if _, err := out.DecodeWire(a.bytes(), 1); err != nil {
		t.Fatal(err)
	}

	return out
}

// outcome is what a CREATE_SESSION run on its own goroutine answered: the
// test's goroutine checks it, since t.Fatal off that goroutine hangs the test.
type outcome struct {
	res createSessionRes
	err error
}

func (o outcome) encode(t *testing.T) []byte {
	t.Helper()

	if o.err != nil {
		t.Fatalf("CREATE_SESSION on its own goroutine answered %v, want a reply", o.err)
	}

	return encodeRes(t, o.res)
}

func encodeRes(t *testing.T, r createSessionRes) []byte {
	t.Helper()

	if r.Ok == nil {
		t.Fatal("the handler answered no success")
	}

	var buf resources.Buf
	if err := r.EncodeWire(1, &buf); err != nil {
		t.Fatal(err)
	}

	return buf.B
}
