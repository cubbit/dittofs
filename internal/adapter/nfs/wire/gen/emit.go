package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

func pad4(n int) int { return n + (4-n%4)%4 }

// least is a lower bound on the bytes an array's element of t encodes to,
// which bounds a claimed count by the bytes left before the array is made: a
// word for anything but a struct, every XDR item taking at least one (RFC 4506
// section 3); a union's discriminant for a union, whose arm may be void; and
// for any other struct, its fields' on the wire in every version.
func (g *gen) least(t *xtype) int {
	s := g.byName[t.name]
	if t.kind != kStruct || s.union {
		return 4
	}
	n := 0
	for _, f := range s.fields {
		if f.since == 0 {
			n += g.least(f.t)
		}
	}
	return n
}

// size is the most bytes t encodes to: a count, or, where a struct's MaxWire
// takes part, an expression.
func (g *gen) size(t *xtype) (int, string) {
	switch t.kind {
	case kUint64, kInt64:
		return 8, ""
	case kFixed:
		return pad4(t.n), ""
	case kVector:
		n, _ := g.size(t.elem)
		return t.n * n, ""
	case kOpaque, kString:
		return 4 + pad4(t.n), ""
	case kArray:
		n, expr := g.size(t.elem)
		if expr == "" {
			return 4 + t.n*n, ""
		}
		return 0, fmt.Sprintf("4 + %d*%s", t.n, expr)
	case kOptional:
		_, expr := g.size(t.elem)
		return 0, "4 + " + expr
	case kStruct:
		return 0, fmt.Sprintf("(*%s)(nil).MaxWire(v)", t.name)
	}
	return 4, ""
}

func (g *gen) sizeExpr(t *xtype) string {
	n, expr := g.size(t)
	if expr == "" {
		return strconv.Itoa(n)
	}
	return expr
}

func goType(t *xtype) string {
	switch {
	case t.name != "":
		return t.name
	case t.kind == kFixed:
		return fmt.Sprintf("[%d]byte", t.n)
	case t.kind == kVector:
		return fmt.Sprintf("[%d]%s", t.n, goType(t.elem))
	case t.kind == kArray:
		return "[]" + goType(t.elem)
	}
	return [...]string{kUint32: "uint32", kUint64: "uint64", kInt64: "int64", kBool: "bool"}[t.kind]
}

// prims is how the runtime reads and writes each basic kind, and the Go type
// it reads and writes, which a field of another type converts to and from.
var prims = map[kind]struct{ read, write, typ string }{
	kUint32: {"d.Uint32()", "e.Uint32", "uint32"},
	kUint64: {"d.Uint64()", "e.Uint64", "uint64"},
	kInt64:  {"d.Uint64()", "e.Uint64", "uint64"},
	kBool:   {"d.Bool()", "e.Bool", "bool"},
}

func (g *gen) decode(b *bytes.Buffer, lv string, t *xtype, depth int) {
	switch t.kind {
	case kUint32, kUint64, kInt64, kBool:
		p, val := prims[t.kind], prims[t.kind].read
		if goType(t) != p.typ {
			val = goType(t) + "(" + val + ")"
		}
		fmt.Fprintf(b, "%s = %s\n", lv, val)
	case kFixed:
		fmt.Fprintf(b, "d.Fixed(%s[:])\n", lv)
	case kOpaque:
		fmt.Fprintf(b, "%s = d.Opaque(%d)\n", lv, t.n)
	case kString:
		fmt.Fprintf(b, "%s = d.String(%d)\n", lv, t.n)
	case kVector:
		fmt.Fprintf(b, "for i%d := range %s {\n", depth, lv)
		g.decode(b, fmt.Sprintf("%s[i%d]", lv, depth), t.elem, depth+1)
		b.WriteString("}\n")
	case kArray:
		fmt.Fprintf(b, "if n%[1]d := d.Count(%[2]d, %[3]d); n%[1]d > 0 {\n%[4]s = make(%[5]s, n%[1]d)\nfor i%[1]d := range %[4]s {\n",
			depth, t.n, g.least(t.elem), lv, goType(t))
		g.decode(b, fmt.Sprintf("%s[i%d]", lv, depth), t.elem, depth+1)
		b.WriteString("}\n}\n")
	case kOptional:
		fmt.Fprintf(b, "if d.Bool() {\n%[1]s = new(%[2]s)\n%[1]s.decodeXDR(d, v)\n}\n", lv, t.elem.name)
	case kStruct:
		fmt.Fprintf(b, "%s.decodeXDR(d, v)\n", lv)
	}
}

func (g *gen) encode(b *bytes.Buffer, lv string, t *xtype, depth int) {
	switch t.kind {
	case kUint32, kUint64, kInt64, kBool:
		p, val := prims[t.kind], lv
		if goType(t) != p.typ {
			val = p.typ + "(" + lv + ")"
		}
		fmt.Fprintf(b, "%s(%s)\n", p.write, val)
	case kFixed:
		fmt.Fprintf(b, "e.Fixed(%s[:])\n", lv)
	case kOpaque:
		fmt.Fprintf(b, "e.Opaque(%s, %d)\n", lv, t.n)
	case kString:
		fmt.Fprintf(b, "e.String(%s, %d)\n", lv, t.n)
	case kVector:
		fmt.Fprintf(b, "for i%d := range %s {\n", depth, lv)
		g.encode(b, fmt.Sprintf("%s[i%d]", lv, depth), t.elem, depth+1)
		b.WriteString("}\n")
	case kArray:
		fmt.Fprintf(b, "e.Count(len(%[2]s), %[3]d)\nfor i%[1]d := range %[2]s {\n", depth, lv, t.n)
		g.encode(b, fmt.Sprintf("%s[i%d]", lv, depth), t.elem, depth+1)
		b.WriteString("}\n")
	case kOptional:
		fmt.Fprintf(b, "e.Bool(%[1]s != nil)\nif %[1]s != nil {\n%[1]s.encodeXDR(e, v)\n}\n", lv)
	case kStruct:
		fmt.Fprintf(b, "%s.encodeXDR(e, v)\n", lv)
	}
}

