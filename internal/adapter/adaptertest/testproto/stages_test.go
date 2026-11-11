package testproto

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/pkg/resources"
)

// unwrapAll unwraps every message of a record's body in order, each with the
// one before it as prev when it is related, as the pipeline does.
func unwrapAll(t *testing.T, body []byte) ([]*header, error) {
	t.Helper()
	rec := &resources.Buf{B: body}
	var out []*header
	var prev core.Header
	for off := 0; ; {
		h, err := headerCodec{}.Decode(rec, off)
		if err != nil {
			t.Fatal(err)
		}
		var before core.Header
		if h.Related() {
			before = prev
		}
		u, err := security{}.Unwrap(h, rec, before)
		if err != nil {
			return out, err
		}
		out = append(out, u.(*header))
		prev = u
		if off = h.Next(); off == 0 {
			return out, nil
		}
	}
}

// Unwrap opens a sealed chain: each member's arguments read clear, and a
// member that inherited its session carries the one it resolved.
func TestUnwrapOpensAChainAndResolvesAnInheritedSession(t *testing.T) {
	body := bytes.Clone(chainVector[lengthLen:])
	hs, err := unwrapAll(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 {
		t.Fatalf("unwrapped %d members, want 2", len(hs))
	}
	if got := string(arguments(body, 0, hs[0])); got != "a" {
		t.Errorf("the Create's name reads %q after Unwrap, want \"a\"", got)
	}
	four := bytes.Clone(nameVector[lengthLen:])
	if named, err := unwrapAll(t, four); err != nil || string(arguments(four, 0, named[0])) != "name" {
		t.Errorf("the four-byte name reads %q after Unwrap, %v", four[headerLen:], err)
	}
	if got := arguments(body, 41, hs[1]); !bytes.Equal(got, []byte{0}) {
		t.Errorf("the Echo's role byte reads % x after Unwrap, want 00", got)
	}
	if hs[0].session() != 5 || hs[1].session() != 5 {
		t.Errorf("sessions after Unwrap: %d and %d, want 5 for both", hs[0].session(), hs[1].session())
	}

	clear := bytes.Clone(echoVector[lengthLen:])
	if _, err := unwrapAll(t, clear); err != nil || !bytes.Equal(clear, echoVector[lengthLen:]) {
		t.Errorf("a message under no session: %v, bytes changed: %v", err, !bytes.Equal(clear, echoVector[lengthLen:]))
	}
}

// A message with any one bit of a byte changed is refused. A session
// rewritten to 0 is not: under session 0 a message is not sealed, and reads
// as it was sent.
func TestUnwrapRefusesAChangedByte(t *testing.T) {
	sealed := Append(nil, Msg{Op: OpCreate, Version: 2, ID: 9, Session: 5, Tree: 1, Credits: 1, Name: "name"})[lengthLen:]
	for i := range sealed {
		if i < 4 {
			continue // next decides which message is read, and a changed one is refused by Decode
		}
		body := bytes.Clone(sealed)
		body[i] ^= 0x01
		if _, err := unwrapAll(t, body); !errors.Is(err, errChecksum) {
			t.Fatalf("byte %d changed: Unwrap = %v, want a checksum refusal", i, err)
		}
	}
	if _, err := unwrapAll(t, bytes.Clone(sealed)); err != nil {
		t.Fatalf("the unchanged message: %v", err)
	}
}

// A related member with an all-ones session is checked under the session of
// the member before it, and refused with none before it.
func TestUnwrapChecksARelatedMemberUnderThePreviousSession(t *testing.T) {
	body := bytes.Clone(chainVector[lengthLen:])
	rec := &resources.Buf{B: body}
	second, err := headerCodec{}.Decode(rec, 41)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (security{}).Unwrap(second, rec, nil); !errors.Is(err, errNoPrev) {
		t.Errorf("an inheriting member with no member before it: Unwrap = %v, want a refusal", err)
	}
	if _, err := (security{}).Unwrap(second, rec, second); !errors.Is(err, errNoPrev) {
		t.Errorf("an inheriting member after one whose session was never resolved: Unwrap = %v, want a refusal", err)
	}

	// Under another session's member before it, the checksum no longer holds.
	other := bytes.Clone(Append(nil, Msg{Op: OpEcho, Version: 2, Session: 6})[lengthLen:])
	orec := &resources.Buf{B: other}
	oh, err := headerCodec{}.Decode(orec, 0)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := security{}.Unwrap(oh, orec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (security{}).Unwrap(second, rec, prev); !errors.Is(err, errChecksum) {
		t.Errorf("an inheriting member checked under session 6: Unwrap = %v, want a checksum refusal", err)
	}
}

// reply encodes the reply to the request in body with the given body bytes,
// as the pipeline would before Wrap.
func reply(t *testing.T, body []byte, payload string) *resources.Buf {
	t.Helper()
	hs, err := unwrapAll(t, body)
	if err != nil {
		t.Fatal(err)
	}
	out := &resources.Buf{}
	if err := (headerCodec{}).Encode(hs[0], StatusOK, out); err != nil {
		t.Fatal(err)
	}
	out.B = append(out.B, payload...)
	return out
}

// A reply wrapped once opens to its body at the client; wrapped twice it does
// not; under no session it is left as it was.
func TestWrapSealsAReplyOnce(t *testing.T) {
	request := Append(nil, Msg{Op: OpEcho, Version: 2, ID: 3, Session: 5, Credits: 1, Payload: []byte("x")})[lengthLen:]

	out := reply(t, bytes.Clone(request), "reply-body")
	if err := (security{}).Wrap(nil, out); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.B, []byte("reply-body")) {
		t.Error("the wrapped reply carries its body in clear")
	}
	once := bytes.Clone(out.B)
	replies, err := openReplies(out.B)
	if err != nil || len(replies) != 1 || string(replies[0].Body) != "reply-body" || replies[0].ID != 3 {
		t.Fatalf("the reply wrapped once opens to %+v, %v", replies, err)
	}

	twice := &resources.Buf{B: bytes.Clone(once)}
	if err := (security{}).Wrap(nil, twice); err != nil {
		t.Fatal(err)
	}
	if replies, err := openReplies(twice.B); err == nil && string(replies[0].Body) == "reply-body" {
		t.Error("a reply wrapped twice still opens to its body")
	}

	// A chain of two replies is sealed in one pass, each under its session.
	chain := reply(t, bytes.Clone(request), "first-body")
	if err := (headerCodec{}).Chain(chain, 0); err != nil {
		t.Fatal(err)
	}
	second := reply(t, bytes.Clone(request), "second-body")
	chain.B = append(chain.B, second.B...)
	if err := (security{}).Wrap(nil, chain); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(chain.B, []byte("first-body")) || bytes.Contains(chain.B, []byte("second-body")) {
		t.Error("a wrapped chain carries a body in clear")
	}
	if replies, err := openReplies(chain.B); err != nil || len(replies) != 2 || string(replies[0].Body) != "first-body" || string(replies[1].Body) != "second-body" {
		t.Errorf("the wrapped chain opens to %+v, %v", replies, err)
	}

	plain := reply(t, bytes.Clone(echoVector[lengthLen:]), "reply-body")
	before := bytes.Clone(plain.B)
	if err := (security{}).Wrap(nil, plain); err != nil || !bytes.Equal(plain.B, before) {
		t.Errorf("a reply under no session: Wrap = %v, bytes changed: %v", err, !bytes.Equal(plain.B, before))
	}
}

// A changed byte in a sealed reply is refused by the client.
func TestClientRefusesAChangedReply(t *testing.T) {
	request := Append(nil, Msg{Op: OpEcho, Version: 2, ID: 3, Session: 5, Credits: 1})[lengthLen:]
	out := reply(t, bytes.Clone(request), "reply-body")
	if err := (security{}).Wrap(nil, out); err != nil {
		t.Fatal(err)
	}
	out.B[len(out.B)-1] ^= 0x01
	if _, err := openReplies(out.B); !errors.Is(err, errChecksum) {
		t.Errorf("a reply with its last byte changed opens with %v, want a checksum refusal", err)
	}
}

// headerOf is the first header of the record m encodes to.
func headerOf(t *testing.T, m Msg) *header {
	t.Helper()
	h, err := headerCodec{}.Decode(&resources.Buf{B: Append(nil, m)[lengthLen:]}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return h.(*header)
}

// Admit charges what a message carries and refuses one that would overdraw,
// then admits it once a reply's grant arrives; a grant stops at the target.
func TestFlowChargesRefusesAnOverdrawAndTakesAGrant(t *testing.T) {
	f := newFlow()
	two := headerOf(t, Msg{Op: OpEcho, Version: 2, Credits: 2})
	if _, err := f.Admit(t.Context(), two); !errors.Is(err, errOverdraw) {
		t.Fatalf("a charge of 2 against the opening credit: Admit = %v, want a refusal", err)
	}
	one := headerOf(t, Msg{Op: OpEcho, Version: 2, Credits: 1})
	release, err := f.Admit(t.Context(), one)
	if err != nil || release == nil {
		t.Fatalf("a charge of 1 against the opening credit: Admit = %v", err)
	}
	if _, err := f.Admit(t.Context(), one); !errors.Is(err, errOverdraw) {
		t.Errorf("a second charge of 1 with nothing left: Admit = %v, want a refusal", err)
	}
	if got := f.grant(2); got != 2 {
		t.Fatalf("a grant of 2 added %d", got)
	}
	if _, err := f.Admit(t.Context(), two); err != nil {
		t.Errorf("the charge of 2 once a grant of 2 arrived: Admit = %v", err)
	}
	if got := f.grant(200); got != creditTarget {
		t.Errorf("a grant of 200 on an empty balance added %d, want the target of %d", got, creditTarget)
	}
	if got := f.grant(1); got != 0 {
		t.Errorf("a grant at the target added %d, want 0", got)
	}
}

// A reply's grant goes through the connection's credits: it is counted, and
// what the target leaves no room for is not granted.
func TestEncodeGrantsThroughTheConnectionsCredits(t *testing.T) {
	f := newFlow()
	codec := headerCodec{flow: f}
	req := headerOf(t, Msg{Op: OpEcho, Version: 2, ID: 4, Credits: 3})
	out := &resources.Buf{}
	if err := codec.Encode(req, StatusOK, out); err != nil {
		t.Fatal(err)
	}
	if got := decodeAll(t, out.B)[0].credits; got != 3 {
		t.Errorf("the reply grants %d, want the 3 its request charged", got)
	}
	if _, err := f.Admit(t.Context(), headerOf(t, Msg{Op: OpEcho, Version: 2, Credits: 4})); err != nil {
		t.Errorf("a charge of 4 after a grant of 3 on the opening credit: %v", err)
	}
	f.grant(creditTarget)
	out.B = out.B[:0]
	if err := codec.Encode(req, StatusOK, out); err != nil {
		t.Fatal(err)
	}
	if got := decodeAll(t, out.B)[0].credits; got != 0 {
		t.Errorf("a reply at the credit target grants %d, want 0", got)
	}
}

func TestHoldKeyIsNeverZero(t *testing.T) {
	var s hold
	for _, id := range []uint64{0, 1, 1<<63 - 1} {
		key := s.Key(headerOf(t, Msg{Op: OpCreate, Version: 2, ID: id}))
		if key == 0 || key != id|1<<63 {
			t.Errorf("Key of message %d = %#x, want its ID with the top bit set", id, key)
		}
	}
}

// Interim grants the request's credits with grant and one without, adds the
// credits of the members after, and stops at the target.
func TestInterimGrants(t *testing.T) {
	req := Msg{Op: OpCreate, Version: 2, ID: 6, Session: 0, Credits: 5}
	after := func(credits ...uint16) []core.Header {
		var hs []core.Header
		for _, c := range credits {
			hs = append(hs, headerOf(t, Msg{Op: OpEcho, Version: 2, Credits: c}))
		}
		return hs
	}
	// The request is a sealed, related, pending member with a member after
	// it, so its header carries a next, flags and a checksum the interim must
	// not copy.
	chain := Append(nil, Msg{Op: OpEcho, Version: 2, Session: 5}, Msg{Op: OpCreate, Version: 2, ID: 6, Session: inherited, Credits: 5, Related: true, Pend: true}, Msg{Op: OpEcho, Version: 2, Session: inherited, Related: true})[lengthLen:]
	hs, err := unwrapAll(t, chain)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		balance int // the connection's credits before
		grant   bool
		after   []core.Header
		want    uint16 // granted on the wire
		left    int    // the connection's credits after
	}{
		{"with grant", 10, true, nil, 5, 15},
		{"without grant", 10, false, nil, 1, 11},
		{"with the members after, which their client charged", 10, false, after(2, 3), 6, 11},
		{"with grant and members after", 10, true, after(2, 3), 10, 15},
		{"a sum past the target", 0, true, after(40, 40), creditTarget, creditTarget},
		{"a sum past 16 bits", 0, true, after(65535, 1), creditTarget, creditTarget},
		{"near the target", 62, true, nil, 2, creditTarget},
	} {
		f := &flow{balance: c.balance}
		s := hold{flow: f}
		out := &resources.Buf{B: []byte("before")}
		if err := s.Interim(hs[1], c.grant, c.after, out); err != nil {
			t.Fatal(err)
		}
		got := decodeAll(t, out.B[len("before"):])
		want := fields{0, flagReply | flagInterim, 2, c.want, OpCreate, 6 | 1<<63, 5, 0, 0, 0, 0, ""}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: the interim is\n got %+v\nwant %+v", c.name, got, want)
		}
		if f.balance != c.left {
			t.Errorf("%s: the connection holds %d credits after, want %d", c.name, f.balance, c.left)
		}
	}
	if _ = req; hs[1].credits() != 5 || hs[1].id() != 6 {
		t.Error("Interim changed the request's own header")
	}
}

