package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A field the generator cannot honour fails generation, naming the field,
// and nothing is written: a variable-length field with no maximum, a tag it
// does not know or that does not fit the field, and a type it cannot bound
// (RFC 20 sections 5.1 and 5.2).
func TestGenerateRefuses(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"type S struct { Name string }", "S.Name: a string with no maximum"},
		{"type S struct { B []byte }", "S.B: a variable-length opaque or array with no maximum"},
		{"type S struct { A []uint32 }", "S.A: a variable-length opaque or array with no maximum"},
		{"type S struct { A []string `xdr:\"max=2\"` }", "S.A: a variable-length array of a type other than an integer, a bool or a struct"},
		{"type S struct { A []struct{} `xdr:\"max=2\"` }", "S.A: a variable-length array of a type other than an integer, a bool or a struct"},
		{"type S struct { P *uint32 }", "S.P: optional data of a type other than a struct"},
		{"type S struct { P *int }", "S.P: optional data of a type other than a struct"},
		{"type S struct { P uint32 `xdr:\"arm\"` }", "S.P: arm on a type other than a pointer to a struct"},
		{"type S struct { A uint32 `xdr:\"min=1\"` }", `S.A: "min=1" is no xdr tag`},
		{"type S struct { A uint32 `xdr:\"since=0\"` }", `S.A: since="0" is not a count above zero`},
		{"type S struct { A uint32 `xdr:\"max=4\"` }", "S.A: max on a type of fixed length"},
		{"type S struct { N int }", "S.N: a type XDR's codec does not know: int"},
		{"type S struct { M map[string]uint32 }", "S.M: a type XDR's codec does not know: map[string]uint32"},
		{"type S struct { A [3]T }\ntype T struct{ X uint32 }", "S.A: a fixed-length array of a type other than an integer or a bool"},
		{"type S struct { A [3]int }", "S.A: a fixed-length array of a type other than an integer or a bool"},
		{"const n = 3\ntype S struct { A [n]uint32 }", "S.A: a fixed length that is not a literal count above zero"},
		{"type S struct { uint32 }", "S: an embedded field"},
		{"type S struct { _ struct{} }", "S._: a void arm is a blank field of type struct{}"},
		{"type S struct { A uint32 `xdr:\"case=1\"` }", "S.A: an arm outside a union"},
		{"type S struct { A uint32; D uint32 `xdr:\"union\"` }", "S.D: a union's discriminant is its struct's first field"},
		{"type U struct { D uint64 `xdr:\"union\"`; A uint32 `xdr:\"case=1\"` }", "U.D: a discriminant is a uint32"},
		{"type U struct { D uint32 `xdr:\"union,since=1\"`; A uint32 `xdr:\"case=1\"` }", "U.D: a discriminant is a uint32 on the wire in every version"},
		{"type U struct { D uint32 `xdr:\"union\"`; A uint32 }", "U.A: a union's field that is no arm"},
		{"type U struct { D uint32 `xdr:\"union\"`; A uint32 `xdr:\"case=x\"` }", `U.A: case="x" is not one uint32`},
		{"type U struct { D uint32 `xdr:\"union\"`; A, B uint32 `xdr:\"case=1\"` }", "U.B: a second arm for 1"},
		{"type U struct { D uint32 `xdr:\"union\"`; _ struct{} `xdr:\"default,since=1\"` }", "U._: a default arm takes no case and no since"},
		{"type U struct { D uint32 `xdr:\"union\"`; _ struct{} `xdr:\"default,case=2\"` }", "U._: a default arm takes no case and no since"},
		{"type U struct { D uint32 `xdr:\"union\"` }", "U: a union with no arm"},
		{"type L struct { Next *L }", "L.Next: a type that holds itself"},
		{"type A struct { B []B `xdr:\"max=2\"` }\ntype B struct { A *A }", "a type that holds itself"},
		{"type N uint32", "no struct types"},
	} {
		t.Run(c.want, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "in.go"), []byte("package p\n\n"+c.src+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			src, err := generate(dir, []string{"in.go"})
			if err == nil {
				t.Fatalf("generated:\n%s", src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refused with %v, want %q", err, c.want)
			}
		})
	}
}

// A count is checked against the bytes left at its elements' least, never more
// than an element takes: a struct's fields on the wire in every version, a
// hyper, a fixed-length opaque or a fixed-length array at its size and any
// other at a word, and a union's discriminant alone, since its arm may be void.
func TestCountsBoundElementsAtTheirLeast(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\n" +
		"type S struct {\n\tA []E `xdr:\"max=2\"`\n\tB []U `xdr:\"max=2\"`\n\tC []F `xdr:\"max=2\"`\n}\n" +
		"type E struct {\n\tX uint32\n\tY uint64 `xdr:\"since=1\"`\n\tZ [8]byte\n}\n" +
		"type U struct {\n\tD uint32 `xdr:\"union\"`\n\tX uint64 `xdr:\"case=1\"`\n}\n" +
		"type F struct {\n\tH uint64\n\tI int64\n\tW [3]uint32\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "in.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := generate(dir, []string{"in.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"d.Count(2, 12); n0 > 0 {\n\t\tx.A = make([]E, n0)",
		"d.Count(2, 4); n0 > 0 {\n\t\tx.B = make([]U, n0)",
		"d.Count(2, 28); n0 > 0 {\n\t\tx.C = make([]F, n0)",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("no %q in:\n%s", want, out)
		}
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

// Every generated file under the NFS adapter is what a fresh run of the
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
