package v4

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/adapter/adaptertest"
	"github.com/cubbit/dittofs/internal/adapter/adaptertest/nfsharness"
	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/protocol"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// The statuses the tests read (RFC 8881 section 15.1) and the bytes of the
// reply's fixed part, written out so no test reads them back from the code.
const (
	statusOK         = 0
	statusPerm       = 1
	statusNoEnt      = 2
	statusInval      = 22
	statusFault      = 10006
	statusDelay      = 10008
	statusInUse      = 10017
	statusNotSame    = 10027
	statusEncrAlg    = 10079
	flagsNonPNFS     = 0x00010000
	flagsConfirmed   = 0x80010000
	flagsPNFSBits    = 0x00060000 // USE_PNFS_MDS and USE_PNFS_DS
	headLen          = 8          // resop and status
	offClientID      = 8
	offMinorID       = 28 // so_minor_id, after the client ID, sequence ID, flags and state protection
	offSeq           = 16
	offFlags         = 20
	offProtect       = 24
	offMinor         = 28
	offMajor         = 36 // its length word, then 16 bytes
	offScope         = 56
	installationSize = 16
)

// installation is the installation ID the tests' nodes share.
var installation = [16]byte{
	0xa0, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
	0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf,
}

// The boots of the verifier 0102030405060708 under minor versions 0 and 1,
// and of 1122334455667788 under minor version 1, from SHA-256 of the
// verifier and the minor version's byte, first eight bytes (computed apart
// from the code).
var (
	verifier1   = [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	boot1Minor0 = [8]byte{0x52, 0x48, 0x35, 0x8b, 0xd9, 0x63, 0x35, 0xe3}
	boot1Minor1 = [8]byte{0xaf, 0x9f, 0x09, 0xbf, 0xc1, 0x66, 0x09, 0x97}
	verifier2   = [8]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	boot2Minor1 = [8]byte{0xaf, 0x7b, 0xe5, 0xe5, 0xa8, 0x69, 0x28, 0x39}
)

// counter is a source of confirms the test can foresee: the bytes 1, 2, 3 and
// on, so the first confirm is 0102030405060708 and its low half 0x05060708.
type counter struct {
	next byte
}

func (c *counter) Read(p []byte) (int, error) {
	for i := range p {
		c.next++
		p[i] = c.next
	}

	return len(p), nil
}

// clock is the pending table's time, which the test advances.
type clock struct {
	now time.Time
}

func (c *clock) Now() time.Time {
	return c.now
}

// exchange is one node's EXCHANGE_ID handler over the in-memory service.
type exchange struct {
	rn       *adaptertest.Runner
	sessions *protocol.Sessions
	h        *Handlers
	members  *core.OpTable
}

// newExchange builds a node whose ID starts with the byte node over rn's service.
func newExchange(t *testing.T, rn *adaptertest.Runner, node byte) *exchange {
	t.Helper()

	e := &exchange{rn: rn}
	e.sessions = protocol.NewSessions(adaptertest.Budgets(), &clock{}, &counter{})
	e.h = &Handlers{
		VFS:      rn.Rec,
		Sessions: e.sessions,
		Server:   Server{Installation: installation, Node: [16]byte{node}},
	}
	e.members = Table(e.h).Ops[wire.ProcCompound].Members

	return e
}

// plain is the share's administrator, proved by a ticket, under no
// protection: a caller that proves no machine credential.
func plain(rn *adaptertest.Runner) vfs.Call {
	c := rn.Mem.Admin()
	c.Identity.Protection = vfs.ProtectionNone

	return c
}

// protected is the same administrator under protection p.
func protected(rn *adaptertest.Runner, p vfs.Protection) vfs.Call {
	c := rn.Mem.Admin()
	c.Identity.Protection = p

	return c
}

// alice is another principal, a user the store holds, proved by a ticket
// under no protection.
func alice(t *testing.T, rn *adaptertest.Runner) vfs.Call {
	t.Helper()

	return adaptertest.OtherUser(t, rn.Mem)
}

// args is EXCHANGE_ID4args as RFC 8881 section 18.35.1 lays it out, built
// by hand from words and opaques.
func args(owner string, verifier [8]byte, flags uint32, protect []byte) []byte {
	b := append([]byte(nil), verifier[:]...)
	b = append(b, opaque([]byte(owner))...)
	b = append(b, words(flags)...)
	b = append(b, protect...)

	return append(b, words(0)...) // no eia_client_impl_id
}

// spNone is state_protect4_a for SP4_NONE.
func spNone() []byte {
	return words(0)
}

// spMach is state_protect4_a for SP4_MACH_CRED with the two bitmaps.
func spMach(enforce, allow []uint32) []byte {
	b := words(1, uint32(len(enforce)))
	b = append(b, words(enforce...)...)
	b = append(b, words(uint32(len(allow)))...)

	return append(b, words(allow...)...)
}

// do sends args as call and answers the member's result bytes, the service's
// record holding no mutating call.
func (e *exchange) do(t *testing.T, call vfs.Call, a []byte) []byte {
	t.Helper()

	r := &core.Request{Call: call, Version: 1, State: &protocol.State{}}

	return nfsharness.Member(
		t,
		e.rn,
		e.members,
		opExchangeID,
		r,
		a,
		adaptertest.Expect{SessionSetup: true},
	)
}

// status is a result's status word.
func status(b []byte) uint32 {
	return binary.BigEndian.Uint32(b[4:])
}

// clientID is a success's eir_clientid.
func clientID(b []byte) vfs.ClientID {
	return vfs.ClientID(binary.BigEndian.Uint64(b[offClientID:]))
}

// field is the word at off of a success.
func field(b []byte, off int) uint32 {
	return binary.BigEndian.Uint32(b[off:])
}

// wantStatus fails t unless got is a result of status want.
func wantStatus(t *testing.T, got []byte, want uint32) {
	t.Helper()

	if len(got) < headLen || status(got) != want {
		t.Fatalf("EXCHANGE_ID answered %x, want status %d", got, want)
	}
}

// seed confirms owner as a client of caller's principal, as the service
// would hold it after CREATE_SESSION: the record's Boot, its Adapter bytes
// and the callbacks.
func seed(
	t *testing.T,
	rn *adaptertest.Runner,
	call vfs.Call,
	owner string,
	boot [8]byte,
	adapter []byte,
) vfs.ClientID {
	t.Helper()

	info := vfs.ClientInfo{
		Protocol: vfs.ProtocolNFSv4,
		Name:     []byte(owner),
		Boot:     boot,
		Adapter:  adapter,
		Recovery: recoverStatus,
	}

	p, err := rn.Mem.Propose(context.Background(), call, info)
	if err != nil {
		t.Fatalf("seed Propose: %v", err)
	}

	info.Proposal = &p

	id, _, err := rn.Mem.Connect(context.Background(), call, info, &vfstest.Callbacks{})
	if err != nil {
		t.Fatalf("seed Connect: %v", err)
	}

	return id
}

// recBytes is the Adapter bytes of an NFSv4.1 record with session sequence
// seq.
func recBytes(t *testing.T, minor, seq uint32) []byte {
	t.Helper()

	b, err := wire.AppendClientRec(nil, &wire.ClientRec{
		Kind: wire.ClientRecKindNFS4,
		NFS4: wire.ClientRecNFS4{Minor: minor, SessionSeq: seq},
	})
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// patchID writes id over the placeholder client ID of a golden result, and
// the minor ID of node 7, the node the tests that compare a golden build,
// over its placeholder (the minor ID is a hash of the node's ID, so the
// golden bytes cannot hold it).
func patchID(b []byte, id vfs.ClientID) []byte {
	b = bytes.Clone(b)
	binary.BigEndian.PutUint64(b[offClientID:], uint64(id))

	sum := sha256.Sum256(append([]byte{7}, make([]byte, 15)...))
	binary.BigEndian.PutUint64(b[offMinorID:], binary.BigEndian.Uint64(sum[:8]))

	return b
}

// nothingHeld fails t if the node's table holds a proposal for id.
func (e *exchange) nothingHeld(t *testing.T, id vfs.ClientID) {
	t.Helper()

	if _, ok := e.sessions.Confirm(id); ok {
		t.Errorf("the table holds a proposal for client %d, want none", id)
	}
}

// noPending fails t if the service holds a proposal for the confirmed client
// id: an EXCHANGE_ID under UPD_CONFIRMED_REC_A makes none (RFC 21 section
// 3.5), and the adapter's own table cannot show one the service made.
func (e *exchange) noPending(t *testing.T, id vfs.ClientID) {
	t.Helper()

	rec, err := e.rn.Mem.Record(t.Context(), plain(e.rn), id)
	if err != nil {
		t.Fatalf("reading client %d's record: %v", id, err)
	}

	if rec.Pending != nil {
		t.Errorf("the service holds a proposal for client %d, want none", id)
	}
}

// A new owner is answered its proposed client ID and the low half of the
// adapter's own confirm as the sequence ID, byte for byte as RFC 8881
// section 18.35.1 lays it out, and the proposal is held for CREATE_SESSION:
// its record bytes carry the sequence ID minus one, and nothing was written.
func TestExchangeIDNewOwner(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)

	got := e.do(t, plain(rn), golden(t, "rfc-nfs4-exchange-id-args"))
	wantStatus(t, got, statusOK)

	id := clientID(got)

	want := patchID(golden(t, "rfc-nfs4-exchange-id-new"), id)
	if !bytes.Equal(got, want) {
		t.Errorf("the result is %x, want %x", got, want)
	}

	held, ok := e.sessions.Confirm(id)
	if !ok {
		t.Fatalf("the table holds no proposal for client %d", id)
	}

	if held.Proposal.Client != id {
		t.Errorf("the held proposal is for client %d, want %d", held.Proposal.Client, id)
	}

	rec, err := wire.DecodeClientRec(held.Info.Adapter)
	if err != nil {
		t.Fatal(err)
	}

	if rec.NFS4.SessionSeq != 0x05060707 {
		t.Errorf(
			"the held record's session sequence is %#x, want the sequence ID less one, 0x05060707",
			rec.NFS4.SessionSeq,
		)
	}

	if held.Info.Boot != boot2Minor1 {
		t.Errorf("the held Boot is %x, want %x", held.Info.Boot, boot2Minor1)
	}
}

// An owner confirmed already, asking again with its own verifier and
// principal, is answered its client ID with CONFIRMED_R and the stored
// session sequence plus one, and no proposal is held (RFC 8881 section
// 18.35.4, item 3).
func TestExchangeIDConfirmedSame(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	id := seed(t, rn, plain(rn), "client-a", boot2Minor1, recBytes(t, 1, 7))

	got := e.do(t, plain(rn), golden(t, "rfc-nfs4-exchange-id-args"))
	wantStatus(t, got, statusOK)

	want := patchID(golden(t, "rfc-nfs4-exchange-id-confirmed"), id)
	if !bytes.Equal(got, want) {
		t.Errorf("the result is %x, want %x", got, want)
	}

	e.nothingHeld(t, id)
}

// The sequence ID wraps at 2^32 as a uint32 does, whether or not the
// request asks to update the confirmed record.
func TestExchangeIDSeqWrap(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	seed(t, rn, plain(rn), "client-a", boot2Minor1, recBytes(t, 1, 0xffffffff))

	for name, flags := range map[string]uint32{"plain": 0, "update": flagUpdConfirmedRecA} {
		got := e.do(t, plain(rn), args("client-a", verifier2, flags, spNone()))
		wantStatus(t, got, statusOK)

		if seq := field(got, offSeq); seq != 0 {
			t.Errorf("%s: the sequence ID is %d, want 0", name, seq)
		}
	}
}

// A second principal asking for a confirmed owner's name is refused
// CLID_INUSE with its own verifier or another, holds no proposal and leaves
// the record as it was (RFC 8881 section 18.35.4, item 6).
func TestExchangeIDOtherPrincipal(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	other := alice(t, rn)
	id := seed(t, rn, plain(rn), "client-a", boot2Minor1, recBytes(t, 1, 7))

	for name, v := range map[string][8]byte{
		"same verifier":    verifier2,
		"another verifier": verifier1,
	} {
		got := e.do(t, other, args("client-a", v, 0, spNone()))
		wantStatus(t, got, statusInUse)
		e.nothingHeld(t, id)

		rec, err := rn.Mem.Record(t.Context(), rn.Mem.Admin(), id)
		if err != nil || rec.Boot != boot2Minor1 {
			t.Errorf("%s: the record is %+v, %v; want it unchanged", name, rec, err)
		}
	}
}

// A confirmed owner asking with another verifier is a reboot: a new client
// ID under a new proposal, no CONFIRMED_R, and the old record untouched
// until the confirm expires it.
func TestExchangeIDNewVerifier(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	id := seed(t, rn, plain(rn), "client-a", boot1Minor1, recBytes(t, 1, 7))

	got := e.do(t, plain(rn), args("client-a", verifier2, 0, spNone()))
	wantStatus(t, got, statusOK)

	if clientID(got) == id {
		t.Errorf("a new verifier was answered the old client ID %d", id)
	}

	if field(got, offFlags) != flagsNonPNFS {
		t.Errorf("the flags are %#x, want %#x", field(got, offFlags), flagsNonPNFS)
	}

	if field(got, offSeq) != 0x05060708 {
		t.Errorf("the sequence ID is %#x, want 0x05060708", field(got, offSeq))
	}

	rec, err := rn.Mem.Record(t.Context(), rn.Mem.Admin(), id)
	if err != nil || rec.Boot != boot1Minor1 {
		t.Errorf("the old record is %+v, %v; want it unchanged", rec, err)
	}
}

// One record kind serves both minor versions: a record confirmed over
// NFSv4.0 is not the same boot as the same verifier over NFSv4.1, so that
// EXCHANGE_ID is a new client, while the same verifier twice over NFSv4.1 is
// the same one (RFC 21 section 3.5).
func TestExchangeIDNFS40Record(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	old := seed(t, rn, plain(rn), "client-a", boot1Minor0, recBytes(t, 0, 0))

	got := e.do(t, plain(rn), args("client-a", verifier1, 0, spNone()))
	wantStatus(t, got, statusOK)

	id := clientID(got)
	if id == old || field(got, offFlags) != flagsNonPNFS {
		t.Errorf(
			"a v4.0 record's verifier over v4.1 was answered client %d flags %#x, "+
				"want a new client and %#x",
			id,
			field(got, offFlags),
			flagsNonPNFS,
		)
	}

	held, ok := e.sessions.Confirm(id)
	if !ok || held.Info.Boot != boot1Minor1 {
		t.Errorf("the held proposal is %+v, %v; want Boot %x", held, ok, boot1Minor1)
	}

	rec, err := rn.Mem.Record(t.Context(), rn.Mem.Admin(), old)
	if err != nil || rec.Boot != boot1Minor0 {
		t.Errorf("the v4.0 record is %+v, %v; want it unchanged", rec, err)
	}

	same := seed(t, rn, plain(rn), "client-b", boot1Minor1, recBytes(t, 1, 3))
	got = e.do(t, plain(rn), args("client-b", verifier1, 0, spNone()))
	wantStatus(t, got, statusOK)

	if clientID(got) != same || field(got, offFlags) != flagsConfirmed {
		t.Errorf(
			"the same v4.1 verifier was answered client %d flags %#x, want %d and %#x",
			clientID(got),
			field(got, offFlags),
			same,
			uint32(flagsConfirmed),
		)
	}

	got = e.do(t, plain(rn), args("client-b", [8]byte{1, 2, 3, 4, 5, 6, 7, 9}, 0, spNone()))
	wantStatus(t, got, statusOK)

	if clientID(got) == same {
		t.Errorf("a verifier with its last byte changed was answered the old client %d", same)
	}
}

// Every node of the installation answers one scope and one major ID, its own
// minor ID apart, and sets USE_NON_PNFS and neither pNFS bit.
func TestExchangeIDFlagsAndScope(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	a, b := newExchange(t, rn, 7), newExchange(t, rn, 9)

	ra := a.do(t, plain(rn), args("client-a", verifier1, 0, spNone()))
	rb := b.do(t, plain(rn), args("client-b", verifier1, 0, spNone()))
	wantStatus(t, ra, statusOK)
	wantStatus(t, rb, statusOK)

	for name, got := range map[string][]byte{"node 7": ra, "node 9": rb} {
		if f := field(got, offFlags); f&flagsNonPNFS == 0 || f&flagsPNFSBits != 0 {
			t.Errorf("%s: the flags are %#x, want USE_NON_PNFS and no pNFS bit", name, f)
		}

		if !bytes.Equal(got[offMajor+4:offMajor+4+installationSize], installation[:]) {
			t.Errorf(
				"%s: the major ID is %x, want the installation %x",
				name,
				got[offMajor+4:offMajor+4+installationSize],
				installation,
			)
		}

		if !bytes.Equal(got[offScope+4:offScope+4+installationSize], installation[:]) {
			t.Errorf(
				"%s: the scope is %x, want the installation %x",
				name,
				got[offScope+4:offScope+4+installationSize],
				installation,
			)
		}
	}

	m7 := binary.BigEndian.Uint64(ra[offMinor:])
	m9 := binary.BigEndian.Uint64(rb[offMinor:])

	// The minor ID is the first eight bytes of the SHA-256 of the node's ID,
	// worked out here from the bytes, not read back from the handler.
	for name, c := range map[string]struct {
		got  uint64
		node byte
	}{"node 7": {m7, 7}, "node 9": {m9, 9}} {
		sum := sha256.Sum256(append([]byte{c.node}, make([]byte, 15)...))
		if want := binary.BigEndian.Uint64(sum[:8]); c.got != want {
			t.Errorf("%s: the minor ID is %#x, want %#x", name, c.got, want)
		}
	}

	if m7 == m9 {
		t.Errorf("two nodes answered the minor ID %#x", m7)
	}
}

// An eia_flags with a bit RFC 8881 section 18.35.1 does not define, or with
// CONFIRMED_R, which only eir_flags carries, is INVAL (section 18.35.3);
// every bit a client may set is accepted.
func TestExchangeIDUndefinedFlags(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)

	for name, flags := range map[string]uint32{
		"bit 2":                        0x00000004,
		"bit 3":                        0x00000008,
		"a bit above the pNFS":         0x00080000,
		"a bit under the update":       0x20000000,
		"CONFIRMED_R":                  flagsConfirmed &^ flagsNonPNFS,
		"USE_NON_PNFS and CONFIRMED_R": flagsConfirmed,
	} {
		got := e.do(t, plain(rn), args("client-"+name, verifier1, flags, spNone()))
		wantStatus(t, got, statusInval)
	}

	// The bits a client may set: SUPP_MOVED_REFER, SUPP_MOVED_MIGR,
	// BIND_PRINC_STATEID and the pNFS mask.
	const defined = 0x00000103 | 0x00070000

	got := e.do(t, plain(rn), args("client-defined", verifier1, defined, spNone()))
	wantStatus(t, got, statusOK)
}

