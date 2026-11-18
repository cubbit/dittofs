package wire_test

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cubbit/dittofs/internal/adapter/smb/wire"
	"github.com/cubbit/dittofs/internal/prof"
	"github.com/cubbit/dittofs/pkg/resources"
)

// codec is what the generator writes for every struct: RFC 20 section 4.7's
// Decoder and Encoder methods.
type codec interface {
	DecodeWire(b []byte, v uint32) (int, error)
	EncodeWire(v uint32, out *resources.Buf) error
	MaxWire(v uint32) int
	IsZero() bool
}

// fresh is a zero value of c's type.
func fresh(c codec) codec {
	return reflect.New(reflect.TypeOf(c).Elem()).Interface().(codec)
}

// allocated is what fn allocates per call, in bytes.
func allocated(fn func()) uint64 {
	const runs = 100
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		fn()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / runs
}

// golden is the codec vector named name: an SMB2 body, its first byte the one
// after the header's last.
func golden(t testing.TB, name string) []byte { return wire.ReadGolden(t, "codec-"+name) }

// span is b's bytes from position from to position to, positions counted from
// the header's first byte, as the vectors' comments and the offsets count them.
func span(b []byte, from, to int) []byte { return b[from-wire.HeaderLen : to-wire.HeaderLen] }

// patched is a copy of b with put written at position pos.
func patched(b []byte, pos int, put ...byte) []byte {
	b = bytes.Clone(b)
	copy(b[pos-wire.HeaderLen:], put)
	return b
}

// spliced is a copy of b with its bytes from position from to position to
// replaced by put.
func spliced(b []byte, from, to int, put ...byte) []byte {
	out := append(bytes.Clone(b[:from-wire.HeaderLen]), put...)
	return append(out, b[to-wire.HeaderLen:]...)
}