func zero(lv string, t *xtype) string {
	switch t.kind {
	case kBool:
		return "!" + lv
	case kString:
		return lv + ` == ""`
	case kOpaque, kArray:
		return "len(" + lv + ") == 0"
	case kFixed, kVector:
		return lv + " == " + goType(t) + "{}"
	case kOptional:
		return lv + " == nil"
	case kStruct:
		return lv + ".IsZero()"
	}
	return lv + " == 0"
}

// since writes body, under a check of the version where the field is not on
// the wire in every one.
func since(b *bytes.Buffer, n int, body func()) {
	if n == 0 {
		body()
		return
	}
	fmt.Fprintf(b, "if v >= %d {\n", n)
	body()
	b.WriteString("}\n")
}

// methods writes s's four methods, and the two its decoder and encoder nest
// through.
//
// decision: the version is a uint32 where RFC 20 section 4.7's Decoder and
// Encoder name core.Version: section 7.1 holds a codec_gen.go to wire's
// imports, among which core is not. Overturned when the RFC settles which
// gives way.
func (g *gen) methods(b *bytes.Buffer, s *strct) {
	fmt.Fprintf(b, `
func (x *%[1]s) DecodeWire(b []byte, v uint32) (int, error) {
	d := wire.Decoder{B: b}
	x.decodeXDR(&d, v)
	return d.N, d.Err
}

func (x *%[1]s) EncodeWire(v uint32, out *resources.Buf) error {
	e := wire.Encoder{B: out.B}
	x.encodeXDR(&e, v)
	if e.Err != nil {
		return e.Err
	}
	out.B = e.B
	return nil
}
`, s.name)

	fields := s.fields
	if s.union {
		fields = s.fields[:1]
	}
	fmt.Fprintf(b, "\nfunc (x *%s) decodeXDR(d *wire.Decoder, v uint32) {\n", s.name)
	for _, f := range fields {
		since(b, f.since, func() { g.decode(b, "x."+f.name, f.t, 0) })
	}
	if s.union {
		g.arms(b, s, "d", func(f field) { g.decode(b, "x."+f.name, f.t, 0) })
	}
	b.WriteString("}\n")

	fmt.Fprintf(b, "\nfunc (x *%s) encodeXDR(e *wire.Encoder, v uint32) {\n", s.name)
	for _, f := range fields {
		since(b, f.since, func() { g.encode(b, "x."+f.name, f.t, 0) })
	}
	if s.union {
		g.arms(b, s, "e", func(f field) { g.encode(b, "x."+f.name, f.t, 0) })
	}
	b.WriteString("}\n")

	fmt.Fprintf(b, "\nfunc (*%s) MaxWire(v uint32) int {\n", s.name)
	g.maxWire(b, s)
	b.WriteString("}\n")

	var terms []string
	for _, f := range s.fields {
		if f.name != "_" {
			terms = append(terms, zero("x."+f.name, f.t))
		}
	}
	fmt.Fprintf(b, "\nfunc (x *%s) IsZero() bool {\nreturn %s\n}\n", s.name, strings.Join(terms, " &&\n"))
}

// arms writes a union's switch on its discriminant: an arm is chosen only in
// the versions it is on the wire in, and a value that chooses none takes the
// default arm, or is refused (RFC 4506 section 4.15).
func (g *gen) arms(b *bytes.Buffer, s *strct, coder string, body func(field)) {
	disc := s.fields[0]
	b.WriteString("switch {\n")
	var def *field
	for _, f := range s.fields[1:] {
		if f.arm == "default" {
			def = &f
			continue
		}
		cond := fmt.Sprintf("x.%s == %s", disc.name, f.arm)
		if f.since > 0 {
			cond += fmt.Sprintf(" && v >= %d", f.since)
		}
		fmt.Fprintf(b, "case %s:\n", cond)
		body(f)
	}
	b.WriteString("default:\n")
	if def != nil {
		body(*def)
	} else {
		fmt.Fprintf(b, "%s.NoArm(uint32(x.%s))\n", coder, disc.name)
	}
	b.WriteString("}\n")
}

// maxWire writes MaxWire's body: the most s encodes to at v, under its tags'
// maxima, each field and arm counted only where it is on the wire.
func (g *gen) maxWire(b *bytes.Buffer, s *strct) {
	fields, total, acc := s.fields, "n", "n += %s\n"
	if s.union {
		fields, total, acc = s.fields[1:], "arm", "arm = max(arm, %s)\n"
	}
	fixed := 0
	var lines []string
	for _, f := range fields {
		if f.t.kind == kVoid {
			continue
		}
		n, expr := g.size(f.t)
		switch {
		case f.since > 0:
			lines = append(lines, fmt.Sprintf("if v >= %d {\n"+acc+"}\n", f.since, g.sizeExpr(f.t)))
		case expr != "":
			lines = append(lines, fmt.Sprintf(acc, expr))
		case s.union:
			fixed = max(fixed, n)
		default:
			fixed += n
		}
	}
	head := 0
	if s.union {
		head = 4
	}
	if len(lines) == 0 {
		fmt.Fprintf(b, "return %d\n", head+fixed)
		return
	}
	fmt.Fprintf(b, "%s := %d\n%s", total, fixed, strings.Join(lines, ""))
	if s.union {
		b.WriteString("return 4 + arm\n")
	} else {
		b.WriteString("return n\n")
	}
}
