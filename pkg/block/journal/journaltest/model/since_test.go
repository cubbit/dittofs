package model

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// sinceDiffersByByte is the comparison of what Since yielded with st as a
// byte-by-byte walk: one map entry per byte named, each byte judged on its
// own. sinceDiffers judges pieces instead, and must say exactly what this
// says.
func sinceDiffersByByte(sv sinceView, st *fileState) string {
	from := fmtVersion(sv.from)
	writes := map[int64]sinceByte{}
	for _, w := range sv.writes {
		for off := w.ext.Off; off < end(w.ext); off++ {
			writes[off] = w.b
		}
	}
	want := func(off int64) (sinceByte, bool) {
		c := st.held[off]
		if c.rec.ver.Compare(sv.from) <= 0 {
			return sinceByte{}, false
		}
		w := sinceByte{ver: c.rec.ver}
		if c.rec.kind == kindWrite {
			w.mtime = c.rec.mtime
		}
		return w, true
	}
	if d := firstRunIn(st.held, func(off int64) string {
		w, ok := want(off)
		got, named := writes[off]
		switch {
		case ok && !named:
			return fmt.Sprintf("held at %s, but Since from %s omits them", fmtVersion(w.ver), from)
		case !ok && named:
			return fmt.Sprintf("held at or below %s, but Since from %s names them at %s", from, from, fmtVersion(got.ver))
		case ok && got != w:
			return fmt.Sprintf("written at %s with mtime %d, but Since from %s names them at %s with mtime %d", fmtVersion(w.ver), w.mtime, from, fmtVersion(got.ver), got.mtime)
		}
		return ""
	}); d != "" {
		return d
	}
	return firstRunIn(writes, func(off int64) string {
		if _, ok := st.held[off]; !ok {
			return fmt.Sprintf("named held by Since from %s, but the model holds nothing there", from)
		}
		return ""
	})
}

// sinceDiffers says what the byte-by-byte walk says, on random files and
// random answers: records that overlap, outrank and abut one another, and
// Since writes that agree, disagree, run past what is held or name nothing
// held. Most draws differ somewhere, so the run each names is checked too.
func TestSinceDiffersJudgesPiecesAsBytes(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	differed := 0
	for i := range 4000 {
		f := &file{}
		for n := range 1 + rng.IntN(6) {
			kind := kindWrite
			if rng.IntN(4) == 0 {
				kind = kindFill
			}
			off, size := rng.Int64N(48), 1+rng.Int64N(24)
			r := &record{kind: kind, ext: ext(off, size), ver: ver(uint64(1 + rng.IntN(5))), seq: uint64(n), data: make([]byte, size), mtime: int64(rng.IntN(2))}
			f.recs = append(f.recs, r)
		}
		st := derive(f, all)

		sv := sinceView{from: ver(uint64(rng.IntN(5)))}
		right := rng.IntN(3) == 0
		if right {
			// The right answer, which says nothing, or nearly: one run's
			// mtime turned.
			for _, h := range st.heldRuns() {
				if h.rec.ver.Compare(sv.from) > 0 {
					b := sinceByte{ver: h.rec.ver}
					if h.rec.kind == kindWrite {
						b.mtime = h.rec.mtime
					}
					sv.writes = append(sv.writes, sinceRun{h.ext, b})
				}
			}
			if len(sv.writes) > 0 && rng.IntN(2) == 0 {
				sv.writes[rng.IntN(len(sv.writes))].b.mtime ^= 1
			}
		}
		for off := int64(0); !right && off < 80; {
			off += rng.Int64N(6)
			size := 1 + rng.Int64N(16)
			sv.writes = append(sv.writes, sinceRun{ext(off, size), sinceByte{ver(uint64(1 + rng.IntN(5))), int64(rng.IntN(2))}})
			off += size
		}

		want := sinceDiffersByByte(sv, st)
		if got := sinceDiffers(sv, st.heldRuns(), st); got != want {
			t.Fatalf("draw %d: sinceDiffers = %q, want the byte walk's %q", i, got, want)
		}
		if want != "" {
			differed++
		}
	}
	if differed == 0 || differed == 4000 {
		t.Fatalf("%d of 4000 draws differed, want some and not all, so both answers are compared", differed)
	}
}
