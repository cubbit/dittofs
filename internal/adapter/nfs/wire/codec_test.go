package wire_test

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"runtime"
	"testing"

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

// vectors are the golden vectors, the version each is read at, and the value
// each holds, as the capture or RFC 4506 gives it (RFC 20 section 5.4).
func vectors(t testing.TB) []struct {
	name string
	v    uint32
	want codec
} {
	stateID := func(seqid uint32, other ...byte) StateID {
		s := StateID{Seqid: seqid}
		copy(s.Other[:], other)
		return s
	}
	sample := Sample{Words: [3]uint32{1, 2, 3}, Flag: true, Name: "abcdefgh", Mask: []uint32{7, 8, 9}}
	sampleV1 := sample
	sampleV1.Extra = 0x0102030405060708
	return []struct {
		name string
		v    uint32
		want codec
	}{
		{"codec-setclientid-args", 0, &SetClientIDArgs{
			Client: ClientID{
				Verifier: [8]byte{0x18, 0xbb, 0xa2, 0x63, 0xbf, 0x7e, 0x26, 0x47},
				ID:       []byte("Linux NFSv4.0 dittofs-knfsd-ref/127.0.0.1"),
			},
			Callback:      CBClient{Program: 0x40000000, Location: NetAddr{NetID: "tcp", Addr: "127.0.0.1.151.157"}},
			CallbackIdent: 1,
		}},
		{"codec-setattr-args", 0, &SetattrArgs{Attrs: FAttr{Mask: []uint32{0x1000}, List: readGolden(t, "codec-acl")}}},
		{"codec-acl", 0, &ACL{ACEs: []ACE{
			{AccessMask: 0x0016019f, Who: "OWNER@"},
			{AccessMask: 0x00160187, Who: "OWNER@"},
			{AccessMask: 0x00120081, Who: "GROUP@"},
			{AccessMask: 0x00120081, Who: "EVERYONE@"},
		}}},
		{"codec-open-claim-null", 0, &OpenClaim{Claim: 0, File: "a.txt"}},
		{"codec-open-claim-previous", 0, &OpenClaim{Claim: 1, DelegateType: 2}},
		{"codec-open-claim-delegate-cur", 0, &OpenClaim{Claim: 2, DelegateCur: DelegateCur{
			StateID: stateID(7, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15),
			File:    "b",
		}}},
		{"codec-open-claim-delegate-prev", 0, &OpenClaim{Claim: 3, FileDelegatePrev: "prev"}},
		{"codec-open-claim-fh", 1, &OpenClaim{Claim: 4}},
		{"codec-open-claim-deleg-cur-fh", 1, &OpenClaim{Claim: 5, DelegateStateID: stateID(3, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)}},
		{"codec-open-claim-deleg-prev-fh", 1, &OpenClaim{Claim: 6}},
		{"codec-settime-client", 0, &SetTime{How: 1, Time: NFSTime{Seconds: -1, NSeconds: 500000000}}},
		{"codec-settime-server", 0, &SetTime{How: 0}},
		{"codec-post-op-fh-present", 0, &PostOpFH{Handle: &FH3{Data: []byte{1, 0, 1, 0, 0, 0, 0, 0}}}},
		{"codec-post-op-fh-absent", 0, &PostOpFH{}},
		{"codec-write-args", 0, &Write4Args{StateID: stateID(1), Offset: 4 << 30, Stable: 2, Data: []byte("hello")}},
		{"codec-void", 0, &Void{}},
		{"codec-sample-v0", 0, &sample},
		{"codec-sample-v1", 1, &sampleV1},
	}
}

// Each golden vector decodes to its struct's fields, reading every byte, and
// each struct encodes to its vector's bytes (RFC 20 section 5.4).
func TestCodecGoldenVectors(t *testing.T) {
	for _, c := range vectors(t) {
		t.Run(c.name, func(t *testing.T) {
			b := readGolden(t, c.name)
			got := fresh(c.want)
			n, err := got.DecodeWire(b, c.v)
			if err != nil || n != len(b) {
				t.Fatalf("decoded %d of %d bytes: %v", n, len(b), err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("decoded\n  %+v\nwant\n  %+v", got, c.want)
			}
			var out resources.Buf
			if err := c.want.EncodeWire(c.v, &out); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.B, b) {
				t.Errorf("encoded\n  %x\nwant\n  %x", out.B, b)
			}
		})
	}
}

