// Command gen writes SMB2's codec (MS-SMB2 2.2) for the struct types declared
// in the Go files it is named, in one file beside them: DecodeWire,
// EncodeWire, MaxWire and IsZero, which RFC 20 section 4.7's Decoder and
// Encoder require, as ordinary Go over smb/wire's runtime, so nothing is
// looked up while a request is served (RFC 20 section 5.1). go generate runs
// it from the package's directory:
//
//	//go:generate go run <dir>/smb/wire/gen -o codec_gen.go create.go
//
// A field's Go type states a fixed field: uint8, uint16, uint32, uint64 or a
// type named over one; [N]byte; a blank (_) for a Reserved field. Its smb2 tag
// states the rest, in MS-SMB2's field order:
//
//   - size=N on a blank uint16: a StructureSize, refused unless N; an odd N
//     counts the variable part's first byte, refused when missing and written
//     when the variable part is empty, but where empty says that part may be
//     empty, as a reply's may;
//   - name=L on a blank uint16 or uint32: a layout field, an offset, length or
//     count the encoder fills from the field it describes;
//   - offset=L, len=L, count=L: where a variable field lies and how long it
//     is, in bytes or members; with no offset it follows the field before;
//   - max=N: the most a length or count may be, which every variable field
//     carries;
//   - align=N on a field with an offset: the multiple its offset must be;
//   - utf16 on a string: UTF-16LE;
//   - chain on a []T with offset and count: each member at the next multiple
//     of 8 (negotiate contexts); chain=L on a []T with offset and len: each
//     found from the one before by its first field, a blank uint32 named L
//     (create contexts);
//   - union on a []byte or integer discriminant, case=V on each arm, a struct
//     whose bytes the discriminant's value V chooses, and default on one
//     []byte arm, the bytes of any other: the arms share one offset and len;
//   - exact on an arm: its data is refused unless its struct takes every byte
//     of it (MS-SMB2 3.3.5.9's create contexts);
//   - since=D: on the wire from dialect D on;
//   - if=L:V on a chain: on the wire only when L, a []uint16 field before it,
//     holds V (a NEGOTIATE request's negotiate contexts, there when its
//     Dialects hold 0x0311).
//
// []byte, string, []uint16 (with count), []T and an arm T take tags; a field
// the generator cannot honour fails generation, naming the field, and nothing
// is written.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	out := flag.String("o", "", "the file to write, in the inputs' directory")
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: gen -o codec_gen.go file.go...")
		os.Exit(2)
	}
	src, err := generate(".", flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, src, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
