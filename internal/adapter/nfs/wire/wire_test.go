package wire_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
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
		{"the cap", wire.MaxRecord, true, true},
		{"one past the cap", wire.MaxRecord + 1, true, false},
		{"a body that does not end the record", 1024, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			mark := wire.AppendRecordMark(nil, int(c.n))
			if !c.last {
				mark[0] &^= 0x80
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
	if fl != wire.AuthUnix || len(cred) != 28 {
		t.Errorf("the credential is %d/%d bytes, want AUTH_SYS of 28", fl, len(cred))
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
			if call == nil || call.XID() != 0x11223344 {
				t.Errorf("the refusal comes back with %v", call)
			}
		})
	}
	t.Run("a message of seven bytes", func(t *testing.T) {
		call, err := wire.ParseCall(make([]byte, 7))
		if call != nil || err != wire.ErrUnaddressable {
			t.Errorf("a seven-byte record comes back with %v, %v", call, err)
		}
	})
	t.Run("a reply", func(t *testing.T) {
		b := binary.BigEndian.AppendUint32(nil, 0x11223344)
		b = binary.BigEndian.AppendUint32(b, 1)
		call, err := wire.ParseCall(b)
		if call == nil || err != wire.ErrNotCall {
			t.Errorf("a reply comes back with %v, %v", call, err)
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
		{"compound-minor-mismatch", wire.AppendCompoundHead(nil, 10021, []byte("abc"), 0)},
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
