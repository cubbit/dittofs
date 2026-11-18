package model

import (
	"cmp"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
)

// deriveByByte is derive painted one map entry per byte: every write and
// fill paints its bytes, every release and removal deletes the bytes it
// covers, and a byte's offloaded bit is set by a fill or a mark of its
// version. derive paints runs instead, and must decide every byte the same.
func deriveByByte(f *file) (map[int64]cell, map[int64]bool) {
	recs := slices.Clone(f.recs)
	slices.SortFunc(recs, func(a, b *record) int {
		if c := a.ver.Compare(b.ver); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})
	held, bit := map[int64]cell{}, map[int64]bool{}
	var marks []*record
	for _, r := range recs {
		switch {
		case r.kind == kindWrite || r.kind == kindFill:
			for i, b := range r.data {
				held[r.ext.Off+int64(i)] = cell{b, r}
			}
		case r.kind == kindRelease || r.kind.removal():
			for off := range held {
				if covers(r.ext, off) {
					delete(held, off)
				}
			}
		case r.kind == kindMark:
			marks = append(marks, r)
		}
	}
	for off, c := range held {
		if c.rec.kind == kindFill {
			bit[off] = true
			continue
		}
		for _, mk := range marks {
			if mk.ver == c.rec.ver && covers(mk.ext, off) {
				bit[off] = true
				break
			}
		}
	}
	return held, bit
}

// derive decides every byte as the byte-by-byte painting does, on random
// files of writes, fills, releases, removals to the end of the file and
// marks, overlapping and outranking one another in any order.
func TestDeriveDecidesRunsAsBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	kinds := []recordKind{kindWrite, kindWrite, kindFill, kindRelease, kindMark, kindTruncate, kindDeallocate}
	for i := range 3000 {
		f := &file{}
		for n := range 1 + rng.IntN(8) {
			r := &record{kind: kinds[rng.IntN(len(kinds))], ver: ver(uint64(1 + rng.IntN(4))), seq: uint64(n)}
			off, size := rng.Int64N(40), 1+rng.Int64N(20)
			r.ext = ext(off, size)
			switch {
			case r.kind == kindWrite || r.kind == kindFill:
				r.data = make([]byte, size)
				for j := range r.data {
					r.data[j] = byte(rng.IntN(3))
				}
			case r.kind == kindTruncate:
				r.ext = ext(off, toEnd(off))
			}
			f.recs = append(f.recs, r)
		}

		wantHeld, wantBit := deriveByByte(f)
		st := derive(f, all)
		if got := st.cells(); !maps.Equal(got, wantHeld) {
			t.Fatalf("draw %d: derive holds %v, want the byte painting's %v", i, got, wantHeld)
		}
		if got := st.bits(); !maps.Equal(got, wantBit) {
			t.Fatalf("draw %d: derive sets bits %v, want the byte painting's %v", i, got, wantBit)
		}
		if st.holds() != (len(wantHeld) > 0) {
			t.Fatalf("draw %d: holds() = %t with %d bytes held", i, st.holds(), len(wantHeld))
		}
		if st.dirty() != (len(wantBit) < len(wantHeld)) {
			t.Fatalf("draw %d: dirty() = %t with %d bytes held and %d offloaded", i, st.dirty(), len(wantHeld), len(wantBit))
		}
		for j, s := range st.spans {
			if j > 0 && (s.ext.Off < end(st.spans[j-1].ext) || s.ext.Off == end(st.spans[j-1].ext) && s.rec == st.spans[j-1].rec) {
				t.Fatalf("draw %d: runs %v are not sorted, disjoint and maximal", i, st.spans)
			}
		}
	}
}
