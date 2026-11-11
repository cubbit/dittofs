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

// A message with any one byte changed is refused, but for the bytes Unwrap
// does not read as the message's own: none.
func TestUnwrapRefusesAChangedByte(t *testing.T) {
	sealed := Append(nil, Msg{Op: OpCreate, Version: 2, ID: 9, Session: 5, Tree: 1, Credits: 1, Name: "name"})[lengthLen:]
	for i := range sealed {
		if i < 4 || (i >= offSession && i < offSession+4) {
			continue // next and the session decide which message and key are read, not what is summed alone
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
