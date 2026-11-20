package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generateFrom runs the generator on src, a file of package p.
func generateFrom(t *testing.T, src string) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "in.go"), []byte("package p\n\n"+src+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return generate(dir, []string{"in.go"})
}

// A field the generator cannot honour fails generation, naming the field,
// and nothing is written: a variable-length field with no maximum, a tag it
// does not know or that does not fit the field, and every layout its tags
// cannot state (RFC 20 sections 5.1 and 5.2).
func TestGenerateRefuses(t *testing.T) {
	const (
		layout = "_ uint16 `smb2:\"name=L\"`\n"
		union  = "D uint16 `smb2:\"union\"`\n_ uint16 `smb2:\"name=L\"`\n"
		def    = "X []byte `smb2:\"len=L,max=4,default\"`\n"
		arm    = "type T struct{ A uint32 }\n"
		chain  = "_ uint32 `smb2:\"name=O\"`\n_ uint16 `smb2:\"name=C\"`\n_ uint16 `smb2:\"name=N\"`\n"
		// counted is a counted chain's layout fields, 6 bytes.
		counted = "_ uint32 `smb2:\"name=O\"`\n_ uint16 `smb2:\"name=C\"`\n"
		past    = ": a largest encoding past 2147483647 bytes"
	)
	for _, c := range []struct{ src, want string }{
		{"type S struct {\n" + layout + "B []byte `smb2:\"len=L\"`\n}", "S.B: a variable-length field with no maximum"},
		{"type S struct {\n" + layout + "N string `smb2:\"len=L,utf16\"`\n}", "S.N: a variable-length field with no maximum"},
		{"type S struct { A uint32 `smb2:\"min=1\"` }", `S.A: a tag it does not know, "min"`},
		{"type S struct { A uint32 `smb2:\"max=4\"` }", `S.A: a tag it cannot honour on this field's type, "max"`},
		{"type S struct { A uint32 `smb2:\"since=x\"` }", "S.A: tag since=x is not a dialect served"},
		{"type S struct { A uint32 `smb2:\"since=0311\"` }", "S.A: tag since=0311 is not a dialect served"},
		{"type S struct { A uint32 `smb2:\"since=785\"` }", "S.A: tag since=785 is not a dialect served"},
		{"type S struct { A uint32 `smb2:\"since=0x202\"` }", "S.A: tag since=0x202 is not a dialect served"},
		{"type S struct {\n" + layout + "N int\n}", "S.N: a type it cannot encode, int"},
		{"type S struct {\n" + layout + "M map[string]uint32\n}", "S.M: a type it cannot encode"},
		{"type S struct {\n" + layout + "A []uint32\n}", "S.A: an array or slice it cannot encode"},
		{"type S struct {\n" + layout + "A [0]byte\n}", "S.A: an array or slice it cannot encode"},
		{"type S struct { A, B uint32 `smb2:\"name=L\"` }", "S: one name per field"},
		{"type S struct {\n" + layout + "uint32\n}", "S: one name per field"},
		{"type S struct { uint32 }", "no wire struct to generate a codec for"},
		{"type S struct { B []byte `smb2:\"len=L,max=4\"` }", "S.B: no layout field named L before it"},
		{"type S struct {\n" + layout + "B []byte `smb2:\"len=L,max=4,align=8\"`\n}", "S.B: align without an offset"},
		{"type S struct { A uint16 `smb2:\"size=4\"` }", "S.A: size and name go on a blank field"},
		{"type S struct { _ uint32 `smb2:\"size=4\"` }", "S._: a StructureSize is a uint16"},
		{"type S struct { _ uint16 `smb2:\"size=4,empty\"` }", "S._: empty on no odd StructureSize"},
		{"type S struct { _ uint8 `smb2:\"name=L\"` }", "S._: a layout field is a uint16 or a uint32"},
		{"type S struct {\n" + layout + layout + "}", "S._: a second layout field named L"},
		{"type S struct {\n" + layout + "_ []byte `smb2:\"len=L,max=4\"`\n}", "S._: a variable field that is blank"},
		{"type S struct {\n_ uint16 `smb2:\"name=L,since=0x311\"`\nB []byte `smb2:\"len=L,max=4\"`\n}", "S.B: its layout field L on the wire in fewer dialects than it"},
		{"type S struct {\n" + layout + "B []byte `smb2:\"len=L,max=4\"`\nC []byte `smb2:\"len=L,max=4\"`\n}", "S.C: a second field located or sized by L"},
		{"type S struct {\n" + layout + "B []byte `smb2:\"max=4\"`\n}", "S.B: a variable-length field with no len"},
		{"type S struct {\n" + layout + "N string `smb2:\"len=L,max=4\"`\n}", "S.N: a string with no utf16"},
		{"type S struct { A []uint16 `smb2:\"max=4\"` }", "S.A: a []uint16 with no count"},
		{"type S struct {\n" + layout + "C []T `smb2:\"count=L,max=4\"`\n}\n" + arm, "S.C: a []T with no chain"},
		{"type S struct {\n" + layout + "C []T `smb2:\"count=L,max=4,chain\"`\n}\n" + arm, "S.C: a chain with no offset"},
		{"type S struct {\n" + layout + "C []T `smb2:\"offset=L,len=L,max=4,chain\"`\n}\n" + arm, "S.C: a counted chain takes count, not len"},
		{"type S struct {\n" + layout + "C []T `smb2:\"offset=L,count=L,max=4,chain=N\"`\n}\n" + arm, "S.C: a linked chain takes len, not count"},
		{"type S struct {\n" + layout + "C []T `smb2:\"offset=L,len=L,max=4,chain=N\"`\n}\n" + arm, "S.C: a linked chain's member T does not begin with a blank uint32 named N"},
		{"type S struct {\n" + counted + "M []T `smb2:\"offset=O,count=C,max=2,align=2,chain\"`\n}\n" + arm, "S.M: a counted chain aligned to 2, not a multiple of 8"},
		{"type S struct {\n" + counted + "M []T `smb2:\"offset=O,count=C,max=2,chain\"`\n}\n" + arm, "S.M: a counted chain aligned to 1, not a multiple of 8"},
		{"type S struct {\n" + layout + "A T `smb2:\"len=L\"`\n}\n" + arm, "S.A: a struct field that is no union's arm: it has no case"},
		{"type S struct {\n" + union + "A S `smb2:\"len=L,case=1\"`\n" + def + "}", "S.A: a type that holds itself, so no maximum"},
		{"type S struct {\n" + layout + "A T `smb2:\"len=L,case=1\"`\n" + def + "}\n" + arm, "S.A: an arm with no union discriminant"},
		{"type S struct {\n" + union + "A T `smb2:\"len=L,case=1\"`\n}\n" + arm, "S.D: a union with no default arm"},
		{"type S struct {\n_ uint16 `smb2:\"name=L\"`\nA T `smb2:\"len=L,case=1\"`\nD uint16 `smb2:\"union\"`\n" + def + "}\n" + arm, "S.A: an arm before its union's discriminant"},
		{"type S struct {\n" + union + "A T `smb2:\"len=L,align=8,case=1\"`\n" + def + "}\n" + arm, "S.A: an arm not located as its union's default"},
		{"type S struct {\n" + union + "A T `smb2:\"len=L,case=x\"`\n" + def + "}\n" + arm, `S.A: a case its integer discriminant cannot hold, "x"`},
		{"type S struct {\n" + layout + "B []byte `smb2:\"len=L,max=4,known=1\"`\n}", "S.B: known on no default arm"},
		{"type S struct {\n" + union + "X []byte `smb2:\"len=L,max=4,default,known=x\"`\n}", `S.X: a known value its integer discriminant cannot hold, "x"`},
		{"type S struct {\n" + union + "A T `smb2:\"len=L,case=1\"`\nX []byte `smb2:\"len=L,max=4,default,known=1\"`\n}\n" + arm, `S.X: a known value an arm takes, "1"`},
		{"type S struct {\n" + union + "E uint16 `smb2:\"union\"`\n" + def + "}", "S.E: a second union discriminant"},
		{"type S struct { _ uint16 `smb2:\"union\"` }", "S._: a union's discriminant is a named field"},
		{"type S struct {\n" + union + "A T8 `smb2:\"len=L,case=1\"`\n" + def + "}\ntype T8 struct{ A uint64 }", "S.A: an arm whose maximum 8 exceeds its union's default 4"},
		{"type S struct {\n" + layout + "B []byte `smb2:\"len=L,max=4\"`\nA uint32\n}", "S.A: a fixed field after a variable one"},
		{"type S struct {\n" + layout + "_ uint16 `smb2:\"name=U\"`\nB []byte `smb2:\"len=L,max=4\"`\n}", "S._: a layout field no field uses"},
		{"type S struct {\n" + chain + "D []uint16 `smb2:\"count=N,max=4\"`\nM []T `smb2:\"offset=O,count=C,max=2,align=8,chain,if=D\"`\n}\n" + arm, "S.M: tag if=D is not L:V, a field and the uint16 it may hold"},
		{"type S struct {\n" + chain + "B []byte `smb2:\"len=N,max=4\"`\nM []T `smb2:\"offset=O,count=C,max=2,align=8,chain,if=B:0x311\"`\n}\n" + arm, "S.M: if=B names no []uint16 field before it"},
		{"type S struct {\n" + chain + "M []T `smb2:\"offset=O,count=C,max=2,align=8,chain,if=D:0x311\"`\nD []uint16 `smb2:\"count=N,max=4\"`\n}\n" + arm, "S.M: if=D names no []uint16 field before it"},
		// A largest encoding past 2^31-1 bytes: 8 + 7 + 2147483633 with the
		// padding its align may need; 2 + 2*1073741823; 8 + 2147483640 for a
		// linked chain, its maximum alone; 13 + 65536*(7 + 65540) for a
		// counted one, each member padded to 8; 13 + 134217728*(7 + 9), a
		// member's 4 bytes filled to its StructureSize of 9; and 4 +
		// 2147483644 for a union, named at the default whose tags bound it.
		{"type S struct {\n_ uint32 `smb2:\"name=O\"`\n_ uint32 `smb2:\"name=L\"`\nB []byte `smb2:\"offset=O,len=L,max=2147483633,align=8\"`\n}", "S.B" + past},
		{"type S struct {\n_ uint16 `smb2:\"name=N\"`\nA []uint16 `smb2:\"count=N,max=1073741823\"`\n}", "S.A" + past},
		{"type S struct {\n_ uint32 `smb2:\"name=O\"`\n_ uint32 `smb2:\"name=L\"`\nC []T `smb2:\"offset=O,len=L,max=2147483640,chain=N\"`\n}\ntype T struct {\n_ uint32 `smb2:\"name=N\"`\n}", "S.C" + past},
		{"type S struct {\n" + counted + "A []T `smb2:\"offset=O,count=C,max=65536,align=8,chain\"`\n}\ntype T struct {\n_ uint32 `smb2:\"name=L\"`\nB []byte `smb2:\"len=L,max=65536\"`\n}", "S.A" + past},
		{"type S struct {\n" + counted + "A []T `smb2:\"offset=O,count=C,max=134217728,align=8,chain\"`\n}\ntype T struct {\n_ uint16 `smb2:\"size=9\"`\n_ uint16 `smb2:\"name=L\"`\nB []byte `smb2:\"len=L,max=0\"`\n}", "S.A" + past},
		{"type S struct {\n" + union + "A T `smb2:\"len=L,case=1\"`\nX []byte `smb2:\"len=L,max=2147483644,default\"`\n}\n" + arm, "S.X" + past},
	} {
		t.Run(c.want, func(t *testing.T) {
			src, err := generateFrom(t, c.src)
			if err == nil {
				t.Fatalf("generated:\n%s", src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refused with %v, want %q", err, c.want)
			}
		})
	}
}

// A counted chain is checked against the bytes left at its members' least,
// never more than a member takes: its fixed fields on the wire in every
// dialect, a uint64 at 8 bytes and a [N]byte at N, so a count past the bytes
// left is refused before the chain is made (RFC 20 section 5.2).
func TestCountsBoundMembersAtTheirLeast(t *testing.T) {
	out, err := generateFrom(t, "type S struct {\n"+
		"_ uint32 `smb2:\"name=O\"`\n_ uint16 `smb2:\"name=C\"`\n"+
		"A []E `smb2:\"offset=O,count=C,max=2,align=8,chain\"`\n}\n"+
		"type E struct {\nX uint64\nY uint32 `smb2:\"since=0x311\"`\nZ [8]byte\n"+
		"_ uint16 `smb2:\"name=L\"`\nB []byte `smb2:\"len=L,max=4\"`\n}")
	if err != nil {
		t.Fatal(err)
	}
	if want := "d.Chain(lO, lC, 2, 18)"; !bytes.Contains(out, []byte(want)) {
		t.Errorf("no %q in:\n%s", want, out)
	}
}

// The largest encoding 2^31-1 bytes allows is taken, a byte field of
// 2147483643 bytes after its 4-byte length, and one byte more is refused. A
// struct past it is refused once, at its innermost field and that field's
// place, not again at the struct whose counted chain or union holds it,
// however far the maxima multiply: a U, T's member, takes 2^34 bytes with
// its padding, and T's 2^30 of them 2^64, which saturates rather than
// wrapping to 13 bytes, so S, which holds T, is not refused as past the bound
// itself.
func TestTheLargestBound(t *testing.T) {
	const length = "type S struct {\n_ uint32 `smb2:\"name=L\"`\n"
	if _, err := generateFrom(t, length+"B []byte `smb2:\"len=L,max=2147483643\"`\n}"); err != nil {
		t.Errorf("a byte field of 2147483643 bytes refused: %v", err)
	}
	if _, err := generateFrom(t, length+"B []byte `smb2:\"len=L,max=2147483644\"`\n}"); err == nil || !strings.Contains(err.Error(), "S.B: a largest encoding past 2147483647 bytes") {
		t.Errorf("a byte field of 2147483644 bytes refused with %v, want its largest encoding refused", err)
	}
	_, err := generateFrom(t, "type S struct {\n_ uint32 `smb2:\"name=O\"`\n_ uint16 `smb2:\"name=C\"`\n"+
		"A []T `smb2:\"offset=O,count=C,max=2,align=8,chain\"`\n}\n"+
		"type T struct {\n_ uint32 `smb2:\"name=L\"`\nB []byte `smb2:\"len=L,max=2147483647\"`\n}")
	if err == nil || strings.Count(err.Error(), "a largest encoding") != 1 || !strings.Contains(err.Error(), "in.go:10:1: T.B: ") {
		t.Errorf("refused with %v, want one refusal, at T.B on line 10", err)
	}
	const counted = "_ uint32 `smb2:\"name=O\"`\n_ uint16 `smb2:\"name=C\"`\n"
	_, err = generateFrom(t, "type S struct {\n"+counted+"A []T `smb2:\"offset=O,count=C,max=107374183,align=8,chain\"`\n}\n"+
		"type T struct {\n"+counted+"A []U `smb2:\"offset=O,count=C,max=1073741824,align=8,chain\"`\n}\n"+
		"type U struct {\n"+counted+"P [12]byte\nA []V `smb2:\"offset=O,count=C,max=1073741822,align=8,chain\"`\n}\n"+
		"type V struct{ P [9]byte }")
	if err == nil || strings.Count(err.Error(), "a largest encoding") != 1 || !strings.Contains(err.Error(), "U.A: ") {
		t.Errorf("refused with %v, want one refusal, at U.A", err)
	}
	// An arm, T, past the bound by its own union's padding is refused at
	// that union's default, and not again at S's, whose region holds it.
	_, err = generateFrom(t, "type S struct {\nD uint16 `smb2:\"union\"`\n_ uint16 `smb2:\"name=L\"`\n"+
		"A T `smb2:\"len=L,case=1\"`\nX []byte `smb2:\"len=L,max=4096,default\"`\n}\n"+
		"type T struct {\nD uint16 `smb2:\"union\"`\n_ uint16 `smb2:\"name=O\"`\n_ uint16 `smb2:\"name=L\"`\n"+
		"X []byte `smb2:\"offset=O,len=L,max=4,align=2147483647,default\"`\n}")
	if err == nil || strings.Count(err.Error(), "a largest encoding") != 1 || !strings.Contains(err.Error(), "T.X: ") {
		t.Errorf("refused with %v, want one refusal, at T.X", err)
	}
}

// MaxWire counts the byte an odd StructureSize takes past the fixed part,
// which Fill writes whatever the fields on the wire take: a size of 9 whose
// since-field is off the wire before 3.0 encodes 9 bytes there, though its
// fields take 8.
func TestMaxWireCountsTheFillByte(t *testing.T) {
	out, err := generateFrom(t, "type S struct {\n"+
		"_ uint16 `smb2:\"size=9\"`\n_ uint16 `smb2:\"name=L\"`\nX uint32\n"+
		"B []byte `smb2:\"len=L,max=4,since=0x300\"`\n}")
	if err != nil {
		t.Fatal(err)
	}
	want := "func (*S) MaxWire(v uint32) int {\n\tn := 8\n\tif v >= 0x300 {\n\t\tn += 4\n\t}\n\treturn max(n, 9)\n}"
	if !bytes.Contains(out, []byte(want)) {
		t.Errorf("no\n%s\nin:\n%s", want, out)
	}
}

// A struct another holds, as a union's arm or a chain's member, gets no
// DecodeWire or EncodeWire, whose offsets would count from a header it does
// not follow; the struct holding it does.
func TestOnlyAStructNoneHoldsIsDecodedAlone(t *testing.T) {
	out, err := generateFrom(t, "type S struct {\n"+
		"D uint16 `smb2:\"union\"`\n_ uint16 `smb2:\"name=L\"`\n_ uint32 `smb2:\"name=O\"`\n_ uint16 `smb2:\"name=C\"`\n"+
		"A A `smb2:\"len=L,case=1\"`\nX []byte `smb2:\"len=L,max=4,default\"`\n"+
		"M []M `smb2:\"offset=O,count=C,max=2,align=8,chain\"`\n}\n"+
		"type A struct{ V uint32 }\ntype M struct{ V uint32 }")
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"S", "A", "M"} {
		for _, method := range []string{"DecodeWire", "EncodeWire"} {
			if has := bytes.Contains(out, []byte("func (x *"+typ+") "+method+"(")); has != (typ == "S") {
				t.Errorf("%s.%s generated: %v", typ, method, has)
			}
		}
	}
}