// le16 and le32 are v's little-endian bytes.
func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// units is UTF-16LE code units, written out.
func units(us ...uint16) []byte {
	var b []byte
	for _, u := range us {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// named is frame 126's CREATE with its name's bytes replaced by name.
func named(t testing.TB, name []byte) []byte {
	b := patched(golden(t, "create-request")[:56], 110, le16(uint16(len(name)))...)
	return append(b, name...)
}

// overlapped is a CREATE whose 128 create contexts each hold their name and
// their data in the same 16 bytes, so they would encode apart, past the 4096
// bytes the chain may take.
func overlapped(t testing.TB) []byte {
	b := patched(golden(t, "create-request")[:56], 110, 0, 0)
	b = patched(b, 112, append(le32(120), le32(128*32)...)...)
	for i := range 128 {
		next := uint32(32)
		if i == 127 {
			next = 0
		}
		b = append(b, le32(next)...)
		b = append(b, 16, 0, 16, 0, 0, 0, 16, 0) // NameOffset, NameLength, Reserved, DataOffset
		b = append(b, le32(16)...)               // DataLength
		b = append(b, "xxxxxxxxxxxxxxxx"...)
	}
	return b
}

// frame6 is the value frame 6's NEGOTIATE response holds.
func frame6(t testing.TB) NegotiateResponse {
	b := golden(t, "negotiate-response")
	return NegotiateResponse{
		SecurityMode:    1,
		DialectRevision: 0x311,
		ServerGUID:      [16]byte([]byte("dittofs-smbsd\x00\x00\x00")),
		Capabilities:    0x0f,
		MaxTransactSize: 8 << 20,
		MaxReadSize:     8 << 20,
		MaxWriteSize:    8 << 20,
		SystemTime:      0x01dd01c44aaaf5f2,
		SecurityBuffer:  span(b, 128, 202),
		Contexts: []NegotiateContext{
			{ContextType: 1, Preauth: PreauthIntegrityCapabilities{HashAlgorithms: []uint16{1}, Salt: span(b, 222, 254)}},
			{ContextType: 2, Encryption: EncryptionCapabilities{Ciphers: []uint16{2}}},
			{ContextType: 8, Data: []byte{1, 0, 2, 0}},
		},
	}
}

// vectors are the golden vectors, the dialect each is read at, and the value
// each holds, as the capture or MS-SMB2 2.2 gives it (RFC 20 section 5.4).
func vectors(t testing.TB) []struct {
	name string
	v    uint32
	want codec
} {
	resp := frame6(t)
	resp21 := resp
	resp21.DialectRevision, resp21.Contexts = 0x210, nil
	req := golden(t, "negotiate-request")
	return []struct {
		name string
		v    uint32
		want codec
	}{
		{"close-request", 0x311, &CloseRequest{FileID: [16]byte{0x32, 0x80, 0xad, 0xb4, 0, 0, 0, 0, 0x66, 0x44, 0x93, 0x43}}},
		{"create-request", 0x311, &CreateRequest{
			ImpersonationLevel: 2, DesiredAccess: 0x000c0000, ShareAccess: 3, CreateDisposition: 1, Name: "file.txt",
		}},
		{"create-contexts", 0x311, &CreateRequest{
			ImpersonationLevel: 2, DesiredAccess: 0x00120089, FileAttributes: 0x80, ShareAccess: 7,
			CreateDisposition: 3, CreateOptions: 0x40, Name: "a.txt",
			Contexts: []CreateContext{
				{Name: []byte("DH2Q"), DurableV2: DurableHandleRequestV2{
					Timeout: 60000, Flags: 2, CreateGUID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
				}},
				{Name: []byte("AlSi"), AllocationSize: AllocationSize{AllocationSize: 1 << 20}},
				{Name: []byte("MxAc"), Data: []byte{0xf2, 0xf5, 0xaa, 0x4a, 0xc4, 0x01, 0xdd, 0x01}},
			},
		}},
		{"negotiate-request", 0x311, &NegotiateRequest{
			SecurityMode: 1, Capabilities: 0x7f, ClientGUID: [16]byte(span(req, 76, 92)),
			Dialects: []uint16{0x202, 0x210, 0x300, 0x302, 0x311},
			Contexts: []NegotiateContext{
				{ContextType: 1, Preauth: PreauthIntegrityCapabilities{HashAlgorithms: []uint16{1}, Salt: span(req, 126, 158)}},
				{ContextType: 2, Encryption: EncryptionCapabilities{Ciphers: []uint16{2, 1, 4, 3}}},
				{ContextType: 8, Data: []byte{3, 0, 2, 0, 1, 0, 0, 0}},
				{ContextType: 5, Data: units('1', '2', '7', '.', '0', '.', '0', '.', '1')},
			},
		}},
		{"negotiate-response", 0x311, &resp},
		{"negotiate-response-21", 0x210, &resp21},
		{"write-request", 0x311, &WriteRequest{
			Offset: 4 << 30, FileID: [16]byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28},
			Data: []byte("hello"),
		}},
		{"echo-request", 0x311, &EchoRequest{}},
	}
}

// Each golden vector decodes to its struct's fields, reading every byte, and
// each struct encodes to its vector's bytes after a reply's header and the
// replies chained before it, its offsets counted from that header (RFC 20
// section 5.4, MS-SMB2 2.2).
func TestCodecGoldenVectors(t *testing.T) {
	before := bytes.Repeat([]byte{0xee}, 80+wire.HeaderLen) // a reply chained before, then this one's header
	for _, c := range vectors(t) {
		t.Run(c.name, func(t *testing.T) {
			b := golden(t, c.name)
			got := fresh(c.want)
			n, err := got.DecodeWire(b, c.v)
			if err != nil || n != len(b) {
				t.Fatalf("decoded %d of %d bytes: %v", n, len(b), err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("decoded\n  %+v\nwant\n  %+v", got, c.want)
			}
			out := resources.Buf{B: bytes.Clone(before)}
			if err := c.want.EncodeWire(c.v, &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.B, append(bytes.Clone(before), b...)) {
				t.Errorf("encoded\n  %x\nwant\n  %x", out.B[len(before):], b)
			}
		})
	}
}

// The negotiate contexts, on the wire from 3.1.1, are neither read nor
// written at 2.1, where their count and offset are Reserved (MS-SMB2 2.2.4).
func TestCodecDialects(t *testing.T) {
	v21 := golden(t, "negotiate-response-21")
	var r NegotiateResponse
	if n, err := r.DecodeWire(golden(t, "negotiate-response"), 0x210); err != nil || n != len(v21) {
		t.Errorf("frame 6 read at 2.1: %d bytes, not the %d before its contexts: %v", n, len(v21), err)
	}
	if r.Contexts != nil {
		t.Errorf("contexts read at 2.1: %+v", r.Contexts)
	}
	r = frame6(t)
	r.DialectRevision = 0x210
	var out resources.Buf
	if err := r.EncodeWire(0x210, &out); err != nil || !bytes.Equal(out.B, v21) {
		t.Errorf("written at 2.1\n  %x (%v)\nwant\n  %x", out.B, err, v21)
	}
}

// A name is UTF-16LE on the wire and WTF-8 in the struct: a surrogate pair is
// its one code point, and a surrogate that is half of no pair is kept as its
// three WTF-8 bytes, which utf8.ValidString refuses, so a handler tells it from
// a U+FFFD the client sent; each writes back to its bytes (RFC 22 section
// 12.3). A name of an odd length is refused.
func TestCodecUTF16Names(t *testing.T) {
	for _, c := range []struct {
		name  string
		units []byte
		want  string
		valid bool
	}{
		{"a surrogate pair", units('a', 0xd83d, 0xde00), "a\U0001F600", true},
		{"a lone high surrogate", units(0xd800, 'a'), "\xed\xa0\x80a", false},
		{"a lone low surrogate", units('a', 0xdfff), "a\xed\xbf\xbf", false},
		{"a high surrogate before a pair", units(0xdbff, 0xd800, 0xdc00), "\xed\xaf\xbf\U00010000", false},
		{"U+FFFD", units(0xfffd), "�", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := named(t, c.units)
			var r CreateRequest
			if _, err := r.DecodeWire(b, 0x311); err != nil {
				t.Fatal(err)
			}
			if r.Name != c.want || utf8.ValidString(r.Name) != c.valid {
				t.Errorf("decoded %q, valid UTF-8 %v; want %q, %v", r.Name, utf8.ValidString(r.Name), c.want, c.valid)
			}
			var out resources.Buf
			if err := r.EncodeWire(0x311, &out); err != nil || !bytes.Equal(out.B, b) {
				t.Errorf("encoded\n  %x (%v)\nwant\n  %x", out.B, err, b)
			}
		})
	}
	var r CreateRequest
	if _, err := r.DecodeWire(named(t, []byte("a\x00b\x00c\x00d")), 0x311); err == nil {
		t.Errorf("a name of 7 bytes decoded as %q", r.Name)
	}
}

// A byte field decodes as a slice of the record, never a copy: a WRITE's 1 MiB
// of data costs less than 4 KiB (RFC 20 section 5.3).
func TestCodecByteFieldIsASliceOfTheRecord(t *testing.T) {
	rec := patched(golden(t, "write-request")[:48], 68, le32(1<<20)...)
	rec = append(rec, bytes.Repeat([]byte{0xa5}, 1<<20)...)
	var w WriteRequest
	if n, err := w.DecodeWire(rec, 0x311); err != nil || n != len(rec) {
		t.Fatalf("decoded %d of %d bytes: %v", n, len(rec), err)
	}
	if len(w.Data) != 1<<20 || &w.Data[0] != &rec[48] {
		t.Fatalf("the data is %d bytes, not the record's own from position 112", len(w.Data))
	}
	if prof.RaceEnabled {
		t.Skip("the allocation count says nothing under the detector")
	}
	if n := allocated(func() {
		var w WriteRequest
		_, _ = w.DecodeWire(rec, 0x311)
	}); n >= 4<<10 {
		t.Errorf("decoding 1 MiB of data allocates %d bytes", n)
	}
}

// A byte field's capacity ends at its last byte, so an append to it copies
// rather than overwriting what the record holds next, a chained request's
// padding (RFC 20 section 5.3).
func TestCodecByteFieldEndsAtItsLength(t *testing.T) {
	rec := append(golden(t, "write-request"), 0, 0, 0) // padding to the next request's 8-byte aligned header
	want := bytes.Clone(rec)
	var w WriteRequest
	if _, err := w.DecodeWire(rec, 0x311); err != nil {
		t.Fatal(err)
	}
	_ = append(w.Data, "more"...)
	if !bytes.Equal(rec, want) {
		t.Errorf("an append to the data overwrote the record\n  %x\nwant\n  %x", rec, want)
	}
}

// What cannot be the struct is refused: a StructureSize or a fixed part wrong,
// a field located in the bytes of a field before it, past the message or past its maximum, an offset off its
// alignment, a create context's Next off 8 or out of its chain, a chain past
// the message, and a context's data shorter than its struct (RFC 20 section
// 5.2, MS-SMB2 2.2, 3.3.5.9). Each input is a vector whole but for the one
// fault it is named for.
func TestCodecRefusals(t *testing.T) {
	closeReq, create := golden(t, "close-request"), golden(t, "create-request")
	ctxs, neg := golden(t, "create-contexts"), golden(t, "negotiate-request")
	for _, c := range []struct {
		name string
		into codec
		b    []byte
	}{
		{"a StructureSize of 25", &CloseRequest{}, patched(closeReq, 64, 25)},
		{"a fixed part cut short", &CloseRequest{}, closeReq[:23]},
		{"a name in the fixed part", &CreateRequest{}, patched(create, 108, 118)},
		{"a name past the message", &CreateRequest{}, patched(create, 108, 136)},
		{"a name longer than the message", &CreateRequest{}, patched(create, 110, 18)},
		{"a name past its maximum", &CreateRequest{}, named(t, units(utf16.Encode([]rune(strings.Repeat("a", 513)))...))},
		{"contexts at an offset off 8", &CreateRequest{}, patched(spliced(ctxs, 130, 136, 0, 0), 112, 132)},
		{"a context's data at an offset off 8", &CreateRequest{}, patched(spliced(spliced(ctxs, 192, 192, 0, 0, 0, 0), 156, 160), 146, 20)},
		{"a context's Next off 8", &CreateRequest{}, patched(patched(spliced(ctxs, 192, 192, 0, 0, 0, 0), 136, 60), 116, 124)},
		{"a context's Next inside it", &CreateRequest{}, patched(ctxs, 136, 8)},
		{"a context's Next past the contexts", &CreateRequest{}, patched(ctxs, 136, 128)},
		{"contexts whose names and data share their bytes", &CreateRequest{}, overlapped(t)},
		{"a context's data before its name", &CreateRequest{}, patched(patched(patched(
			spliced(ctxs, 240, 256, append(span(ctxs, 248, 256), "MxAc"...)...), 228, 24), 234, 16), 116, 116)},
		{"contexts past the message", &CreateRequest{}, patched(ctxs, 116, 121)},
		{"a context's data shorter than its struct", &CreateRequest{}, patched(ctxs, 148, 16)},
		{"negotiate contexts at an offset off 8", &NegotiateRequest{}, patched(neg, 92, 110)},
		{"negotiate contexts past the message", &NegotiateRequest{}, patched(neg, 96, 5)},
		{"negotiate contexts past their maximum", &NegotiateRequest{}, patched(neg, 96, 17)},
		{"dialects past their maximum", &NegotiateRequest{}, patched(neg, 66, 65)},
		{"a negotiate context's data past the message", &NegotiateRequest{}, patched(neg, 202, 20)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if n, err := c.into.DecodeWire(c.b, 0x311); err == nil {
				t.Errorf("accepted, %d bytes read", n)
			}
		})
	}
}

