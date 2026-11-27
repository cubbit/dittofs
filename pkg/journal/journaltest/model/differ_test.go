package model

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal"
)

// byteObservation is an observation one map entry per byte, as differByByte
// reads it.
type byteObservation struct {
	held    map[int64]heldByte
	missing map[int64]bool
	extents map[int64]bool
	bits    map[int64]indexByte
}

// expand returns o one map entry per byte.
func expand(o *observation) byteObservation {
	b := byteObservation{map[int64]heldByte{}, map[int64]bool{}, map[int64]bool{}, map[int64]indexByte{}}
	for _, h := range o.held {
		for off := h.ext.Off; off < end(h.ext); off++ {
			b.held[off] = heldByte{h.data[off-h.ext.Off], h.ver}
		}
	}
	for _, e := range o.missing {
		for off := e.Off; off < end(e); off++ {
			b.missing[off] = true
		}
	}
	for _, e := range o.extents {
		for off := e.Off; off < end(e); off++ {
			b.extents[off] = true
		}
	}
	for _, r := range o.bits {
		for off := r.ext.Off; off < end(r.ext); off++ {
			b.bits[off] = r.b
		}
	}
	return b
}

// differByByte is differ's comparison of the bytes, Extents and the index
// byte by byte, each byte judged on its own. differ judges pieces instead,
// and must say exactly what this says.
func differByByte(o byteObservation, st *fileState) string {
	held, bits := st.cells(), st.bits()
	if d := firstRunIn(held, func(off int64) string {
		c := held[off]
		switch got, ok := o.held[off]; {
		case !ok && c.rec.durable && c.rec.kind == kindWrite:
			return fmt.Sprintf("held by %s, but the journal reports them missing: a synced write was lost", heldBy(c))
		case !ok:
			return fmt.Sprintf("held by %s, but the journal reports them missing", heldBy(c))
		case got.ver != c.rec.ver:
			return fmt.Sprintf("held by %s, but the journal serves them at %s", heldBy(c), fmtVersion(got.ver))
		case got.b != c.b:
			return fmt.Sprintf("held by %s, but the journal serves other bytes there", heldBy(c))
		case !o.extents[off]:
			return "held, but Extents omits them"
		}
		return ""
	}); d != "" {
		return d
	}
	if d := firstRunIn(o.held, func(off int64) string {
		if _, ok := held[off]; !ok {
			return fmt.Sprintf("served at %s, but the model holds nothing there: removed, released or never written", fmtVersion(o.held[off].ver))
		}
		return ""
	}); d != "" {
		return d
	}
	if d := firstRunIn(o.extents, func(off int64) string {
		if _, ok := held[off]; !ok {
			return "named by Extents, but the model holds nothing there"
		}
		return ""
	}); d != "" {
		return d
	}
	if d := firstRunIn(held, func(off int64) string {
		got, ok := o.bits[off]
		want := held[off]
		switch {
		case !ok:
			return "held, but the index has no held entry for them"
		case got.ver != want.rec.ver:
			return fmt.Sprintf("held at %s, but the index names %s", fmtVersion(want.rec.ver), fmtVersion(got.ver))
		case got.offloaded && !bits[off]:
			return fmt.Sprintf("dirty at %s, but the index marks them offloaded: a release would evict the only copy", fmtVersion(want.rec.ver))
		case !got.offloaded && bits[off]:
			return fmt.Sprintf("offloaded at %s, but the index marks them dirty", fmtVersion(want.rec.ver))
		}
		return ""
	}); d != "" {
		return d
	}
	return firstRunIn(o.bits, func(off int64) string {
		if _, ok := held[off]; !ok {
			return "named held by the index, but the model holds nothing there"
		}
		return ""
	})
}

// randomFile returns a file of random writes, fills, releases and marks.
func randomFile(rng *rand.Rand) *file {
	f := &file{}
	kinds := []recordKind{kindWrite, kindWrite, kindFill, kindRelease, kindMark}
	for n := range 1 + rng.IntN(6) {
		r := &record{kind: kinds[rng.IntN(len(kinds))], ver: ver(uint64(1 + rng.IntN(4))), seq: uint64(n), durable: rng.IntN(2) == 0}
		off, size := rng.Int64N(40), 1+rng.Int64N(20)
		r.ext = ext(off, size)
		if r.kind == kindWrite || r.kind == kindFill {
			r.data = make([]byte, size)
			for j := range r.data {
				r.data[j] = byte(rng.IntN(2))
			}
		}
		f.recs = append(f.recs, r)
	}
	return f
}

// answer returns an observation of st as a journal might answer: right, or
// wrong here and there in each answer, its runs cut at random.
func answer(rng *rand.Rand, st *fileState) *observation {
	o := &observation{hasIndex: true, asOf: st.change} // the change version and the index markers right: differByByte covers neither
	wrong := func() bool { return rng.IntN(6) == 0 }
	at := int64(0)
	for _, s := range st.spans {
		if wrong() {
			continue // reported missing
		}
		data := make([]byte, s.ext.Len)
		copy(data, s.rec.data[s.ext.Off-s.rec.ext.Off:])
		if wrong() {
			data[rng.IntN(len(data))] ^= 1
		}
		v := s.rec.ver
		if wrong() {
			v = ver(v.Counter + 1)
		}
		e := s.ext
		if wrong() && e.Off > at {
			e.Off-- // serves a byte the model does not hold
			data = append([]byte{0}, data...)
			e.Len++
		}
		o.held = append(o.held, heldSpan{e, v, data})
		at = end(e)
		if !wrong() {
			o.extents = append(o.extents, s.ext)
		}
		ib := indexByte{s.rec.ver, s.rec.kind == kindFill}
		if wrong() {
			ib.offloaded = !ib.offloaded
		}
		if !wrong() {
			o.bits = append(o.bits, indexRun{e, ib})
		}
	}
	if wrong() {
		o.extents = append(o.extents, ext(60+rng.Int64N(5), 1+rng.Int64N(4)))
	}
	o.extents = merge(o.extents)
	return o
}

