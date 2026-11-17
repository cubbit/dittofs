package wire_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/internal/prof"
)

// readGolden reads testdata/golden/name.hex: its bytes as hex digits, a # to
// the end of its line a comment.
func readGolden(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/golden/" + name + ".hex")
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var digits strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, r := range line {
			if !strings.ContainsRune(" \t\r", r) {
				digits.WriteRune(r)
			}
		}
	}
	if digits.Len()%2 != 0 {
		t.Fatalf("golden %s holds an odd number of hex digits", name)
	}
	b := make([]byte, digits.Len()/2)
	if _, err := hex.Decode(b, []byte(digits.String())); err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return b
}

// body reads a golden record's mark and returns the record's body.
func body(t *testing.T, name string) []byte {
	t.Helper()
	g := readGolden(t, name)
	n, last, err := wire.Fragment(g)
	if err != nil || !last || n != len(g)-4 {
		t.Fatalf("golden %s: mark of %d bytes, last %v, %v", name, n, last, err)
	}
	return g[4:]
}

// A record mark gives its fragment's length and whether it ends the record,
// refusing a mark shorter than four bytes and a length past the cap, 1 MiB and
// its 4 KiB of header room.
func TestFragment(t *testing.T) {
	for _, c := range []struct {
		name string
		n    uint32
		last bool
		ok   bool
	}{
		{"empty", 0, true, true},
		{"4 KiB", 4 << 10, true, true},
		{"1 MiB", 1 << 20, true, true},
		{"the cap", 1052672, true, true},
		{"one past the cap", 1052673, true, false},
		{"a body that does not end the record", 1024, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			// The mark is written by hand: past the cap the writer refuses to
			// state it, which is TestAppendRecordMarkPastTheCapPanics.
			mark := binary.BigEndian.AppendUint32(nil, uint32(int(c.n)))
			if c.last {
				mark[0] |= 0x80
			}
			n, last, err := wire.Fragment(mark)
			if c.ok && (err != nil || n != int(c.n) || last != c.last) {
				t.Errorf("a mark of %d bytes, last %v: %d, %v", c.n, c.last, n, err)
			}
			if !c.ok && err == nil {
				t.Errorf("a mark of %d bytes past the cap is accepted", c.n)
			}
		})
	}
	for n := range 4 {
		if _, _, err := wire.Fragment(make([]byte, n)); err == nil {
			t.Errorf("a mark of %d bytes is accepted", n)
		}
	}
}

// A mark the reader would refuse to state, its body past the cap, panics
// rather than write it; the cap's own mark reads back.
func TestAppendRecordMarkPastTheCapPanics(t *testing.T) {
	if wire.MaxRecord >= 1<<31 {
		t.Fatal("the cap itself would overflow the mark")
	}
	rec := wire.AppendRecordMark(nil, wire.MaxRecord)
	if n, _, err := wire.Fragment(rec); err != nil || n != wire.MaxRecord {
		t.Fatalf("the cap's mark reads back %d (%v), want %d", n, err, wire.MaxRecord)
	}
	// A negative body is as unstatable as one past the cap: the bit states
	// neither.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a negative body's mark is written")
			}
		}()
		wire.AppendRecordMark(nil, -1)
	}()
	defer func() {
		if recover() == nil {
			t.Error("a mark past the cap is written")
		}
	}()
	wire.AppendRecordMark(nil, 1<<31)
}

// A mark written for a body reads back as that body, with its last-fragment
// bit set.
func TestAppendRecordMark(t *testing.T) {
	m := wire.AppendRecordMark(nil, 4096)
	n, last, err := wire.Fragment(m)
	if err != nil || n != 4096 || !last {
		t.Errorf("a mark for 4096 bytes reads %d, last %v, %v", n, last, err)
	}
}