// Final's header carries the key, and grants the rest of the request's
// credits only when its interim withheld them.
func TestFinalGrantsWhatTheInterimWithheld(t *testing.T) {
	var s hold
	req := headerOf(t, Msg{Op: OpCreate, Version: 2, ID: 6, Credits: 5, Pend: true})
	for _, c := range []struct {
		grant bool
		want  uint16
	}{{true, 4}, {false, 0}} {
		out := &resources.Buf{}
		if err := (headerCodec{}).Encode(s.Final(req, c.grant), StatusDelay, out); err != nil {
			t.Fatal(err)
		}
		got := decodeAll(t, out.B)[0]
		if got.credits != c.want || got.id != 6|1<<63 || got.flags != flagReply || core.Status(got.status) != StatusDelay {
			t.Errorf("the final reply with grant %v is %+v; want %d credits, the key as its ID and the reply flag alone", c.grant, got, c.want)
		}
	}
	if req.credits() != 5 || req.id() != 6 {
		t.Error("Final changed the request's own header")
	}

	// The final reply's grant reaches the connection's credits, and the
	// other stages read Final's header as the request's: the same key, the
	// same pend flag, the same cache slot.
	f := &flow{balance: 10}
	counted := &resources.Buf{}
	fin := s.Final(req, true)
	if err := (headerCodec{flow: f}).Encode(fin, StatusOK, counted); err != nil {
		t.Fatal(err)
	}
	if f.balance != 14 {
		t.Errorf("after a final reply granting 4 the connection holds %d credits, want 14", f.balance)
	}
	if s.Key(fin) != s.Key(req) || !s.Pends(fin, nil) {
		t.Errorf("Final's header has key %#x and pends %v; the request's has %#x and pends", s.Key(fin), s.Pends(fin, nil), s.Key(req))
	}
	r := &replay{m: map[replayKey][]byte{}}
	r.Store(fin, counted)
	if len(r.m) != 1 {
		t.Error("a reply stored under Final's header was dropped")
	}
	free := headerOf(t, Msg{Op: OpCreate, Version: 2, ID: 6})
	out := &resources.Buf{}
	if err := (headerCodec{}).Encode(s.Final(free, true), StatusOK, out); err != nil {
		t.Fatal(err)
	}
	if got := decodeAll(t, out.B)[0].credits; got != 0 {
		t.Errorf("the final reply to a request that charged nothing grants %d, want 0", got)
	}
}

