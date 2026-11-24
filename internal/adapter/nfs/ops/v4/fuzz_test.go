package v4

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/core"
	"github.com/cubbit/dittofs/internal/adapter/nfs/wire"
	"github.com/cubbit/dittofs/pkg/resources"
)

// seeds adds every golden file to f, each as it is and, where it opens with a
// record mark, past it.
func seeds(f *testing.F, add func(b []byte)) {
	files, err := filepath.Glob("../../wire/testdata/golden/*.hex")
	if err != nil || len(files) == 0 {
		f.Fatalf("no golden file found: %v", err)
	}
	for _, name := range files {
		b := golden(f, strings.TrimSuffix(filepath.Base(name), ".hex"))
		add(b)
		if len(b) > 4 {
			add(b[4:])
		}
	}
}

// FuzzCompound holds the COMPOUND row's decoder to the header's parse, an
// independent decoder: for any call wire.ParseCall accepts as a COMPOUND, the
// decoder takes no more than its arguments and, where it decodes, reads the
// header's minor version, the head it echoes carries wire.Call.Tag's tag and
// its count is the word after the minor version; ILLEGAL's decoder takes exactly the bytes after it (RFC 20
// section 5.4, RFC 21 section 11).
func FuzzCompound(f *testing.F) {
	seeds(f, func(b []byte) { f.Add(b) })
	illegal := Table(&Handlers{}).Ops[wire.ProcCompound].Members.Ops[opIllegal]
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := wire.ParseCall(b)
		if err != nil {
			return
		}
		minor, ok := c.MinorVersion()
		if !ok {
			return
		}
		args := c.Args()
		inv, n, err := decodeCompound(args, core.Version(minor))
		if err != nil {
			return
		}
		var a compoundArgs
		if _, err := a.DecodeWire(args, core.Version(minor)); err != nil || int(a.Minor) != minor {
			t.Fatalf("the COMPOUND row's arguments read a minor version of %d (%v), the header's is %d", a.Minor, err, minor)
		}
		if n < 0 || n > len(args) {
			t.Fatalf("the COMPOUND row took %d bytes of %d", n, len(args))
		}
		tag := c.Tag()
		at := 4 + len(tag) + (4-len(tag)%4)%4 + 4 // the tag's length, the tag padded, the minor version
		if len(args) < at+4 {
			t.Fatalf("the COMPOUND row decoded %d bytes of arguments, which hold no count after the minor version at %d", len(args), at)
		}
		if want := int(binary.BigEndian.Uint32(args[at:])); inv.Count != want {
			t.Fatalf("the COMPOUND row reads a count of %d, the word after the minor version is %d", inv.Count, want)
		}
		out := &resources.Buf{}
		if err := inv.EncodeHead(out, 10044, 3, core.Version(minor)); err != nil {
			t.Fatal(err)
		}
		if want := wire.AppendCompoundStatus(nil, 10044, tag, 3); !bytes.Equal(out.B, want) {
			t.Fatalf("the head echoes %x, the header's tag gives %x", out.B, want)
		}
		rest := args[n:]
		if _, k, err := illegal.Decode(rest, core.Version(minor)); err != nil || k != len(rest) {
			t.Fatalf("ILLEGAL took %d bytes of %d, %v", k, len(rest), err)
		}
	})
}

// codec is what the generator writes for each struct codecTypes lists.
type codec interface {
	DecodeWire(b []byte, v uint32) (int, error)
	EncodeWire(v uint32, out *resources.Buf) error
}

// FuzzCodec decodes an input's bytes past its first two as the struct its
// first picks among every struct the generator wrote a codec for, at the
// minor version its second names: what decodes takes no more than the bytes,
// and re-encodes to bytes that decode, whole, to the same value (RFC 20
// section 5.4). Seeded with every golden file for every struct. A round trip
// alone passes where encoder and decoder share a bug: the generator's runtime
// is held to a second decoder by wire's own FuzzCodec, and these structs'
// bytes by the golden files the loopback tests compare.
func FuzzCodec(f *testing.F) {
	kinds := len(codecTypes())
	seeds(f, func(b []byte) {
		for i := range kinds {
			f.Add(append([]byte{byte(i), 1}, b...))
		}
	})
	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) < 2 {
			return
		}
		pick, v, b := int(in[0])%kinds, uint32(in[1]%3), in[2:]
		got := codecTypes()[pick].(codec)
		n, err := got.DecodeWire(b, v)
		if err != nil {
			return
		}
		if n < 0 || n > len(b) {
			t.Fatalf("%T at %d: took %d bytes of %d", got, v, n, len(b))
		}
		out := &resources.Buf{}
		if err := got.EncodeWire(v, out); err != nil {
			t.Fatalf("%T at %d: a decoded value does not encode: %v", got, v, err)
		}
		again := codecTypes()[pick].(codec)
		if m, err := again.DecodeWire(out.B, v); err != nil || m != len(out.B) || !reflect.DeepEqual(again, got) {
			t.Fatalf("%T at %d: %+v encodes to %x, which reads back as %+v (%d of %d bytes, %v)", got, v, got, out.B, again, m, len(out.B), err)
		}
	})
}
