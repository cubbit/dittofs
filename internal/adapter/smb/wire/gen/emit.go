package main

import (
	"bytes"
	"fmt"
	"go/format"
	"slices"
	"strconv"
	"strings"
)

// emit writes the codec of every struct, in the order the inputs declare
// them.
//
// decision: the dialect is a uint32 where RFC 20 section 4.7's Decoder and
// Encoder name core.Version: section 7.1 holds a codec_gen.go to wire's
// imports, among which core is not. Overturned when the RFC settles which
// gives way.
//
// decision: a byte field is appended to out, never lent as a segment of its
// own through (*Buf).Attach (RFC 20 section 5.3): no signature of section 4.7
// carries a second segment yet, and epic 6's section 9 leaves the encoder's
// half to the first READ. Overturned when section 4.7 or 6.8 names the call.
func (g *gen) emit(pkg string, files []string) ([]byte, error) {
	var b bytes.Buffer
	std := "" // the standard library's imports: slices for an if= gate
	for _, s := range g.structs {
		if slices.ContainsFunc(s.fields, func(f *field) bool { return f.ifList != "" }) {
			std = "\t\"slices\"\n\n"
		}
	}
	fmt.Fprintf(&b, "%s%s; DO NOT EDIT.\n\npackage %s\n\nimport (\n%s\t%q\n\t%q\n)\n",
		header, strings.Join(files, " "), pkg, std, wirePath, resourcesPath)
	// A struct another holds, as an arm or a chain's member, is read and
	// written through its holder alone: its offsets count from its own first
	// byte, not from a header, so it has no DecodeWire or EncodeWire.
	held := map[string]bool{}
	for _, s := range g.structs {
		for _, f := range s.fields {
			if f.kind == kArm || f.kind == kChain {
				held[f.typ] = true
			}
		}
	}
	for _, s := range g.structs {
		n := "d.N"
		if odd := carried(s); odd > 0 {
			n = fmt.Sprintf("max(d.N, %d)", odd)
		}
		// A nil reply writes nothing: a failure's body is the header codec's
		// ERROR response, and core hands an encoder no reply for a request it
		// refused or a handler that failed with none (RFC 20 section 4.7, RFC 22
		// section 16.5).
		if !held[s.name] {
			fmt.Fprintf(&b, `
func (x *%[1]s) DecodeWire(b []byte, v uint32) (int, error) {
	d := wire.Decoder{B: b, Base: wire.HeaderLen}
	x.decodeSMB2(&d, v)
	return %[2]s, d.Err
}

func (x *%[1]s) EncodeWire(v uint32, out *resources.Buf) error {
	if x == nil {
		return nil
	}
	e := wire.Encoder{B: out.B, Origin: len(out.B) - wire.HeaderLen}
	x.encodeSMB2(&e, v)
	if e.Err != nil {
		return e.Err
	}
	out.B = e.B
	return nil
}
`, s.name, n)
		}
		g.decode(&b, s)
		g.encode(&b, s)
		g.maxWire(&b, s)
		isZero(&b, s)
	}
	return format.Source(b.Bytes())
}

// used returns the labels s's variable fields are located and sized by.
func used(s *strct) []string {
	var ls []string
	for _, f := range s.fields {
		for _, l := range []string{f.offset, f.length, f.count} {
			if l != "" && !slices.Contains(ls, l) {
				ls = append(ls, l)
			}
		}
	}
	return ls
}

// width returns the width of the layout field labelled l.
func width(s *strct, l string) int {
	for _, f := range s.fields {
		if f.label == l {
			return f.n
		}
	}
	return 0
}

// gate opens f's checks, if it has any, and returns what closes them: the
// dialect it is on the wire from, and, where a value x is at hand, the value
// its if= list must hold. MaxWire has no value: it counts such a field.
//
// A request's own list gates its field, not the dialect v, since a NEGOTIATE
// request is read before any dialect is chosen: its negotiate contexts are on
// the wire when its Dialects hold 0x0311, and the same eight bytes are
// otherwise ClientStartTime, which the server ignores (MS-SMB2 2.2.3; RFC 20
// section 5.1, the dialects a NEGOTIATE request offers).
func gate(b *bytes.Buffer, f *field, x bool) string {
	var conds []string
	if f.since != "" {
		conds = append(conds, "v >= "+f.since)
	}
	if x && f.ifList != "" {
		conds = append(conds, fmt.Sprintf("slices.Contains(x.%s, %s)", f.ifList, f.ifValue))
	}
	if len(conds) == 0 {
		return ""
	}
	fmt.Fprintf(b, "if %s {\n", strings.Join(conds, " && "))
	return "}\n"
}