// A field on the wire from minor version 1 is neither read nor written at 0,
// and an arm on the wire from 1 is, at 0, a discriminant with no arm, refused
// both ways (RFC 20 section 5.1).
func TestCodecMinorVersions(t *testing.T) {
	v0, v1 := readGolden(t, "codec-sample-v0"), readGolden(t, "codec-sample-v1")
	var s Sample
	if n, err := s.DecodeWire(v1, 0); err != nil || n != len(v0) {
		t.Errorf("version 1's sample read at 0: %d bytes of %d, %v", n, len(v0), err)
	}
	if s.Extra != 0 {
		t.Errorf("a field from version 1 read at 0: %#x", s.Extra)
	}
	s.Extra = 0x0102030405060708
	var out resources.Buf
	if err := s.EncodeWire(0, &out); err != nil || !bytes.Equal(out.B, v0) {
		t.Errorf("encoded at 0\n  %x (%v)\nwant\n  %x", out.B, err, v0)
	}
	for _, name := range []string{"codec-open-claim-fh", "codec-open-claim-deleg-cur-fh", "codec-open-claim-deleg-prev-fh"} {
		var c OpenClaim
		if _, err := c.DecodeWire(readGolden(t, name), 0); err == nil {
			t.Errorf("%s: an arm from version 1 decoded at 0", name)
		}
		if _, err := c.DecodeWire(readGolden(t, name), 1); err != nil {
			t.Fatal(err)
		}
		if err := c.EncodeWire(0, &resources.Buf{}); err == nil {
			t.Errorf("%s: an arm from version 1 encoded at 0", name)
		}
	}
}

// A byte field decodes as a slice of the record, never a copy: a WRITE's 1 MiB
// of data costs less than 4 KiB (RFC 20 section 5.3).
func TestCodecByteFieldIsASliceOfTheRecord(t *testing.T) {
	rec := readGolden(t, "codec-write-args")[:28] // its stateid, offset and stable
	rec = binary.BigEndian.AppendUint32(rec, 1<<20)
	rec = append(rec, bytes.Repeat([]byte{0xa5}, 1<<20)...)
	var w Write4Args
	if n, err := w.DecodeWire(rec, 0); err != nil || n != len(rec) {
		t.Fatalf("decoded %d of %d bytes: %v", n, len(rec), err)
	}
	if len(w.Data) != 1<<20 || &w.Data[0] != &rec[32] {
		t.Fatalf("the data is %d bytes, not the record's own from byte 32", len(w.Data))
	}
	if prof.RaceEnabled {
		t.Skip("the allocation count says nothing under the detector")
	}
	if n := allocated(func() {
		var w Write4Args
		_, _ = w.DecodeWire(rec, 0)
	}); n >= 4<<10 {
		t.Errorf("decoding 1 MiB of data allocates %d bytes", n)
	}
}

// A byte field's capacity ends at its last byte, so an append to it copies
// rather than overwriting what the record holds next, a COMPOUND's next
// operation (RFC 20 section 5.3).
func TestCodecByteFieldEndsAtItsLength(t *testing.T) {
	rec := binary.BigEndian.AppendUint32(readGolden(t, "codec-write-args"), 9) // the next operation, GETATTR
	want := bytes.Clone(rec)
	var w Write4Args
	if _, err := w.DecodeWire(rec, 0); err != nil {
		t.Fatal(err)
	}
	_ = append(w.Data, "more"...)
	if !bytes.Equal(rec, want) {
		t.Errorf("an append to the data overwrote the record\n  %x\nwant\n  %x", rec, want)
	}
}

// What cannot be the struct is refused: a discriminant with no arm, a string
// or an array past its bound, a count past the bytes left, a fixed-length
// opaque cut short, missing padding, and a bool or optional flag other than 0
// and 1 (RFC 20 section 5.2, RFC 4506). Each input is whole at minor version 0
// but for the one fault it is named for.
func TestCodecRefusals(t *testing.T) {
	word := func(b []byte, vs ...uint32) []byte {
		for _, v := range vs {
			b = binary.BigEndian.AppendUint32(b, v)
		}
		return b
	}
	head := func() []byte { return word(nil, 1, 2, 3, 1) } // a sample's words and flag
	write := readGolden(t, "codec-write-args")
	for _, c := range []struct {
		name string
		into codec
		b    []byte
	}{
		{"a discriminant with no arm and no default", &OpenClaim{}, word(nil, 7)},
		{"a string past its bound", &Sample{}, word(append(word(head(), 9), "abcdefghi\x00\x00\x00"...), 0)},
		{"an array past its bound", &Sample{}, word(head(), 0, 4, 1, 2, 3, 4)},
		{"a count past the bytes left", &Sample{}, word(head(), 0, 3, 1, 2)},
		{"a fixed-length opaque cut short", &Write4Args{}, write[:14]},
		{"missing padding", &Write4Args{}, write[:len(write)-3]},
		{"a bool of 2", &Sample{}, word(nil, 1, 2, 3, 2, 0, 0)},
		{"optional data's flag of 2", &PostOpFH{}, word(nil, 2, 0)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if n, err := c.into.DecodeWire(c.b, 0); err == nil {
				t.Errorf("accepted, %d bytes read", n)
			}
		})
	}
}

