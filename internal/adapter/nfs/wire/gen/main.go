// Command gen writes XDR's codec (RFC 4506) for the struct types declared in
// the Go files it is named, in one file beside them: DecodeWire, EncodeWire,
// MaxWire and IsZero, which RFC 20 section 4.7's Decoder and Encoder require,
// as ordinary Go over nfs/wire's runtime, so nothing is looked up while a
// request is served (RFC 20 section 5.1). go generate runs it from the
// package's directory:
//
//	//go:generate go run <dir>/nfs/wire/gen -o codec_gen.go read.go write.go
//
// A field's Go type states its fixed form: uint32, uint64, int64, bool, or a
// type named over one (an enum); [N]byte, a fixed-length opaque; [N] of one of
// those, a fixed-length array; a struct declared beside it; *T of such a
// struct, optional data. Its xdr tag states the rest: max=N, the bound of a
// string, a []byte opaque or a []T array of integers, bools or structs, which
// each must carry; union, on a union's discriminant, a uint32 and its struct's
// first field, every other field an arm tagged case=V or default, a void arm a
// blank field of type struct{}; since=N, a field or arm on the wire from minor
// version N on. A field it cannot honour fails generation, naming the field,
// and nothing is written.
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