// SP4_SSV is refused ENCR_ALG_UNSUPP, SP4_MACH_CRED without a ticket under
// integrity or privacy INVAL, and under one a proposal that protects its
// client (RFC 8881 section 18.35.3).
func TestExchangeIDStateProtect(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)

	ssv := words(2, 0, 0, 0, 0, 0, 0, 0, 0) // no bitmaps, no algorithms, window and handles 0
	wantStatus(t, e.do(t, plain(rn), args("client-a", verifier1, 0, ssv)), statusEncrAlg)
	wantStatus(t, e.do(t, plain(rn), args("client-a", verifier1, 0, spMach(nil, nil))), statusInval)
	wantStatus(
		t,
		e.do(
			t,
			protected(rn, vfs.ProtectionNone),
			args("client-a", verifier1, 0, spMach(nil, nil)),
		),
		statusInval,
	)

	for name, p := range map[string]vfs.Protection{
		"integrity": vfs.ProtectionIntegrity,
		"privacy":   vfs.ProtectionPrivacy,
	} {
		got := e.do(t, protected(rn, p), args("client-"+name, verifier1, 0, spMach(nil, nil)))
		wantStatus(t, got, statusOK)

		held, ok := e.sessions.Confirm(clientID(got))
		if !ok || !held.Info.Protect {
			t.Errorf("%s: the held proposal is %+v, %v; want one with Protect set", name, held, ok)
		}
	}
}