// A refusal of a claimed length or count costs less than 4 KiB: it is checked
// against the bytes left before anything is made (RFC 20 section 5.2).
func TestCodecClaimedLengths(t *testing.T) {
	if prof.RaceEnabled {
		t.Skip("the allocation count says nothing under the detector")
	}
	acl := binary.BigEndian.AppendUint32(nil, 65536) // entries, at the bound, and no bytes for them
	write := binary.BigEndian.AppendUint32(readGolden(t, "codec-write-args")[:28], 1<<20)
	for _, c := range []struct {
		name string
		into func() codec
		b    []byte
	}{
		{"an ACL of 65536 entries", func() codec { return &ACL{} }, acl},
		{"a WRITE of 1 MiB", func() codec { return &Write4Args{} }, write},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.into().DecodeWire(c.b, 0); err == nil {
				t.Fatal("a claim past the bytes left is accepted")
			}
			if n := allocated(func() { _, _ = c.into().DecodeWire(c.b, 0) }); n >= 4<<10 {
				t.Errorf("the refusal allocates %d bytes", n)
			}
		})
	}
}

// A value the peer's decoder would refuse is refused on encode, and its
// output is left as it was: a string, an array or an opaque past its bound,
// and a discriminant with no arm.
func TestCodecEncodeRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		from codec
	}{
		{"a string past its bound", &Sample{Name: "abcdefghi"}},
		{"an array past its bound", &Sample{Mask: []uint32{1, 2, 3, 4}}},
		{"an opaque past its bound", &PostOpFH{Handle: &FH3{Data: make([]byte, 65)}}},
		{"a discriminant with no arm", &OpenClaim{Claim: 7}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := resources.Buf{B: []byte("before")}
			if err := c.from.EncodeWire(1, &out); err == nil {
				t.Error("encoded")
			}
			if string(out.B) != "before" {
				t.Errorf("the output became %q", out.B)
			}
		})
	}
}

// MaxWire is the most a struct encodes to at a version under its tags' maxima,
// each written out from RFC 4506's sizes: 4 bytes a word, 8 a hyper, an
// opaque's length word and its bytes padded to 4 (RFC 20 section 4.7).
func TestCodecMaxWire(t *testing.T) {
	const ace = 3*4 + 4 + 1024
	for _, c := range []struct {
		of     codec
		v0, v1 int
	}{
		{&SetClientIDArgs{}, 8 + 4 + 1024 + 4 + 4 + 16 + 4 + 64 + 4, 8 + 4 + 1024 + 4 + 4 + 16 + 4 + 64 + 4},
		{&SetattrArgs{}, 16 + 4 + 4*4 + 4 + 4096, 16 + 4 + 4*4 + 4 + 4096},
		{&ACL{}, 4 + 65536*ace, 4 + 65536*ace},
		{&OpenClaim{}, 4 + 16 + 4 + 256, 4 + 16 + 4 + 256}, // CLAIM_DELEGATE_CUR's, the largest arm
		{&SetTime{}, 4 + 8 + 4, 4 + 8 + 4},
		{&PostOpFH{}, 4 + 4 + 64, 4 + 4 + 64},
		{&Write4Args{}, 16 + 8 + 4 + 4 + 1<<20, 16 + 8 + 4 + 4 + 1<<20},
		{&Sample{}, 3*4 + 4 + 4 + 8 + 4 + 3*4, 3*4 + 4 + 4 + 8 + 4 + 3*4 + 8},
	} {
		if n := c.of.MaxWire(0); n != c.v0 {
			t.Errorf("%T at 0: %d, want %d", c.of, n, c.v0)
		}
		if n := c.of.MaxWire(1); n != c.v1 {
			t.Errorf("%T at 1: %d, want %d", c.of, n, c.v1)
		}
	}
}

// IsZero is true of a zero struct and false once any one of its fields is set,
// a nested struct's by its first field (RFC 20 section 4.7).
func TestCodecIsZero(t *testing.T) {
	var set func(v reflect.Value)
	set = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Bool:
			v.SetBool(true)
		case reflect.Uint8, reflect.Uint32, reflect.Uint64:
			v.SetUint(1)
		case reflect.Int64:
			v.SetInt(1)
		case reflect.String:
			v.SetString("x")
		case reflect.Slice:
			v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		case reflect.Array:
			set(v.Index(0))
		case reflect.Pointer:
			v.Set(reflect.New(v.Type().Elem()))
		case reflect.Struct:
			set(v.Field(0))
		default:
			t.Fatalf("no value set for a %s", v.Type())
		}
	}
	for _, c := range []codec{&SetClientIDArgs{}, &SetattrArgs{}, &ACL{}, &OpenClaim{}, &SetTime{}, &PostOpFH{}, &Write4Args{}, &Void{}, &Sample{}} {
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