// The captured NULL call and COMPOUND parse to the fields tshark reads in the
// same capture, and the COMPOUND's credential is a slice of the record.
func TestParseCaptured(t *testing.T) {
	t.Run("NULL", func(t *testing.T) {
		rec := body(t, "nfsv4-null-call")
		c, err := wire.ParseCall(rec)
		if err != nil {
			t.Fatal(err)
		}
		fl, cred := c.Credential()
		vfl, verf := c.Verifier()
		if c.XID() != 0x33b50de8 || c.Type() != 0 || c.RPCVersion() != 2 || c.Program() != 100003 ||
			c.Version() != 4 || c.Procedure() != 0 {
			t.Errorf("the NULL call parses as %#x %d %d %d %d %d", c.XID(), c.Type(), c.RPCVersion(), c.Program(), c.Version(), c.Procedure())
		}
		if fl != 0 || len(cred) != 0 || vfl != 0 || len(verf) != 0 {
			t.Errorf("credentials %d/%d, verifier %d/%d", fl, len(cred), vfl, len(verf))
		}
		if _, ok := c.MinorVersion(); ok || c.Tag() != nil {
			t.Errorf("a NULL call carries a COMPOUND's tag or minor version")
		}
	})
	t.Run("COMPOUND", func(t *testing.T) {
		rec := body(t, "nfsv4-compound-call")
		c, err := wire.ParseCall(rec)
		if err != nil {
			t.Fatal(err)
		}
		fl, cred := c.Credential()
		minor, ok := c.MinorVersion()
		if c.XID() != 0x34b50de8 || c.Program() != 100003 || c.Version() != 4 || c.Procedure() != 1 {
			t.Errorf("the COMPOUND parses as %#x %d %d %d", c.XID(), c.Program(), c.Version(), c.Procedure())
		}
		if fl != 1 || len(cred) != 40 || !ok || minor != 0 || len(c.Tag()) != 0 {
			t.Errorf("credential %d/%d, minor %d (%v), tag %d bytes", fl, len(cred), minor, ok, len(c.Tag()))
		}
		// The credential is a slice of the record: changing the record's byte
		// changes the credential's.
		rec[40] = 'X' // the machine name's first byte, past the stamp, its length and the credential's length
		if cred[8] != 'X' {
			t.Error("the credential is a copy of the record's bytes")
		}
	})
}

// ParseCall allocates nothing: the call is a view over the record (RFC 20
// section 5).
func TestParseCallAllocatesNothing(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the allocation count says nothing under the detector")
	}
	for _, name := range []string{"nfsv4-null-call", "nfsv4-compound-call", "mount-v3-null-call", "nfs-v3-getattr-call"} {
		rec := body(t, name)
		if n := testing.AllocsPerRun(1000, func() { _, _ = wire.ParseCall(rec) }); n != 0 {
			t.Errorf("%s: ParseCall allocates %v per call", name, n)
		}
	}
}

// A COMPOUND whose tag is not a multiple of four parses to it, and its minor
// version is read past the tag's padding; the version is 1 so a parse that
// stops at the tag's five bytes reads 0 rather than the same value.
func TestParseShortTagCompound(t *testing.T) {
	c, err := wire.ParseCall(body(t, "nfsv4-compound-short-tag"))
	if err != nil {
		t.Fatal(err)
	}
	minor, ok := c.MinorVersion()
	if string(c.Tag()) != "abcde" || !ok || minor != 1 {
		t.Errorf("the tag is %q, minor %d (%v)", c.Tag(), minor, ok)
	}
}