// With UPD_CONFIRMED_REC_A, an unknown owner is NOENT, another verifier
// NOT_SAME whoever asks, and another principal under the record's verifier
// PERM, none leaving a proposal; the owner's own verifier and principal are
// answered its client ID and the next sequence ID (RFC 8881 section 18.35.4,
// items 6 to 9; RFC 21 section 3.5).
func TestExchangeIDUpdConfirmed(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	other := alice(t, rn)
	wantStatus(
		t,
		e.do(t, plain(rn), args("client-a", verifier2, flagUpdConfirmedRecA, spNone())),
		statusNoEnt,
	)

	id := seed(t, rn, plain(rn), "client-a", boot2Minor1, recBytes(t, 1, 7))
	wantStatus(
		t,
		e.do(t, other, args("client-a", verifier2, flagUpdConfirmedRecA, spNone())),
		statusPerm,
	)
	e.nothingHeld(t, id)
	e.noPending(t, id)

	for _, call := range []vfs.Call{plain(rn), other} {
		wantStatus(
			t,
			e.do(t, call, args("client-a", verifier1, flagUpdConfirmedRecA, spNone())),
			statusNotSame,
		)
		e.nothingHeld(t, id)
		e.noPending(t, id)
	}

	got := e.do(t, plain(rn), args("client-a", verifier2, flagUpdConfirmedRecA, spNone()))
	wantStatus(t, got, statusOK)

	if clientID(got) != id || field(got, offSeq) != 8 {
		t.Errorf(
			"an update of the owner's own record was answered client %d sequence %d, want %d and 8",
			clientID(got),
			field(got, offSeq),
			id,
		)
	}

	e.nothingHeld(t, id)
	e.noPending(t, id)
}

