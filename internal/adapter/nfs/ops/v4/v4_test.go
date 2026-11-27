package v4

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
)

// words is XDR's encoding of each: four bytes, big-endian (RFC 4506 section
// 4.1).
func words(ws ...uint32) []byte {
	var b []byte
	for _, w := range ws {
		b = binary.BigEndian.AppendUint32(b, w)
	}
	return b
}

// opaque is XDR's variable-length opaque: its length, its bytes and their
// padding to four (RFC 4506 section 4.10).
func opaque(body []byte) []byte {
	b := append(words(uint32(len(body))), body...)
	return append(b, make([]byte, (4-len(body)%4)%4)...)
}

// golden reads one of the wire package's golden files, hex with # comments.
func golden(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../wire/testdata/golden/" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	var digits strings.Builder
	for line := range strings.SplitSeq(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		digits.WriteString(strings.Join(strings.Fields(line), ""))
	}
	b, err := hex.DecodeString(digits.String())
	if err != nil {
		t.Fatalf("golden %s: %v", name, err)
	}
	return b
}

// short is b in hex where it is short, and its length and ends otherwise, so a
// failure on a 64 KiB tag stays readable.
func short(b []byte) string {
	if len(b) <= 64 {
		return fmt.Sprintf("%x", b)
	}
	return fmt.Sprintf("%d bytes %x...%x", len(b), b[:16], b[len(b)-16:])
}

// specified is whether some minor version defines code, as the RFCs list the
// operations: NFSv4.0's ACCESS (3) to RELEASE_LOCKOWNER (39), RFC 7530
// section 16; NFSv4.1's BACKCHANNEL_CTL (40) to RECLAIM_COMPLETE (58), RFC
// 8881 section 18; NFSv4.2's ALLOCATE (59) to CLONE (71), RFC 7862 section
// 15, and the extended attributes' GETXATTR (72) to REMOVEXATTR (75), RFC
// 8276 section 8; and ILLEGAL (10044).
func specified(code uint32) bool {
	return 3 <= code && code <= 39 || 40 <= code && code <= 58 || 59 <= code && code <= 71 ||
		72 <= code && code <= 75 || code == 10044
}

// Every 32-bit operation code reads as itself where some minor version
// defines it and as OP_ILLEGAL otherwise, taking its four bytes; none reads
// as a default (RFC 21 section 12, N26). The codes below 2^16, the top 2^16
// and a seeded sample between are read.
func TestReadOpIsTotal(t *testing.T) {
	check := func(code uint32) {
		got, n, err := readOp(words(code, 0xdeadbeef))
		want := uint32(10044)
		if specified(code) {
			want = code
		}
		if err != nil || n != 4 || got != want {
			t.Fatalf("code %d reads as %d, %d bytes, %v; want %d, 4 bytes", code, got, n, err, want)
		}
	}
	for code := range uint32(1 << 16) {
		check(code)
	}
	for code := uint32(math.MaxUint32 - 1<<16); ; code++ {
		check(code)
		if code == math.MaxUint32 {
			break
		}
	}
	rnd := rand.New(rand.NewPCG(1, 2))
	for range 1 << 16 {
		check(rnd.Uint32())
	}
	if _, _, err := readOp([]byte{0, 0, 0}); err == nil {
		t.Error("a code of three bytes reads, want an error")
	}
}

// A result's head is its operation code and status, the code OP_ILLEGAL for
// an NFS4ERR_OP_ILLEGAL status whatever the code, and for a code no minor
// version defines, the 0 core passes for a code it could not read among them
// (RFC 21 section 11.5).
func TestEncodeResultNamesILLEGAL(t *testing.T) {
	for _, c := range []struct {
		name     string
		code     uint32
		status   core.Status
		wantCode uint32
	}{
		{"READ refused NFS4ERR_OP_ILLEGAL", 25, 10044, 10044},
		{"SEQUENCE refused NFS4ERR_OP_ILLEGAL", 53, 10044, 10044},
		{"ILLEGAL", 10044, 10044, 10044},
		{"a code core could not read", 0, 10036, 10044},
		{"an undefined code", 9999, 10036, 10044},
		{"READ not supported", 25, 10004, 25},
		{"REMOVEXATTR succeeding", 75, 0, 75},
	} {
		out := &resources.Buf{}
		if err := encodeResult(out, c.code, c.status, 1); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := words(c.wantCode, uint32(c.status)); !bytes.Equal(out.B, want) {
			t.Errorf("%s: the result's head is %x, want %x", c.name, out.B, want)
		}
	}
}