func TestCancelsAndPends(t *testing.T) {
	var s hold
	if key, ok := s.Cancels(headerOf(t, Msg{Op: OpCancel, Version: 2, Target: 9})); !ok || key != 9|1<<63 {
		t.Errorf("Cancels of a Cancel naming message 9 = %#x, %v; want that request's key", key, ok)
	}
	if _, ok := s.Cancels(headerOf(t, Msg{Op: OpEcho, Version: 2, ID: 9})); ok {
		t.Error("Cancels names a request for an Echo")
	}
	if !s.Pends(headerOf(t, Msg{Op: OpCreate, Version: 2, Pend: true}), nil) || s.Pends(headerOf(t, Msg{Op: OpCreate, Version: 2, Related: true}), nil) {
		t.Error("Pends does not follow the message's pend flag")
	}
}

// The cache keeps its own copy under the session and sequence, and answers
// nothing for a request it has not seen.
func TestReplayStoresItsOwnCopyPerSessionAndSequence(t *testing.T) {
	r := &replay{m: map[replayKey][]byte{}}
	req := headerOf(t, Msg{Op: OpCreate, Version: 1, Seq: 7, Session: 3})
	rep := &resources.Buf{B: []byte("the reply")}
	r.Store(req, rep)
	rep.B[0] = 'X'
	if got := r.m[replayKey{3, 7}]; string(got) != "the reply" {
		t.Errorf("the cache holds %q after the caller changed its reply, want its own copy", got)
	}
	for _, other := range []Msg{{Op: OpCreate, Version: 1, Seq: 8, Session: 3}, {Op: OpCreate, Version: 1, Seq: 7, Session: 2}, {Op: OpCreate, Version: 1, Seq: 7}} {
		if buf, err := r.Lookup(t.Context(), headerOf(t, other)); buf != nil || err != nil {
			t.Errorf("Lookup of sequence %d under session %d = %v, %v; want nothing", other.Seq, other.Session, buf, err)
		}
	}
}