// differ says what the byte-by-byte walk says, on random files and random
// answers to them.
func TestDifferJudgesPiecesAsBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	differed := 0
	const draws = 4000
	for i := range draws {
		st := derive(randomFile(rng), all)
		o := answer(rng, st)
		want := differByByte(expand(o), st)
		got := differ(o, st, ver(1<<20))
		if got != want {
			t.Fatalf("draw %d: differ = %q, want the byte walk's %q", i, got, want)
		}
		if want != "" {
			differed++
		}
	}
	if differed == 0 || differed == draws {
		t.Fatalf("%d of %d draws differed, want some and not all, so both answers are compared", differed, draws)
	}
}

// addReadByByte is the check of one read's answer one map entry per byte.
// addRead keeps runs instead, and must refuse what this refuses, naming the
// same byte, and keep what this keeps.
func addReadByByte(b byteObservation, w extent, p []byte, missing []extent, held []journal.Held) error {
	for _, h := range held {
		piece, ok := intersect(h.Extent, w)
		if !ok || piece != h.Extent {
			return fmt.Errorf("ReadAt %s reports %s held, outside the read", fmtExtent(w), fmtExtent(h.Extent))
		}
		for off := piece.Off; off < end(piece); off++ {
			if _, dup := b.held[off]; dup {
				return fmt.Errorf("ReadAt %s reports byte %d held twice", fmtExtent(w), off)
			}
			b.held[off] = heldByte{p[off-w.Off], h.Version}
		}
	}
	for _, e := range missing {
		piece, ok := intersect(e, w)
		if !ok || piece != e {
			return fmt.Errorf("ReadAt %s reports %s missing, outside the read", fmtExtent(w), fmtExtent(e))
		}
		for off := piece.Off; off < end(piece); off++ {
			if _, dup := b.held[off]; dup || b.missing[off] {
				return fmt.Errorf("ReadAt %s reports byte %d both held and missing, or missing twice", fmtExtent(w), off)
			}
			b.missing[off] = true
		}
	}
	for off := w.Off; off < end(w); off++ {
		if _, ok := b.held[off]; !ok && !b.missing[off] {
			return fmt.Errorf("ReadAt %s reports byte %d neither held nor missing: missing must be exact", fmtExtent(w), off)
		}
	}
	return nil
}

// addRead refuses and keeps what the byte-by-byte check does, on random
// reads of two windows whose answers overlap, run outside the read and leave
// gaps now and then.
func TestAddReadChecksRunsAsBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	refused := 0
	const draws = 4000
	for i := range draws {
		o := &observation{}
		b := byteObservation{map[int64]heldByte{}, map[int64]bool{}, map[int64]bool{}, map[int64]indexByte{}}
		var gotErr, wantErr error
		for _, w := range []extent{ext(0, 20), ext(25, 20)} {
			p := make([]byte, w.Len)
			for j := range p {
				p[j] = byte(rng.IntN(4))
			}
			var held []journal.Held
			var missing []extent
			for at := w.Off; at < end(w); {
				n := 1 + rng.Int64N(6)
				e := ext(at, min(n, end(w)-at))
				switch rng.IntN(12) {
				case 0: // a gap
				case 1: // a run past the window
					held = append(held, journal.Held{Extent: ext(e.Off, e.Len+3), Version: ver(1)})
				case 2: // a run named twice
					held = append(held, journal.Held{Extent: e, Version: ver(1)}, journal.Held{Extent: e, Version: ver(2)})
				case 3: // missing over held
					held = append(held, journal.Held{Extent: e, Version: ver(1)})
					missing = append(missing, e)
				default:
					if rng.IntN(2) == 0 {
						held = append(held, journal.Held{Extent: e, Version: ver(uint64(1 + rng.IntN(3)))})
					} else {
						missing = append(missing, e)
					}
				}
				at = end(e)
			}
			rng.Shuffle(len(held), func(a, c int) { held[a], held[c] = held[c], held[a] })
			gotErr, wantErr = o.addRead(w, p, missing, held), addReadByByte(b, w, p, missing, held)
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Fatalf("draw %d: addRead of %s = %v, want the byte check's %v", i, fmtExtent(w), gotErr, wantErr)
			}
			if wantErr != nil {
				refused++
				break
			}
		}
		if wantErr != nil {
			continue
		}
		got := expand(o)
		if fmt.Sprint(got.held) != fmt.Sprint(b.held) || fmt.Sprint(got.missing) != fmt.Sprint(b.missing) {
			t.Fatalf("draw %d: addRead keeps held %v missing %v, want %v and %v", i, got.held, got.missing, b.held, b.missing)
		}
	}
	if refused == 0 || refused == draws {
		t.Fatalf("%d of %d draws were refused, want some and not all", refused, draws)
	}
}