// The COMPOUND row's decoder reads COMPOUND4args' tag, minor version and
// count from a capture's and a hand-written call's arguments and takes no
// more; its head echoes the tag whole, its size the same for any status and
// count; a tag running past the bytes is refused, one of 64 KiB is not, the
// tag having no cap of its own (RFC 21 sections 11 and 11.5).
func TestTheCompoundRowDecodes(t *testing.T) {
	args := func(name string) []byte {
		rec := golden(t, name)[4:] // past the record mark
		c, err := wire.ParseCall(rec)
		if err != nil {
			t.Fatalf("golden %s: %v", name, err)
		}
		return c.Args()
	}
	big := bytes.Repeat([]byte{'t'}, 64<<10)
	for _, c := range []struct {
		name  string
		args  []byte
		tag   []byte
		count int
	}{
		{"the capture's NFSv4.0 SETCLIENTID", args("nfsv4-compound-call"), []byte{}, 1},
		{"a five-byte tag", args("nfsv4-compound-short-tag"), []byte("abcde"), 0},
		{"a tag of 64 KiB", append(opaque(big), words(1, 3)...), big, 3},
	} {
		inv, n, err := decodeCompound(c.args, 0)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if want := 4 + len(opaque(c.tag)) + 4; n != want {
			t.Errorf("%s: took %d bytes, want %d", c.name, n, want)
		}
		if inv.Count != c.count {
			t.Errorf("%s: a count of %d, want %d", c.name, inv.Count, c.count)
		}
		for _, h := range []struct {
			s core.Status
			n int
		}{{0, 0}, {10044, 2}, {10070, 129}} {
			out := &resources.Buf{}
			if err := inv.EncodeHead(out, h.s, h.n, 0); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			want := append(append(words(uint32(h.s)), opaque(c.tag)...), words(uint32(h.n))...)
			if !bytes.Equal(out.B, want) {
				t.Errorf("%s: the head for status %d and %d results is %s, want %s", c.name, h.s, h.n, short(out.B), short(want))
			}
		}
	}
	for _, c := range []struct {
		name string
		args []byte
	}{
		{"a tag past the bytes", append(words(9), "abcd"...)},
		{"no count", append(opaque([]byte("t")), words(1)...)},
	} {
		if _, _, err := decodeCompound(c.args, 0); err == nil {
			t.Errorf("%s: decoded, want an error", c.name)
		}
	}
}

// ILLEGAL's row takes every byte it is handed, and its handler's error is
// answered NFS4ERR_OP_ILLEGAL by the members' table, with nothing past the
// status, under every minor version (RFC 21 section 11.5).
func TestIllegalIsAnsweredOpIllegal(t *testing.T) {
	members := Table(&Handlers{}).Ops[wire.ProcCompound].Members
	row := members.Ops[opIllegal]
	for minor := range core.Version(3) {
		rest := words(1, 2, 3)
		inv, n, err := row.Decode(rest, minor)
		if err != nil || n != len(rest) {
			t.Fatalf("minor %d: ILLEGAL took %d bytes, %v; want %d", minor, n, err, len(rest))
		}
		r := &core.Request{Version: minor}
		err = inv.Call(t.Context(), r)
		if got, _, _ := members.Errors.Map(err, row, minor); got != 10044 {
			t.Errorf("minor %d: ILLEGAL's error %v is answered %d, want NFS4ERR_OP_ILLEGAL 10044", minor, err, got)
		}
		if !errors.Is(err, errIllegal) {
			t.Errorf("minor %d: ILLEGAL returns %v, want its own error", minor, err)
		}
		out := &resources.Buf{}
		if err := inv.Encode(out, minor); err != nil || len(out.B) != 0 {
			t.Errorf("minor %d: ILLEGAL's reply writes %x, %v; want nothing", minor, out.B, err)
		}
		if row.Versions.First > minor || minor > row.Versions.Last {
			t.Errorf("minor %d: ILLEGAL serves %d to %d, want every minor version", minor, row.Versions.First, row.Versions.Last)
		}
	}
}

// The tag's only bound is the record's: the generated codec's max for it is
// wire.MaxRecord, in the arguments and in the head, so a change to the record
// cap that left the tag's max behind would give the tag a cap of its own (RFC
// 21 section 11.5).
func TestTheTagIsBoundedByTheRecordAlone(t *testing.T) {
	if got, want := (*compoundArgs)(nil).MaxWire(0), 4+wire.MaxRecord+4+4; got != want {
		t.Errorf("COMPOUND4args' head encodes to at most %d bytes, want %d: a tag of wire.MaxRecord, the minor version and the count", got, want)
	}
	if got, want := (*compoundHead)(nil).MaxWire(0), 4+4+wire.MaxRecord+4; got != want {
		t.Errorf("COMPOUND4res' head encodes to at most %d bytes, want %d: the status, a tag of wire.MaxRecord and the count", got, want)
	}
}