// A file keeps what its handler needs beside its wire structs: a struct
// with no tagged field gets no codec, and a field nothing can encode, on a
// struct the codec never sees, stops nothing.
func TestAnUntaggedStructGetsNoCodec(t *testing.T) {
	out, err := generateFrom(t, "type S struct {\n"+
		"_ uint16 `smb2:\"name=L\"`\n"+
		"B []byte `smb2:\"len=L,max=4\"`\n}\n"+
		"type h struct{ s fmt.Stringer; n uint32 }")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("func (x *S) DecodeWire(")) {
		t.Errorf("no codec for the tagged struct:\n%s", out)
	}
	if bytes.Contains(out, []byte("func (x *h) ")) {
		t.Errorf("the untagged struct gained a codec:\n%s", out)
	}
}

// stale walks root for the files the generator wrote, known by their header,
// and returns how many it read and each that differs from a fresh run on the
// inputs its header names (RFC 20 section 5.1).
func stale(t *testing.T, root string) (checked int, differ []string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		first, _, _ := strings.Cut(string(b), "\n")
		inputs, ok := strings.CutPrefix(first, header)
		if !ok {
			return nil
		}
		inputs, _ = strings.CutSuffix(inputs, "; DO NOT EDIT.")
		checked++
		if fresh, err := generate(filepath.Dir(path), strings.Fields(inputs)); err != nil || !bytes.Equal(fresh, b) {
			differ = append(differ, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return checked, differ
}

// Every generated file under the SMB adapter is what a fresh run of the
// generator writes, so a hand edit, or a change to the generator or to the
// structs not regenerated, fails here.
func TestGeneratedCodeIsFresh(t *testing.T) {
	checked, differ := stale(t, filepath.Join("..", ".."))
	if checked == 0 {
		t.Fatal("no generated file found: the walk matches nothing")
	}
	for _, path := range differ {
		t.Errorf("%s differs from a fresh run: run go generate in its directory", path)
	}
}

// Over its seed, a generated file edited by hand, the check reports it: a
// check that matched nothing, or compared nothing, would pass the tree too.
func TestFreshnessCheckFindsAHandEdit(t *testing.T) {
	checked, differ := stale(t, filepath.Join("testdata", "stale"))
	if checked != 1 || len(differ) != 1 {
		t.Errorf("read %d generated files and found %v differing, want the seed's one", checked, differ)
	}
}