// An odd StructureSize counts the variable part's first byte, which a request
// carries and a reply, or a WRITE, need not: a CREATE of its fixed part alone
// is refused and an empty one is written with that byte, while a WRITE of its
// fixed part alone decodes and an empty NEGOTIATE reply is written without it
// (MS-SMB2 2.2.13, 3.3.5.9).
func TestCodecOddStructureSize(t *testing.T) {
	var c CreateRequest
	if n, err := c.DecodeWire(patched(golden(t, "create-request")[:56], 110, 0, 0), 0x311); err == nil {
		t.Errorf("a CREATE of 56 bytes accepted, %d read", n)
	}
	var out resources.Buf
	if err := (&CreateRequest{}).EncodeWire(0x311, &out); err != nil || len(out.B) != 57 || out.B[56] != 0 {
		t.Errorf("an empty CREATE written as %x (%v), not 57 bytes ending in 0", out.B, err)
	}
	var w WriteRequest
	if n, err := w.DecodeWire(patched(golden(t, "write-request")[:48], 68, le32(0)...), 0x311); err != nil || n != 48 {
		t.Errorf("a WRITE of 48 bytes: %d read, %v", n, err)
	}
	out = resources.Buf{}
	if err := (&NegotiateResponse{}).EncodeWire(0x210, &out); err != nil || len(out.B) != 64 {
		t.Errorf("an empty NEGOTIATE reply written as %d bytes (%v), not 64", len(out.B), err)
	}
}