// region is where a variable field's bytes lie, decoded, refused past limit.
func region(f *field, limit string) string {
	if f.offset != "" {
		return fmt.Sprintf("d.At(l%s, l%s, %s, %d)", f.offset, f.length, limit, f.align)
	}
	return fmt.Sprintf("d.Next(l%s, %s)", f.length, limit)
}

// discriminant is the expression a union's switch reads, and how a case
// value is written for it.
func discriminant(d *field) (string, func(string) string) {
	if d.kind == kBytes {
		return "string(x." + d.name + ")", strconv.Quote
	}
	return "x." + d.name, func(v string) string { return v }
}

// empties says, as one expression, that every arm but the chosen one holds
// no value: a union's encode writes the one arm its switch named, and any
// other arm's value would silently stay behind, so it is refused.
func empties(arms []*field, chosen *field) string {
	var terms []string
	for _, a := range arms {
		if a == chosen {
			continue
		}
		if a.kind == kArm {
			terms = append(terms, "x."+a.name+".IsZero()")
		} else {
			terms = append(terms, "len(x."+a.name+") == 0")
		}
	}
	return strings.Join(terms, " && ")
}

func (g *gen) decode(b *bytes.Buffer, s *strct) {
	fmt.Fprintf(b, "\nfunc (x *%s) decodeSMB2(d *wire.Decoder, v uint32) {\n", s.name)
	labels := used(s)
	if len(labels) > 0 {
		fmt.Fprintf(b, "var l%s int\n", strings.Join(labels, ", l"))
	}
	disc, arms := s.disc()
	for _, f := range s.fields {
		if (f.kind == kArm || f.def) && f != arms[0] {
			continue
		}
		end := gate(b, f, true)
		switch {
		case f.empty:
			fmt.Fprintf(b, "d.Size(%d, %d)\n", f.size, f.size-1)
		case f.size > 0:
			fmt.Fprintf(b, "d.Size(%d, %d)\n", f.size, f.size)
		case slices.Contains(labels, f.label):
			fmt.Fprintf(b, "l%s = int(d.Uint%d())\n", f.label, 8*f.n)
		case f.name == "_":
			fmt.Fprintf(b, "d.Skip(%d)\n", f.n)
		case f.kind == kInt && f.typ != "":
			fmt.Fprintf(b, "x.%s = %s(d.Uint%d())\n", f.name, f.typ, 8*f.n)
		case f.kind == kInt:
			fmt.Fprintf(b, "x.%s = d.Uint%d()\n", f.name, 8*f.n)
		case f.kind == kFixed:
			fmt.Fprintf(b, "d.Bytes(x.%s[:])\n", f.name)
		case f.kind == kBytes && !f.def:
			fmt.Fprintf(b, "x.%s = %s\n", f.name, region(f, strconv.Itoa(f.max)))
		case f.kind == kString:
			fmt.Fprintf(b, "x.%s = d.UTF16(%s)\n", f.name, region(f, strconv.Itoa(f.max)))
		case f.kind == kUint16s:
			fmt.Fprintf(b, "x.%s = d.Uint16s(l%s, %d)\n", f.name, f.count, f.max)
		case f.kind == kChain && f.link != "":
			fmt.Fprintf(b, `{
	region := %s
	for at := 0; at < len(region) && d.Err == nil; {
		var m []byte
		m, at = d.Link(region, at)
		var el %s
		sub := d.Sub(m)
		el.decodeSMB2(&sub, v)
		d.Join(&sub)
		x.%s = append(x.%s, el)
	}
}
`, region(f, strconv.Itoa(f.max)), f.typ, f.name, f.name)
		case f.kind == kChain:
			fmt.Fprintf(b, `if at := d.Chain(l%s, l%s, %d, %d); at >= 0 {
	x.%s = make([]%s, l%s)
	for i := range x.%s {
		sub, start := d.Member(at)
		x.%s[i].decodeSMB2(&sub, v)
		at = d.After(&sub, start)
	}
}
`, f.offset, f.count, f.max, least(g.byName[f.typ]), f.name, f.typ, f.count, f.name, f.name)
		default: // a union's arms, at its first
			// A union's bytes are bounded by the bytes holding them alone, not
			// by its default's max: an arm reads its own struct from them; a
			// known value with no arm keeps them within the default's max and
			// past it refuses the request, as the server acts on it; and any
			// other value's are kept within that max and past it none, so an
			// unknown context too long to keep is ignored, not the request
			// refused (MS-SMB2 3.3.5.4; RFC 22 section 16). None rather than
			// all, so a decoded value still encodes within its maxima.
			//
			// decision: an unknown value's bytes past the default's max are
			// dropped, not refused, which fails open: safe only while every
			// value a handler reads from the default's bytes is listed in
			// known=, and its name is kept, so a handler can still refuse it by
			// name. Overturned if a handler acts on a value's bytes without
			// adding it to known=.
			def := arms[len(arms)-1]
			expr, lit := discriminant(disc)
			fmt.Fprintf(b, "{\nr := %s\nswitch %s {\n", region(def, "len(d.B)"), expr)
			for _, a := range arms[:len(arms)-1] {
				take := "Join"
				if a.exact {
					take = "Exact"
				}
				fmt.Fprintf(b, "case %s:\nsub := d.Sub(r)\nx.%s.decodeSMB2(&sub, v)\nd.%s(&sub)\n", lit(a.cas), a.name, take)
			}
			if len(def.known) > 0 {
				lits := make([]string, len(def.known))
				for i, k := range def.known {
					lits[i] = lit(k)
				}
				fmt.Fprintf(b, "case %s:\nsub := d.Sub(r)\nx.%s = sub.Next(len(r), %d)\nd.Join(&sub)\n", strings.Join(lits, ", "), def.name, def.max)
			}
			fmt.Fprintf(b, "default:\nif len(r) <= %d {\nx.%s = r\n}\n}\n}\n", def.max, def.name)
		}
		b.WriteString(end)
	}
	b.WriteString("}\n")
}