// A confirmed owner under a machine credential, asked by a caller that does
// not prove one, is another principal, whatever the flag: CLID_INUSE, or
// PERM under UPD_CONFIRMED_REC_A. The new verifier has its own test below.
func TestExchangeIDProtectedOwner(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	info := vfs.ClientInfo{
		Protocol: vfs.ProtocolNFSv4,
		Name:     []byte("client-a"),
		Boot:     boot2Minor1,
		Protect:  true,
		Adapter:  recBytes(t, 1, 7),
		Recovery: recoverStatus,
	}
	call := protected(rn, vfs.ProtectionIntegrity)

	p, err := rn.Mem.Propose(t.Context(), call, info)
	if err != nil {
		t.Fatal(err)
	}

	info.Proposal = &p

	id, _, err := rn.Mem.Connect(t.Context(), call, info, &vfstest.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(t, e.do(t, plain(rn), args("client-a", verifier2, 0, spNone())), statusInUse)
	wantStatus(
		t,
		e.do(t, plain(rn), args("client-a", verifier2, flagUpdConfirmedRecA, spNone())),
		statusPerm,
	)
	e.nothingHeld(t, id)
	e.noPending(t, id)

	got := e.do(t, call, args("client-a", verifier2, 0, spNone()))
	wantStatus(t, got, statusOK)

	if clientID(got) != id {
		t.Errorf(
			"the owner under its credential was answered client %d, want %d",
			clientID(got),
			id,
		)
	}
}

// A confirmed owner under a machine credential, asked with a new verifier by
// a caller of the same principal that proves no machine credential, is not
// replaced: CLID_INUSE, or NOT_SAME under UPD_CONFIRMED_REC_A (the verifier
// differs, which is answered before the principal), no proposal is held and
// the stored record is untouched. The owner under its credential still
// replaces it.
func TestExchangeIDProtectedOwnerNewVerifier(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	info := vfs.ClientInfo{
		Protocol: vfs.ProtocolNFSv4,
		Name:     []byte("client-a"),
		Boot:     boot1Minor1,
		Protect:  true,
		Adapter:  recBytes(t, 1, 7),
		Recovery: recoverStatus,
	}
	call := protected(rn, vfs.ProtectionIntegrity)

	p, err := rn.Mem.Propose(t.Context(), call, info)
	if err != nil {
		t.Fatal(err)
	}

	info.Proposal = &p

	id, _, err := rn.Mem.Connect(t.Context(), call, info, &vfstest.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}

	wantStatus(t, e.do(t, plain(rn), args("client-a", verifier2, 0, spNone())), statusInUse)
	wantStatus(
		t,
		e.do(t, plain(rn), args("client-a", verifier2, flagUpdConfirmedRecA, spNone())),
		statusNotSame,
	)
	e.nothingHeld(t, id)
	e.noPending(t, id)

	rec, err := rn.Mem.Record(t.Context(), call, id)
	if err != nil || rec.Boot != boot1Minor1 {
		t.Errorf("the stored record is %+v (err %v), want boot %x kept", rec, err, boot1Minor1)
	}

	got := e.do(t, call, args("client-a", verifier2, 0, spNone()))
	wantStatus(t, got, statusOK)

	if clientID(got) == id {
		t.Errorf("the owner's new verifier was answered the old client ID %d", id)
	}
}

// The two bitmaps of SP4_MACH_CRED are kept in the proposal's record bytes as
// sent, and echoed in the reply with CREATE_SESSION added to the operations
// to enforce.
func TestExchangeIDMustAllowKept(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	enforce, allow := []uint32{0x00000001, 0, 0x00000800}, []uint32{0x40000000}

	got := e.do(
		t,
		protected(rn, vfs.ProtectionPrivacy),
		args("client-a", verifier1, 0, spMach(enforce, allow)),
	)
	wantStatus(t, got, statusOK)

	held, ok := e.sessions.Confirm(clientID(got))
	if !ok {
		t.Fatal("the table holds no proposal")
	}

	rec, err := wire.DecodeClientRec(held.Info.Adapter)
	if err != nil {
		t.Fatal(err)
	}

	if !equal(rec.NFS4.MustEnforce, enforce) || !equal(rec.NFS4.MustAllow, allow) {
		t.Errorf(
			"the record holds %v and %v, want %v and %v",
			rec.NFS4.MustEnforce,
			rec.NFS4.MustAllow,
			enforce,
			allow,
		)
	}

	wantEcho := words(1, 3, 1, 0x800, 0x800, 1, 0x40000000)
	if !bytes.Equal(got[offProtect:offProtect+len(wantEcho)], wantEcho) {
		t.Errorf(
			"the protection echoed is %x, want %x",
			got[offProtect:offProtect+len(wantEcho)],
			wantEcho,
		)
	}
}

// The reply names CREATE_SESSION among the operations to enforce, whatever
// list the client sent, since CREATE_SESSION refuses a caller that is not the
// owner's proved machine credential either way (RFC 8881 section 18.35.3 lets
// the reply differ from the request).
func TestExchangeIDEnforcesCreateSession(t *testing.T) {
	cases := []struct {
		name    string
		enforce []uint32
		want    []byte
	}{
		{"none sent", nil, words(1, 2, 0, 0x800, 0)},
		{"one word", []uint32{1}, words(1, 2, 1, 0x800, 0)},
		{"already named", []uint32{0, 0x800}, words(1, 2, 0, 0x800, 0)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rn := adaptertest.NewRunner(t)
			e := newExchange(t, rn, 7)

			got := e.do(
				t,
				protected(rn, vfs.ProtectionPrivacy),
				args("client-a", verifier1, 0, spMach(c.enforce, nil)),
			)
			wantStatus(t, got, statusOK)

			if !bytes.Equal(got[offProtect:offProtect+len(c.want)], c.want) {
				t.Errorf(
					"the protection echoed is %x, want %x",
					got[offProtect:offProtect+len(c.want)],
					c.want,
				)
			}
		})
	}
}