// A create context's data is its struct's bytes, none past them, as MS-SMB2
// 3.3.5.9 has a server refuse any other; a negotiate context's may run past
// its struct, which reads its fields alone (MS-SMB2 2.2.3.1).
func TestCodecContextDataLength(t *testing.T) {
	ctxs := golden(t, "create-contexts")
	longer := patched(patched(patched(spliced(ctxs, 192, 192, make([]byte, 8)...), 148, 40), 136, 64), 116, 128)
	var c CreateRequest
	if n, err := c.DecodeWire(longer, 0x311); err == nil {
		t.Errorf("a DH2Q context of 40 bytes accepted, %d bytes read", n)
	}
	var r NegotiateRequest
	if _, err := r.DecodeWire(patched(golden(t, "negotiate-request"), 162, 12), 0x311); err != nil {
		t.Fatalf("an encryption context of 12 bytes refused: %v", err)
	}
	if want := []uint16{2, 1, 4, 3}; !reflect.DeepEqual(r.Contexts[1].Encryption.Ciphers, want) {
		t.Errorf("its ciphers %v, want %v", r.Contexts[1].Encryption.Ciphers, want)
	}
}

// A refusal of a claimed length or count costs less than 4 KiB: it is checked
// against the bytes left before anything is made (RFC 20 section 5.2).
func TestCodecClaimedLengths(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the allocation count says nothing under the detector")
	}
	for _, c := range []struct {
		name string
		into func() codec
		b    []byte
	}{
		{"a WRITE of 1 MiB", func() codec { return &WriteRequest{} }, patched(golden(t, "write-request")[:49], 68, le32(1<<20)...)},
		{"a name of 1024 bytes", func() codec { return &CreateRequest{} }, patched(golden(t, "create-request")[:57], 110, le16(1024)...)},
		{"4096 bytes of create contexts", func() codec { return &CreateRequest{} }, patched(golden(t, "create-request"), 112, 136, 0, 0, 0, 0, 0x10)},
		{"16 negotiate contexts", func() codec { return &NegotiateRequest{} }, patched(golden(t, "negotiate-request")[:48], 96, 16)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.into().DecodeWire(c.b, 0x311); err == nil {
				t.Fatal("a claim past the bytes left is accepted")
			}
			if n := allocated(func() { _, _ = c.into().DecodeWire(c.b, 0x311) }); n >= 4<<10 {
				t.Errorf("the refusal allocates %d bytes", n)
			}
		})
	}
}