// carried is s's odd StructureSize when its variable part's first byte, which
// that size counts, must be there; 0 when it need not.
func carried(s *strct) int {
	for _, f := range s.fields {
		if f.size%2 == 1 && !f.empty {
			return f.size
		}
	}
	return 0
}

// least is the fewest bytes a member of s takes: its fixed part on the wire
// in every dialect.
func least(s *strct) int {
	n := 0
	for _, f := range s.fields {
		if f.variable() {
			break
		}
		if f.since == "" {
			n += f.n
		}
	}
	return n
}

func (g *gen) encode(b *bytes.Buffer, s *strct) {
	fmt.Fprintf(b, "\nfunc (x *%s) encodeSMB2(e *wire.Encoder, v uint32) {\n", s.name)
	labels := used(s)
	if len(labels) > 0 {
		fmt.Fprintf(b, "var p%s int\n", strings.Join(labels, ", p"))
	}
	if carried(s) > 0 {
		b.WriteString("start := e.Pos()\n")
	}
	disc, arms := s.disc()
	for _, f := range s.fields {
		if (f.kind == kArm || f.def) && f != arms[0] {
			continue
		}
		end := gate(b, f, true)
		if !f.variable() {
			switch {
			case f.size > 0:
				fmt.Fprintf(b, "e.Uint16(%d)\n", f.size)
			case slices.Contains(labels, f.label):
				fmt.Fprintf(b, "p%s = e.Hole(%d)\n", f.label, f.n)
			case f.name == "_":
				fmt.Fprintf(b, "e.Zero(%d)\n", f.n)
			case f.kind == kInt && f.typ != "":
				fmt.Fprintf(b, "e.Uint%d(uint%d(x.%s))\n", 8*f.n, 8*f.n, f.name)
			case f.kind == kInt:
				fmt.Fprintf(b, "e.Uint%d(x.%s)\n", 8*f.n, f.name)
			case f.kind == kFixed:
				fmt.Fprintf(b, "e.Bytes(x.%s[:])\n", f.name)
			}
			b.WriteString(end)
			continue
		}
		loc := f
		if f.kind == kArm || f.def {
			loc = arms[len(arms)-1]
		}
		b.WriteString("{\n")
		switch {
		case loc.offset != "":
			fmt.Fprintf(b, "begin, at := e.Begin(%d)\n", loc.align)
		case loc.length != "":
			b.WriteString("at := e.Pos()\n")
		}
		switch {
		case f.kind == kBytes && !f.def:
			fmt.Fprintf(b, "e.Bytes(x.%s)\n", f.name)
		case f.kind == kString:
			fmt.Fprintf(b, "e.UTF16(x.%s)\n", f.name)
		case f.kind == kUint16s:
			fmt.Fprintf(b, "for _, u := range x.%s {\ne.Uint16(u)\n}\n", f.name)
		case f.kind == kChain && f.link != "":
			fmt.Fprintf(b, "link := -1\nfor i := range x.%s {\nlink = e.Link(link)\nsub := e.Sub()\nx.%s[i].encodeSMB2(&sub, v)\ne.Join(&sub)\n}\n", f.name, f.name)
		case f.kind == kChain:
			fmt.Fprintf(b, "for i := range x.%s {\ne.Begin(8)\nsub := e.Sub()\nx.%s[i].encodeSMB2(&sub, v)\ne.Join(&sub)\n}\n", f.name, f.name)
		default: // a union's arms, at its first
			expr, lit := discriminant(disc)
			fmt.Fprintf(b, "switch %s {\n", expr)
			for _, a := range arms[:len(arms)-1] {
				fmt.Fprintf(b, "case %s:\ne.Arm(%s)\nsub := e.Sub()\nx.%s.encodeSMB2(&sub, v)\ne.Join(&sub)\n", lit(a.cas), empties(arms, a), a.name)
			}
			fmt.Fprintf(b, "default:\ne.Arm(%s)\ne.Bytes(x.%s)\n}\n", empties(arms[:len(arms)-1], nil), loc.name)
		}
		if loc.length != "" {
			fmt.Fprintf(b, "e.Len(p%s, %d, e.Pos()-at, %d)\n", loc.length, width(s, loc.length), loc.max)
		}
		if loc.count != "" {
			fmt.Fprintf(b, "e.Len(p%s, %d, len(x.%s), %d)\n", loc.count, width(s, loc.count), f.name, loc.max)
		}
		if loc.offset != "" {
			fmt.Fprintf(b, "e.Off(p%s, %d, begin, at)\n", loc.offset, width(s, loc.offset))
		}
		b.WriteString("}\n" + end)
	}
	if odd := carried(s); odd > 0 {
		fmt.Fprintf(b, "e.Fill(start, %d)\n", odd)
	}
	b.WriteString("}\n")
}