func equal(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// A record whose Adapter bytes are of a version this node does not read is
// answered the fault, and neither rewritten nor answered as a new owner.
func TestExchangeIDVersion2Record(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	future := []byte{2, 0, 0, 0, 2}
	id := seed(t, rn, plain(rn), "client-a", boot2Minor1, future)

	wantStatus(t, e.do(t, plain(rn), args("client-a", verifier2, 0, spNone())), statusFault)
	e.nothingHeld(t, id)

	rec, err := rn.Mem.Record(t.Context(), rn.Mem.Admin(), id)
	if err != nil || !bytes.Equal(rec.Adapter, future) {
		t.Errorf("the record's bytes are %x, %v; want %x unchanged", rec.Adapter, err, future)
	}
}

// The errors EXCHANGE_ID raises map to their statuses, and the row takes no
// part in a drain (RFC 21 section 11.5).
func TestExchangeIDErrorRows(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	op := e.members.Ops[opExchangeID]

	if op.Role != core.RoleNone || op.Versions != (core.Range{First: 1, Last: lastMinor}) {
		t.Errorf(
			"the row's role is %v and versions %v, want none and 1 to %d",
			op.Role,
			op.Versions,
			lastMinor,
		)
	}

	for name, c := range map[string]struct {
		err  error
		want uint32
	}{
		"another principal": {&vfs.StateError{Kind: vfs.StateClientInUse}, statusInUse},
		"pool full":         {&vfs.Delay{Cause: vfs.DelayResources}, statusDelay},
	} {
		if s, _, _ := e.members.Errors.Map(c.err, op, 1); uint32(s) != c.want {
			t.Errorf("%s: status %d, want %d", name, s, c.want)
		}
	}
}

// The status of recoverStatus is open state's, by the name open state gives it.
func TestRecoverStatusIsOpenStates(t *testing.T) {
	if got := recoverStatus.String(); got != "status" {
		t.Errorf("recoverStatus is named %q, want open state's \"status\"", got)
	}
}

// Nothing the handler decided is in one node's memory: a node that starts
// over, its table empty, answers a confirmed owner as before.
func TestExchangeIDRestart(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	first := newExchange(t, rn, 7)
	id := seed(t, rn, plain(rn), "client-a", boot2Minor1, recBytes(t, 1, 7))
	a := golden(t, "rfc-nfs4-exchange-id-args")

	want := first.do(t, plain(rn), a)
	got := newExchange(t, rn, 7).do(t, plain(rn), a)
	wantStatus(t, got, statusOK)

	if !bytes.Equal(got, want) || clientID(got) != id || field(got, offFlags) != flagsConfirmed {
		t.Errorf("a node that restarted answered %x, want %x", got, want)
	}
}

// The name a proposal holds is the handler's own copy: the request's buffer
// is reused once the call returns (RFC 20 section 5.3), so a name aliasing it
// would change under the held proposal.
func TestExchangeIDKeepsItsOwnName(t *testing.T) {
	rn := adaptertest.NewRunner(t)
	e := newExchange(t, rn, 7)
	a := golden(t, "rfc-nfs4-exchange-id-args")

	got := e.do(t, plain(rn), a)
	wantStatus(t, got, statusOK)

	for i := range a {
		a[i] = 0xff
	}

	held, ok := e.sessions.Confirm(clientID(got))
	if !ok {
		t.Fatal("the table holds no proposal")
	}

	if string(held.Info.Name) != "client-a" {
		t.Errorf(
			"the held name is %q after the request's buffer was reused, want %q",
			held.Info.Name,
			"client-a",
		)
	}
}

// Every EXCHANGE_ID reads the owner with ClientByName before it proposes
// (RFC 21 section 3.5), with or without UPD_CONFIRMED_REC_A: a failure of that
// read is the answer, where a Propose alone would have succeeded.
func TestExchangeIDReadsTheOwnerBeforeItProposes(t *testing.T) {
	for name, flags := range map[string]uint32{"new": 0, "update": flagUpdConfirmedRecA} {
		t.Run(name, func(t *testing.T) {
			rn := adaptertest.NewRunner(t)
			e := newExchange(t, rn, 7)
			rn.Faults.Fail(vfs.OpClientByName, &vfs.StateError{Kind: vfs.StateStaleClient})

			wantStatus(
				t,
				e.do(t, plain(rn), args("client-a", verifier1, flags, spNone())),
				statusStaleClient,
			)
		})
	}
}
