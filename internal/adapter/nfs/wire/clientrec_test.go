package wire_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
)

// recGolden is each golden file of the Adapter bytes and the value it holds.
var recGolden = []struct {
	file string
	rec  wire.ClientRec
}{
	{"clientrec-nfs41", wire.ClientRec{
		Kind: wire.ClientRecKindNFS4,
		NFS4: wire.ClientRecNFS4{
			Minor:        1,
			SessionSeq:   5,
			SessionReply: []byte{1, 2, 3, 4, 5},
			MustEnforce:  []uint32{3, 0x10},
		},
	}},
	{"clientrec-nfs40", wire.ClientRec{
		Kind: wire.ClientRecKindNFS4,
		NFS4: wire.ClientRecNFS4{
			MustEnforce: []uint32{1},
			MustAllow:   []uint32{1, 2, 4},
			Callback: &wire.CBClient4{
				Program: 0x40000000,
				NetID:   "tcp",
				Addr:    "10.0.0.1.8.1",
				Ident:   7,
			},
			Confirm: &wire.ClientRecConfirm{Verifier: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}},
		},
	}},
	{"clientrec-nlm4", wire.ClientRec{
		Kind: wire.ClientRecKindNLM,
		NLM: wire.ClientRecNLM{
			MonName: "client.example",
			Priv: [16]byte{
				0xa0,
				0xa1,
				0xa2,
				0xa3,
				0xa4,
				0xa5,
				0xa6,
				0xa7,
				0xa8,
				0xa9,
				0xaa,
				0xab,
				0xac,
				0xad,
				0xae,
				0xaf,
			},
			Host:   []byte{192, 168, 0, 1},
			Server: []byte{192, 168, 0, 2},
		},
	}},
	{"clientrec-nlm6", wire.ClientRec{
		Kind: wire.ClientRecKindNLM,
		NLM: wire.ClientRecNLM{
			MonName: "h",
			Priv:    [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			Host:    []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
			Server:  []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2},
		},
	}},
}

// word, opq and cat build a record by hand, a word a uint32 and opq an XDR
// opaque or string, zero padded (RFC 4506 sections 4.1, 4.10).
func word(v ...uint32) []byte {
	var b []byte
	for _, w := range v {
		b = binary.BigEndian.AppendUint32(b, w)
	}

	return b
}

func opq(p []byte) []byte {
	return append(append(word(uint32(len(p))), p...), make([]byte, (4-len(p)%4)%4)...)
}

func cat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// An NFSv4 record of the Adapter bytes, version 1, with the parts a row varies
// and every other part valid: reply, the two bitmaps as counted words, then the
// callback and the confirm, each absent unless given.
func nfs4Rec(reply, enforce, allow, callback, confirm []byte) []byte {
	if callback == nil {
		callback = word(0)
	}

	if confirm == nil {
		confirm = word(0)
	}

	return cat([]byte{1}, word(2, 1, 5), opq(reply), enforce, allow, callback, confirm)
}

// An NLM record of the Adapter bytes, version 1, kind 3, around a name, a host
// and a server as opaques already built.
func nlmRec(name, host, server []byte) []byte {
	return cat([]byte{1}, word(3), name, make([]byte, 16), host, server)
}

var (
	noWords   = word(0)
	ip4       = opq([]byte{10, 0, 0, 1})
	ip6       = opq(bytes.Repeat([]byte{6}, 16))
	threeWord = word(3, 1, 2, 3)
	fourWord  = word(4, 1, 2, 3, 4)
)

// callback is a present NFSv4.0 callback around a netid and an address.
func callback(netID, addr string) []byte {
	return cat(word(1, 3), opq([]byte(netID)), opq([]byte(addr)), word(9))
}

func repeat(n int) []byte {
	return bytes.Repeat([]byte{'a'}, n)
}

// Each golden file decodes to its value, reading every byte, and the value
// encodes to the file's bytes: a field moved, dropped or retyped fails one.
func TestClientRecGolden(t *testing.T) {
	for _, c := range recGolden {
		t.Run(c.file, func(t *testing.T) {
			b := readGolden(t, c.file)

			got, err := wire.DecodeClientRec(b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}

			if !reflect.DeepEqual(got, c.rec) {
				t.Errorf("decoded\n%+v\nwant\n%+v", got, c.rec)
			}

			enc, err := wire.AppendClientRec(nil, &c.rec)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			if !bytes.Equal(enc, b) {
				t.Errorf("encoded %x, want %x", enc, b)
			}

			after, err := wire.AppendClientRec(append(make([]byte, 0, 256), "keep"...), &c.rec)
			if err != nil || !bytes.Equal(after, append([]byte("keep"), b...)) {
				t.Errorf("appended after keep: %x (%v), want keep then %x", after, err, b)
			}
		})
	}
}