// maxWire writes MaxWire: the most each field may take at v under its tags'
// maxima, with the padding before it.
func (g *gen) maxWire(b *bytes.Buffer, s *strct) {
	fixed := 0
	var rest bytes.Buffer
	_, arms := s.disc()
	for _, f := range s.fields {
		if (f.kind == kArm || f.def) && f != arms[0] {
			continue
		}
		var n string
		switch {
		case !f.variable() && f.since == "":
			fixed += f.n
			continue
		case !f.variable():
			n = strconv.Itoa(f.n)
		case f.kind == kUint16s:
			n = fmt.Sprintf("%d", 2*f.max)
		case f.kind == kChain && f.link == "":
			n = fmt.Sprintf("%d + %d*(7+(*%s)(nil).MaxWire(v))", f.align-1, f.max, f.typ)
		case f.kind == kArm || f.def:
			def := arms[len(arms)-1]
			n = fmt.Sprintf("%d + max(%d", def.align-1, def.max)
			for _, a := range arms[:len(arms)-1] {
				n += fmt.Sprintf(", (*%s)(nil).MaxWire(v)", a.typ)
			}
			n += ")"
		default:
			n = fmt.Sprintf("%d", f.align-1+f.max)
		}
		end := gate(&rest, f, false)
		fmt.Fprintf(&rest, "n += %s\n%s", n, end)
	}
	fmt.Fprintf(b, "\nfunc (*%s) MaxWire(v uint32) int {\nn := %d\n%sreturn n\n}\n", s.name, fixed, rest.String())
}

// isZero writes IsZero: every named field at its zero value.
func isZero(b *bytes.Buffer, s *strct) {
	var terms []string
	for _, f := range s.fields {
		switch {
		case f.name == "_":
		case f.kind == kInt:
			terms = append(terms, "x."+f.name+" == 0")
		case f.kind == kFixed:
			terms = append(terms, fmt.Sprintf("x.%s == [%d]byte{}", f.name, f.n))
		case f.kind == kString:
			terms = append(terms, "x."+f.name+` == ""`)
		case f.kind == kArm:
			terms = append(terms, "x."+f.name+".IsZero()")
		default:
			terms = append(terms, "len(x."+f.name+") == 0")
		}
	}
	if len(terms) == 0 {
		terms = []string{"true"}
	}
	fmt.Fprintf(b, "\nfunc (x *%s) IsZero() bool {\nreturn %s\n}\n", s.name, strings.Join(terms, " &&\n"))
}