// An NFS version 3 call has no COMPOUND prefix: the same procedure number is
// not read as one.
func TestParseV3Proc1(t *testing.T) {
	c, err := wire.ParseCall(body(t, "nfs-v3-getattr-call"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Version() != 3 || c.Procedure() != 1 || c.Program() != 100003 {
		t.Errorf("the call parses as %d %d %d", c.Program(), c.Version(), c.Procedure())
	}
	if _, ok := c.MinorVersion(); ok || c.Tag() != nil {
		t.Error("an NFS version 3 call carries a COMPOUND's tag or minor version")
	}
}

// Procedure 1 of version 4 of a program other than NFS reads no COMPOUND
// prefix: the tag and minor version belong to 100003 alone, and a call of any
// other program that carries them is answered as arguments it does not decode.
func TestProcedureOneOfAnotherProgramReadsNoCompound(t *testing.T) {
	b := binary.BigEndian.AppendUint32(nil, 0x11223344)
	for _, v := range []uint32{0, 2, 100005, 4, 1} { // MOUNT version 4, proc 1
		b = binary.BigEndian.AppendUint32(b, v)
	}
	b = append(b, make([]byte, 16)...) // AUTH_NONE credential and verifier
	tag := binary.BigEndian.AppendUint32(nil, 3)
	tag = append(tag, "tag"...)
	tag = append(tag, 0)
	b = append(b, tag...)
	b = binary.BigEndian.AppendUint32(b, 7) // a minor version, never read
	b = binary.BigEndian.AppendUint32(b, 0) // one operation of arguments
	c, err := wire.ParseCall(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.MinorVersion(); ok {
		t.Error("procedure 1 of another program carries a COMPOUND's minor version")
	}
	if c.Tag() != nil {
		t.Error("procedure 1 of another program carries a COMPOUND's tag")
	}
}

// A COMPOUND cut after its tag still reports the tag it read: the minor
// version is the field the record ran out of.
func TestACutCompoundReportsItsTag(t *testing.T) {
	b := binary.BigEndian.AppendUint32(nil, 0x11223344)
	for _, v := range []uint32{0, 2, 100003, 4, 1} {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	b = append(b, make([]byte, 16)...) // AUTH_NONE credential and verifier
	b = binary.BigEndian.AppendUint32(b, 3)
	b = append(b, "tag"...)
	b = append(b, 0) // the tag's padding and no minor version
	c, err := wire.ParseCall(b)
	if err == nil {
		t.Fatal("a COMPOUND cut after its tag parses whole")
	}
	if tag := c.Tag(); string(tag) != "tag" {
		t.Errorf("a COMPOUND cut after its tag reports %q, want the tag it read", tag)
	}
	if _, ok := c.MinorVersion(); ok {
		t.Error("a COMPOUND cut after its tag carries a minor version")
	}

	// A tag the record does not hold, the cut inside it, names no tag at all,
	// as a non-COMPOUND names none: only a tag read whole is one.
	inside := binary.BigEndian.AppendUint32(nil, 0x11223344)
	for _, v := range []uint32{0, 2, 100003, 4, 1} {
		inside = binary.BigEndian.AppendUint32(inside, v)
	}
	inside = append(inside, make([]byte, 16)...)      // AUTH_NONE credential and verifier
	inside = binary.BigEndian.AppendUint32(inside, 4) // a tag of four bytes, never sent
	cutInside, perr := wire.ParseCall(inside)
	if perr == nil {
		t.Fatal("a COMPOUND cut inside its tag parses whole")
	}
	if tag := cutInside.Tag(); tag != nil {
		t.Errorf("a COMPOUND cut inside its tag reports %q, want no tag", tag)
	}
}

// A call the parse did not accept names no credential and no verifier,
// whatever its bytes would have read as: the router refuses it unread (RFC 21
// section 11.5).
func TestAnUnacceptedCallNamesNoCredential(t *testing.T) {
	// An RPC version the parse stops at, before the credential.
	v3 := binary.BigEndian.AppendUint32(nil, 0x11223344)
	for _, v := range []uint32{0, 3, 1, 2, 3, 9, 9, 9} {
		v3 = binary.BigEndian.AppendUint32(v3, v)
	}
	c, err := wire.ParseCall(v3)
	if err != nil {
		t.Fatalf("an RPC version 3 call: %v", err)
	}
	if fl, body := c.Credential(); fl != 0 || body != nil {
		t.Errorf("an unaccepted call names a credential %d of %d bytes", fl, len(body))
	}
	if fl, body := c.Verifier(); fl != 0 || body != nil {
		t.Errorf("an unaccepted call names a verifier %d of %d bytes", fl, len(body))
	}

	// A call whose credential runs past the record.
	past := binary.BigEndian.AppendUint32(nil, 0x11223344)
	for _, v := range []uint32{0, 2, 100003, 4, 0, 1} {
		past = binary.BigEndian.AppendUint32(past, v)
	}
	past = binary.BigEndian.AppendUint32(past, 8) // a credential of 8 bytes, never sent
	c, err = wire.ParseCall(past)
	if err == nil {
		t.Fatal("a credential cut by the record parses")
	}
	if fl, body := c.Credential(); fl != 0 || body != nil {
		t.Errorf("an unaccepted call names a credential %d of %d bytes", fl, len(body))
	}
}

// A record too short to address answers nothing, and reads no transaction ID:
// XID is zero rather than a panic.
func TestARecordTooShortToAddressReadsNoXID(t *testing.T) {
	c, err := wire.ParseCall([]byte{1, 2, 3})
	if err == nil || c != nil {
		t.Fatalf("three bytes parse as %v (%v)", c, err)
	}
	if xid := c.XID(); xid != 0 {
		t.Errorf("a record too short to address reads %#x", xid)
	}
}

// A hand-written NULL call to MOUNT version 3 under AUTH_SYS parses too, with
// no minor version read.
func TestParseMountCall(t *testing.T) {
	c, err := wire.ParseCall(body(t, "mount-v3-null-call"))
	if err != nil {
		t.Fatal(err)
	}
	fl, cred := c.Credential()
	if c.XID() != 0x11223344 || c.Program() != 100005 || c.Version() != 3 || c.Procedure() != 0 {
		t.Errorf("the MOUNT call parses as %#x %d %d %d", c.XID(), c.Program(), c.Version(), c.Procedure())
	}
	if fl != wire.AuthSys || len(cred) != 32 {
		t.Fatalf("the credential is %d/%d bytes, want AUTH_SYS of 32", fl, len(cred))
	}
	a, err := wire.DecodeAuthSys(cred)
	if err != nil {
		t.Fatal(err)
	}
	if a.Stamp != 0x55667788 || string(a.MachineName()) != "host1" || a.UID != 1000 || a.GID != 100 {
		t.Errorf("the MOUNT credential is %+v", a)
	}
	// The accessors callers read: the group list through Groups, not the array.
	if groups := a.Groups(); len(groups) != 1 || groups[0] != 27 {
		t.Errorf("the MOUNT credential's groups are %v (of %d), want 27 alone", groups, a.NGIDs)
	}
	if _, ok := c.MinorVersion(); ok {
		t.Error("a MOUNT call carries a COMPOUND's minor version")
	}
}

// A call whose later fields do not decode comes back with its error, and a
// record too short to address comes back with none.
func TestParseRefusals(t *testing.T) {
	head := func(prog, vers, proc uint32) []byte {
		b := binary.BigEndian.AppendUint32(nil, 0x11223344)
		for _, v := range []uint32{0, 2, prog, vers, proc} {
			b = binary.BigEndian.AppendUint32(b, v)
		}
		return b
	}
	for _, c := range []struct {
		name string
		rec  []byte
	}{
		{"a credential body of 401 bytes", func() []byte {
			b := head(100003, 4, 0)
			b = binary.BigEndian.AppendUint32(b, 1)
			b = binary.BigEndian.AppendUint32(b, 401)
			b = append(b, make([]byte, 404)...)  // 401 and its padding, inside the record
			return append(b, make([]byte, 8)...) // a valid verifier, so the body's size is the only refusal
		}()},
		{"a verifier past the record", func() []byte {
			b := head(100003, 4, 0)
			b = append(b, 0, 0, 0, 0, 0, 0, 0, 0) // AUTH_NONE, no body
			b = binary.BigEndian.AppendUint32(b, 0)
			return binary.BigEndian.AppendUint32(b, 8)
		}()},
		{"a tag past the record", func() []byte {
			b := head(100003, 4, 1)
			b = append(b, make([]byte, 16)...) // AUTH_NONE and AUTH_NULL, no bodies
			return binary.BigEndian.AppendUint32(b, 100)
		}()},
		{"an opaque's padding past the record", func() []byte {
			b := head(100003, 4, 0)
			b = binary.BigEndian.AppendUint32(b, 1)
			b = binary.BigEndian.AppendUint32(b, 1)
			return append(b, 0x7f)
		}()},
		{"a verifier's padding past the record", func() []byte {
			b := head(100003, 4, 0)
			b = append(b, 0, 0, 0, 0, 0, 0, 0, 0) // AUTH_NONE, no body
			b = binary.BigEndian.AppendUint32(b, 1)
			b = binary.BigEndian.AppendUint32(b, 1)
			return append(b, 0x7f) // one byte and no padding
		}()},
		{"a COMPOUND's minor version past the record", func() []byte {
			b := head(100003, 4, 1)
			b = append(b, make([]byte, 16)...)
			return binary.BigEndian.AppendUint32(b, 0) // an empty tag, no minor version
		}()},
	} {
		t.Run(c.name, func(t *testing.T) {
			call, err := wire.ParseCall(c.rec)
			if err == nil {
				t.Fatal("accepted")
			}
			if len(call) == 0 || call.XID() != 0x11223344 {
				t.Errorf("the refusal comes back with %x", []byte(call))
			}
		})
	}
	t.Run("an RPC version other than two, its credential past the record", func(t *testing.T) {
		b := binary.BigEndian.AppendUint32(nil, 0x11223344)
		for _, v := range []uint32{0, 3, 100003, 4, 0} {
			b = binary.BigEndian.AppendUint32(b, v)
		}
		// The credential's length claims a body the record does not hold; a
		// version other than 2 stops the parse before it, so the router can
		// answer RPC_MISMATCH rather than a decode refusal (RFC 21 section
		// 11.5).
		b = binary.BigEndian.AppendUint32(b, 1)
		b = binary.BigEndian.AppendUint32(b, 400)
		call, err := wire.ParseCall(b)
		if err != nil {
			t.Fatalf("RPC version 3: %v, want no error", err)
		}
		if call.RPCVersion() != 3 {
			t.Errorf("the RPC version reads %d", call.RPCVersion())
		}
	})
	t.Run("an RPC version other than two, the record ending after it", func(t *testing.T) {
		b := binary.BigEndian.AppendUint32(nil, 0x11223344)
		b = binary.BigEndian.AppendUint32(b, 0)
		b = binary.BigEndian.AppendUint32(b, 3)
		call, err := wire.ParseCall(b)
		if err != nil || call.RPCVersion() != 3 {
			t.Errorf("RPC version 3 and nothing after it: %d, %v, want 3 and no error", call.RPCVersion(), err)
		}
	})
	t.Run("a message of seven bytes", func(t *testing.T) {
		call, err := wire.ParseCall(make([]byte, 7))
		if len(call) != 0 || !errors.Is(err, wire.ErrUnaddressable) {
			t.Errorf("a seven-byte record comes back with %x, %v", []byte(call), err)
		}
	})
	t.Run("a reply", func(t *testing.T) {
		b := binary.BigEndian.AppendUint32(nil, 0x11223344)
		b = binary.BigEndian.AppendUint32(b, 1)
		call, err := wire.ParseCall(b)
		if len(call) == 0 || !errors.Is(err, wire.ErrNotCall) {
			t.Errorf("a reply comes back with %x, %v", []byte(call), err)
		}
	})
}

// DecodeAuthSys gives the captured credential's fields, and refuses RFC 5531's
// limits.
func TestDecodeAuthSys(t *testing.T) {
	c, err := wire.ParseCall(body(t, "nfsv4-compound-call"))
	if err != nil {
		t.Fatal(err)
	}
	_, cred := c.Credential()
	a, err := wire.DecodeAuthSys(cred)
	if err != nil {
		t.Fatal(err)
	}
	if a.Stamp != 0 || string(a.MachineName()) != "dittofs-knfsd-ref" || a.UID != 0 || a.GID != 0 || a.NGIDs != 0 {
		t.Errorf("the captured credential is %+v", a)
	}

	parms := func(name []byte, groups int) []byte {
		b := binary.BigEndian.AppendUint32(nil, 0)
		b = binary.BigEndian.AppendUint32(b, uint32(len(name)))
		b = append(b, name...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		b = binary.BigEndian.AppendUint32(b, 0)
		b = binary.BigEndian.AppendUint32(b, 0)
		b = binary.BigEndian.AppendUint32(b, uint32(groups))
		for range groups {
			b = binary.BigEndian.AppendUint32(b, 0)
		}
		return b
	}
	if _, err := wire.DecodeAuthSys(parms(bytes.Repeat([]byte{'a'}, 256), 0)); err == nil {
		t.Error("a machine name of 256 bytes is accepted")
	}
	if _, err := wire.DecodeAuthSys(parms([]byte("host"), 17)); err == nil {
		t.Error("a group list of 17 is accepted")
	}
	cut := parms([]byte("host"), 4)
	if _, err := wire.DecodeAuthSys(cut[:len(cut)-4]); err == nil {
		t.Error("a body cut inside its group list is accepted")
	}
	over := append(parms([]byte("host"), 2), 9, 9, 9, 9)
	if _, err := wire.DecodeAuthSys(over); err == nil {
		t.Error("a body with bytes after its group list is accepted")
	}

	// The limits, at their edge: a machine name of 255 bytes and a group list
	// of 16 decode, each the most RFC 5531 allows.
	if a, err := wire.DecodeAuthSys(parms(nil, 16)); err != nil || a.NGIDs != 16 {
		t.Errorf("a group list of 16 is %v (%v), want it decoded", a.NGIDs, err)
	}
	if a, err := wire.DecodeAuthSys(parms([]byte("host"), 2)); err != nil || a.NGIDs != 2 {
		t.Errorf("a group list of 2 is %v (%v), want every group it named", a.NGIDs, err)
	}
	if a, err := wire.DecodeAuthSys(parms(bytes.Repeat([]byte{'a'}, 255), 0)); err != nil || a.NameLen != 255 {
		t.Errorf("a machine name of 255 bytes is %d (%v), want it decoded", a.NameLen, err)
	}

	// A body cut inside its machine name: the name's length says more than the
	// body holds.
	whole := parms([]byte("0123456789"), 0)
	// A fresh slice of its own, so nothing past its end is in bounds: a decoder
	// that read the name past the body's end would reach out of the record.
	cutName := append([]byte(nil), whole[:len(whole)-20]...)
	if _, err := wire.DecodeAuthSys(cutName); err == nil {
		t.Error("a body cut inside its machine name is accepted")
	}
}

// Each reply is written byte for byte: the capture's NULL reply for the same
// transaction ID, and the hand-written vectors of every accept and reject
// state.
func TestReplies(t *testing.T) {
	null := body(t, "nfsv4-null-call")
	call, err := wire.ParseCall(null)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := wire.AppendAccepted(nil, call.XID(), wire.AcceptSuccess), body(t, "nfsv4-null-reply"); !bytes.Equal(got, want) {
		t.Errorf("the NULL reply is %x, want %x", got, want)
	}
	for _, c := range []struct {
		golden string
		got    []byte
	}{
		{"accept-prog-unavail", wire.AppendAccepted(nil, 0x33b50de8, wire.AcceptProgUnavail)},
		{"accept-prog-mismatch", wire.AppendProgMismatch(nil, 0x33b50de8, 3, 4)},
		{"accept-proc-unavail", wire.AppendAccepted(nil, 0x33b50de8, wire.AcceptProcUnavail)},
		{"accept-garbage-args", wire.AppendAccepted(nil, 0x33b50de8, wire.AcceptGarbageArgs)},
		{"accept-system-err", wire.AppendAccepted(nil, 0x33b50de8, wire.AcceptSystemErr)},
		{"deny-rpc-mismatch", wire.AppendDenied(nil, 0x33b50de8, true, 0)},
		{"deny-auth-badcred", wire.AppendDenied(nil, 0x33b50de8, false, wire.AuthBadCred)},
		{"deny-auth-badverf", wire.AppendDenied(nil, 0x33b50de8, false, wire.AuthBadVerf)},
		{"compound-minor-mismatch", wire.AppendCompoundStatus(nil, 10021, []byte("abcde"), 0)},
		{"compound-two-results", wire.AppendCompoundStatus(nil, 0, []byte("abcde"), 2)},
	} {
		t.Run(c.golden, func(t *testing.T) {
			if want := readGolden(t, c.golden); !bytes.Equal(c.got, want) {
				t.Errorf("%s is %x, want %x", c.golden, c.got, want)
			}
		})
	}
}

// A refusal that claims a length allocates nothing for it: the bytes are never
// read, so a claim of more than 1 MiB costs less than 4 KiB.
func TestClaimedLengths(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the allocation count says nothing under the detector")
	}
	head := func(prog, vers, proc uint32) []byte {
		b := binary.BigEndian.AppendUint32(nil, 0x11223344)
		for _, v := range []uint32{0, 2, prog, vers, proc} {
			b = binary.BigEndian.AppendUint32(b, v)
		}
		return b
	}
	big := ^uint32(0)
	for _, c := range []struct {
		name string
		fn   func() error
	}{
		{"a credential's body", func() error {
			b := binary.BigEndian.AppendUint32(head(100003, 4, 0), 1)
			b = binary.BigEndian.AppendUint32(b, big)
			_, err := wire.ParseCall(b)
			return err
		}},
		{"a tag", func() error {
			b := append(head(100003, 4, 1), make([]byte, 16)...)
			b = binary.BigEndian.AppendUint32(b, big)
			_, err := wire.ParseCall(b)
			return err
		}},
		{"a group list", func() error {
			body := binary.BigEndian.AppendUint32(nil, 0)
			body = binary.BigEndian.AppendUint32(body, 0)
			body = binary.BigEndian.AppendUint32(body, 0)
			body = binary.BigEndian.AppendUint32(body, 0)
			body = binary.BigEndian.AppendUint32(body, big)
			_, err := wire.DecodeAuthSys(body)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.fn(); err == nil {
				t.Fatal("a claimed length past the record is accepted")
			}
			const runs = 1000
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range runs {
				_ = c.fn()
			}
			runtime.ReadMemStats(&after)
			if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 4<<10 {
				t.Errorf("a refusal allocates %d bytes", per)
			}
		})
	}
}