// A first byte other than 1 is an unknown version, whatever follows it.
func TestClientRecVersion(t *testing.T) {
	good := readGolden(t, "clientrec-nfs41")
	for _, v := range []byte{0, 2, 255} {
		b := bytes.Clone(good)

		b[0] = v
		if _, err := wire.DecodeClientRec(b); !errors.Is(err, wire.ErrRecVersion) {
			t.Errorf("version %d: %v, want ErrRecVersion", v, err)
		}
	}
}

// A body cut anywhere is truncated, and so is no byte at all.
func TestClientRecTruncated(t *testing.T) {
	for _, c := range recGolden {
		b := readGolden(t, c.file)
		for n := range len(b) {
			if _, err := wire.DecodeClientRec(b[:n]); !errors.Is(err, wire.ErrRecTruncated) {
				t.Errorf(
					"%s cut to %d bytes of %d: %v, want ErrRecTruncated",
					c.file,
					n,
					len(b),
					err,
				)
			}
		}
	}
}

// corruptRows are records, each valid but for the one thing it names.
var corruptRows = []struct {
	name string
	b    []byte
}{
	{"a trailing byte", append(nfs4Rec(nil, noWords, noWords, nil, nil), 0)},
	{"minor version 2", cat([]byte{1}, word(2, 2, 5), opq(nil), noWords, noWords, word(0, 0))},
	{"a trailing byte after nlm", append(nlmRec(opq(nil), ip4, ip4), 0)},
	{"kind 0", cat([]byte{1}, word(0))},
	{"kind 1", cat([]byte{1}, word(1))},
	{"kind 4", cat([]byte{1}, word(4))},
	{"callback flag 2", nfs4Rec(nil, noWords, noWords, word(2), nil)},
	{"confirm flag 2", nfs4Rec(nil, noWords, noWords, nil, word(2))},
	{"session reply of 257", nfs4Rec(repeat(257), noWords, noWords, nil, nil)},
	{"enforce of 4 words", nfs4Rec(nil, fourWord, noWords, nil, nil)},
	{"allow of 4 words", nfs4Rec(nil, noWords, fourWord, nil, nil)},
	{"netid of 17", nfs4Rec(nil, noWords, noWords, callback(string(repeat(17)), "a"), nil)},
	{"addr of 65", nfs4Rec(nil, noWords, noWords, callback("tcp", string(repeat(65))), nil)},
	{"mon_name of 1025", nlmRec(opq(repeat(1025)), ip4, ip4)},
	{"host of 0", nlmRec(opq(nil), opq(nil), ip4)},
	{"host of 5", nlmRec(opq(nil), opq(make([]byte, 5)), ip4)},
	{"host of 8", nlmRec(opq(nil), opq(make([]byte, 8)), ip4)},
	{"host of 12", nlmRec(opq(nil), opq(make([]byte, 12)), ip4)},
	{"host of 17", nlmRec(opq(nil), opq(make([]byte, 17)), ip4)},
	{"server of 0", nlmRec(opq(nil), ip4, opq(nil))},
	{"server of 8", nlmRec(opq(nil), ip4, opq(make([]byte, 8)))},
	{"server of 15", nlmRec(opq(nil), ip4, opq(make([]byte, 15)))},
}

// Each row past a bound, a kind with no arm or a length other than 4 or 16 is corrupt,
// not truncated: the bytes are there.
func TestClientRecCorrupt(t *testing.T) {
	for _, c := range corruptRows {
		t.Run(c.name, func(t *testing.T) {
			if _, err := wire.DecodeClientRec(c.b); !errors.Is(err, wire.ErrRecCorrupt) {
				t.Errorf("%v, want ErrRecCorrupt", err)
			}
		})
	}
}

// Each bound's own value decodes, and encodes back to the bytes it came from.
func TestClientRecAtLimits(t *testing.T) {
	for _, c := range []struct {
		name string
		b    []byte
		rec  wire.ClientRec
	}{
		{
			"nfs4 bounds",
			nfs4Rec(
				repeat(256),
				threeWord,
				threeWord,
				callback(string(repeat(16)), string(repeat(64))),
				nil,
			),
			wire.ClientRec{
				Kind: wire.ClientRecKindNFS4,
				NFS4: wire.ClientRecNFS4{
					Minor:        1,
					SessionSeq:   5,
					SessionReply: repeat(256),
					MustEnforce:  []uint32{1, 2, 3},
					MustAllow:    []uint32{1, 2, 3},
					Callback: &wire.CBClient4{
						Program: 3,
						NetID:   strings.Repeat("a", 16),
						Addr:    strings.Repeat("a", 64),
						Ident:   9,
					},
				},
			},
		},
		{
			"mon_name of 1024, host and server of 4",
			nlmRec(opq(repeat(1024)), ip4, ip4),
			wire.ClientRec{
				Kind: wire.ClientRecKindNLM,
				NLM: wire.ClientRecNLM{
					MonName: strings.Repeat("a", 1024),
					Host:    []byte{10, 0, 0, 1},
					Server:  []byte{10, 0, 0, 1},
				},
			},
		},
		{
			"host and server of 16",
			nlmRec(opq(nil), ip6, ip6),
			wire.ClientRec{
				Kind: wire.ClientRecKindNLM,
				NLM: wire.ClientRecNLM{
					Host:   bytes.Repeat([]byte{6}, 16),
					Server: bytes.Repeat([]byte{6}, 16),
				},
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := wire.DecodeClientRec(c.b)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}

			if !reflect.DeepEqual(got, c.rec) {
				t.Errorf("decoded\n%+v\nwant\n%+v", got, c.rec)
			}

			enc, err := wire.AppendClientRec(nil, &c.rec)
			if err != nil || !bytes.Equal(enc, c.b) {
				t.Errorf("encoded %x (%v), want %x", enc, err, c.b)
			}
		})
	}
}