// A value the peer's decoder would refuse is refused on encode, and its output
// is left as it was: a name, a count or a context past its maximum, contexts
// past the bytes their chain may take, a name that is not WTF-8, and an offset
// its two bytes cannot hold.
func TestCodecEncodeRefusals(t *testing.T) {
	big := CreateContext{Name: []byte("MxAc"), Data: make([]byte, 1000)}
	for _, c := range []struct {
		name string
		from codec
	}{
		{"a name past its maximum", &CreateRequest{Name: strings.Repeat("a", 513)}},
		{"a name that is not WTF-8", &CreateRequest{Name: "a\xffb"}},
		{"a context's data past its maximum", &CreateRequest{Contexts: []CreateContext{{Name: []byte("MxAc"), Data: make([]byte, 1025)}}}},
		{"contexts past their chain's maximum", &CreateRequest{Contexts: []CreateContext{big, big, big, big, big}}},
		{"dialects past their maximum", &NegotiateRequest{Dialects: make([]uint16, 65)}},
		{"negotiate contexts past their maximum", &NegotiateRequest{Contexts: make([]NegotiateContext, 17)}},
		{"an offset past its two bytes", &WriteRequest{Data: make([]byte, 1<<16), ChannelInfo: []byte{1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := resources.Buf{B: []byte("before")}
			if err := c.from.EncodeWire(0x311, &out); err == nil {
				t.Error("encoded")
			}
			if string(out.B) != "before" {
				t.Errorf("the output became %q", out.B)
			}
		})
	}
}

// MaxWire is the most a struct encodes to at a dialect under its tags' maxima,
// each written out from MS-SMB2 2.2's layouts: the fixed part, each variable
// field's maximum and the padding its alignment may need (RFC 20 section 4.7).
func TestCodecMaxWire(t *testing.T) {
	const negCtx = 8 + 1024 // its fixed part and its data's maximum, past either struct's
	for _, c := range []struct {
		of        codec
		v21, v311 int
	}{
		{&CloseRequest{}, 24, 24},
		{&CreateRequest{}, 56 + 1024 + 7 + 4096, 56 + 1024 + 7 + 4096},
		{&CreateContext{}, 16 + 16 + 7 + 1024, 16 + 16 + 7 + 1024},
		{&DurableHandleRequestV2{}, 32, 32},
		{&AllocationSize{}, 8, 8},
		{&NegotiateRequest{}, 36 + 2*64 + 7 + 16*(7+negCtx), 36 + 2*64 + 7 + 16*(7+negCtx)},
		{&NegotiateResponse{}, 64 + 4096, 64 + 4096 + 7 + 16*(7+negCtx)},
		{&NegotiateContext{}, negCtx, negCtx},
		{&PreauthIntegrityCapabilities{}, 4 + 2*8 + 64, 4 + 2*8 + 64},
		{&EncryptionCapabilities{}, 2 + 2*8, 2 + 2*8},
		{&WriteRequest{}, 48 + 8<<20 + 1024, 48 + 8<<20 + 1024},
		{&EchoRequest{}, 4, 4},
	} {
		if n := c.of.MaxWire(0x210); n != c.v21 {
			t.Errorf("%T at 2.1: %d, want %d", c.of, n, c.v21)
		}
		if n := c.of.MaxWire(0x311); n != c.v311 {
			t.Errorf("%T at 3.1.1: %d, want %d", c.of, n, c.v311)
		}
	}
}

// IsZero is true of a zero struct and false once any one of its named fields
// is set, a nested struct's by its first named field (RFC 20 section 4.7).
func TestCodecIsZero(t *testing.T) {
	var set func(v reflect.Value)
	set = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			v.SetUint(1)
		case reflect.String:
			v.SetString("x")
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		case reflect.Array:
			set(v.Index(0))
		case reflect.Struct:
			for i := range v.NumField() {
				if v.Type().Field(i).Name != "_" {
					set(v.Field(i))
					return
				}
			}
			t.Fatalf("no named field in a %s", v.Type())
		default:
			t.Fatalf("no value set for a %s", v.Type())
		}
	}
	for _, c := range []codec{
		&CloseRequest{}, &CreateRequest{}, &CreateContext{}, &DurableHandleRequestV2{}, &AllocationSize{},
		&NegotiateRequest{}, &NegotiateResponse{}, &NegotiateContext{}, &PreauthIntegrityCapabilities{},
		&EncryptionCapabilities{}, &WriteRequest{}, &EchoRequest{},
	} {
		if !fresh(c).IsZero() {
			t.Errorf("a zero %T is not zero", c)
		}
		typ := reflect.TypeOf(c).Elem()
		for i := range typ.NumField() {
			if typ.Field(i).Name == "_" {
				continue
			}
			x := fresh(c)
			set(reflect.ValueOf(x).Elem().Field(i))
			if x.IsZero() {
				t.Errorf("%T with %s set is zero", c, typ.Field(i).Name)
			}
		}
	}
}