// A decoded value owns its bytes: the record's buffer is then free to be
// written over, as AppendClientRec(b[:0], ...) does when a node rewrites it.
func TestClientRecNoAlias(t *testing.T) {
	for _, c := range []struct {
		file string
		rec  wire.ClientRec
	}{recGolden[0], recGolden[2]} {
		b := readGolden(t, c.file)

		got, err := wire.DecodeClientRec(b)
		if err != nil || !reflect.DeepEqual(got, c.rec) {
			t.Fatalf("%s decodes to %+v (%v), want %+v", c.file, got, err, c.rec)
		}

		for i := range b {
			b[i] = 0xff
		}

		if !reflect.DeepEqual(got, c.rec) {
			t.Errorf("%s: after the input was overwritten, %+v, want %+v", c.file, got, c.rec)
		}
	}
}

// A value the decoder refuses is refused by the encoder, and the buffer is
// handed back as it came: no node writes a record no node reads.
func TestAppendClientRecRefuses(t *testing.T) {
	nlm := func(host, server int) wire.ClientRec {
		return wire.ClientRec{
			Kind: wire.ClientRecKindNLM,
			NLM:  wire.ClientRecNLM{Host: make([]byte, host), Server: make([]byte, server)},
		}
	}

	nfs4 := func(n wire.ClientRecNFS4) wire.ClientRec {
		return wire.ClientRec{Kind: wire.ClientRecKindNFS4, NFS4: n}
	}
	for _, c := range []struct {
		name string
		rec  wire.ClientRec
	}{
		{"minor version 2", nfs4(wire.ClientRecNFS4{Minor: 2})},
		{"kind 0", wire.ClientRec{}},
		{"kind 1", wire.ClientRec{Kind: 1}},
		{"kind 4", wire.ClientRec{Kind: 4}},
		{"session reply of 257", nfs4(wire.ClientRecNFS4{SessionReply: repeat(257)})},
		{"enforce of 4 words", nfs4(wire.ClientRecNFS4{MustEnforce: make([]uint32, 4)})},
		{"allow of 4 words", nfs4(wire.ClientRecNFS4{MustAllow: make([]uint32, 4)})},
		{"netid of 17", nfs4(wire.ClientRecNFS4{
			Callback: &wire.CBClient4{NetID: string(repeat(17))},
		})},
		{"addr of 65", nfs4(wire.ClientRecNFS4{
			Callback: &wire.CBClient4{Addr: string(repeat(65))},
		})},
		{"mon_name of 1025", wire.ClientRec{
			Kind: wire.ClientRecKindNLM,
			NLM: wire.ClientRecNLM{
				MonName: string(repeat(1025)),
				Host:    make([]byte, 4),
				Server:  make([]byte, 4),
			},
		}},
		{"host of 0", nlm(0, 4)},
		{"host of 5", nlm(5, 4)},
		{"host of 8", nlm(8, 4)},
		{"host of 12", nlm(12, 4)},
		{"host of 17", nlm(17, 4)},
		{"server of 0", nlm(4, 0)},
		{"server of 8", nlm(4, 8)},
		{"server of 15", nlm(4, 15)},
	} {
		t.Run(c.name, func(t *testing.T) {
			dst := append(make([]byte, 0, 64), "keep"...)

			got, err := wire.AppendClientRec(dst, &c.rec)
			if !errors.Is(err, wire.ErrRecCorrupt) {
				t.Errorf("%v, want ErrRecCorrupt", err)
			}

			if string(got) != "keep" {
				t.Errorf("returned %q, want the buffer as it came", got)
			}
		})
	}

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("a nil record panicked: %v", r)
		}
	}()

	got, err := wire.AppendClientRec(append(make([]byte, 0, 64), "keep"...), nil)
	if !errors.Is(err, wire.ErrRecCorrupt) || string(got) != "keep" {
		t.Errorf("a nil record: %q, %v, want keep and ErrRecCorrupt", got, err)
	}
}
