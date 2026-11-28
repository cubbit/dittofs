package assembler_test

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/carver"
	"github.com/cubbit/dittofs/pkg/carver/assembler"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
)

// t0 is when every chunk a builder makes was written, unless a test sets
// Oldest itself.
var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// lengths is n chunk lengths of size bytes each.
func lengths(n int, size int32) []int32 {
	l := make([]int32, n)
	for i := range l {
		l[i] = size
	}
	return l
}

// stream builds one pass's chunks: files one after another, file i numbered
// i+1, each file's chunks in offset order with gap bytes between two. Every
// chunk's hash is distinct, its Keys 1 and its Oldest t0.
func stream(gap int64, files ...[]int32) []carver.ChunkInfo {
	var s []carver.ChunkInfo
	for i, f := range files {
		var id entity.FileID
		binary.BigEndian.PutUint64(id[:], uint64(i+1))
		var off int64
		for _, n := range f {
			var h entity.ChunkHash
			binary.BigEndian.PutUint64(h[:], uint64(len(s)+1))
			s = append(s, carver.ChunkInfo{
				File:   id,
				Chunk:  carver.Chunk{Offset: off, Length: n, Hash: h},
				Keys:   1,
				Oldest: t0,
			})
			off += int64(n) + gap
		}
	}
	return s
}

// folded is what one pass's fold returned.
type folded struct {
	plans []carver.BlockPlan   // every plan, in the order Add and then Finish returned them
	disp  []carver.Disposition // each input chunk's disposition
	held  bool                 // Finish's held, for the last plan
}

// fold runs one pass as the Assemble state does: New, Add over the stream and
// Finish at now. It fails when Add closes more than one plan, or gives a chunk
// that is not all zeros a disposition other than Carried.
func fold(t *testing.T, cfg carver.AssemblerConfig, s []carver.ChunkInfo, now time.Time) folded {
	t.Helper()
	f, err := assembler.New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	var r folded
	for i, c := range s {
		closed, d := f.Add(c)
		if len(closed) > 1 {
			t.Fatalf("Add(chunk %d): got %d plans, want at most 1", i, len(closed))
		}
		if !c.Zero && d != carver.Carried {
			t.Fatalf("Add(chunk %d): got disposition %d, want Carried", i, d)
		}
		r.plans = append(r.plans, closed...)
		r.disp = append(r.disp, d)
	}
	var last *carver.BlockPlan
	last, r.held = f.Finish(now)
	if last != nil {
		r.plans = append(r.plans, *last)
	} else if r.held {
		t.Fatalf("Finish: held with no plan")
	}
	return r
}

// carriedBytes sums the lengths of chunks.
func carriedBytes(chunks []carver.ChunkInfo) int64 {
	var n int64
	for _, c := range chunks {
		n += int64(c.Length)
	}
	return n
}

// full reports whether a plan reached the target in carried bytes or holds
// the count cap's chunks.
func full(cfg carver.AssemblerConfig, p carver.BlockPlan) bool {
	return carriedBytes(p.Carried) >= cfg.BlockTarget || len(p.Carried) == cfg.MaxChunks
}

// checkPlans asserts what every fold of s must give, a pure check of its
// plans: their Refs tile s in order; A1, each plan's carried chunks whole refs
// of its own, at most MaxChunks of them, under the target without the last;
// A3, every plan but the last full, a held last plan short and younger than
// MaxAge at now, a short last plan that is not held at least MaxAge old; A5,
// each file's carried chunks in a plan contiguous and in offset order.
func checkPlans(t *testing.T, cfg carver.AssemblerConfig, s []carver.ChunkInfo, plans []carver.BlockPlan, now time.Time, held bool) {
	t.Helper()

	var refs []carver.ChunkInfo
	for _, p := range plans {
		refs = append(refs, p.Refs...)
	}
	if len(refs) != len(s) {
		t.Fatalf("plans' Refs: got %d refs, want the %d input chunks", len(refs), len(s))
	}
	for i := range s {
		if refs[i] != s[i] {
			t.Fatalf("plans' Refs[%d]: got %+v, want input chunk %+v", i, refs[i], s[i])
		}
	}

	for i, p := range plans {
		if len(p.Carried) > cfg.MaxChunks {
			t.Errorf("plan %d: got %d carried chunks, want at most MaxChunks %d", i, len(p.Carried), cfg.MaxChunks)
		}
		for _, c := range p.Carried {
			if !isRef(p, c) {
				t.Errorf("plan %d: carried chunk %+v is not one of its refs", i, c)
			}
		}
		if n := len(p.Carried); n > 0 {
			if under := carriedBytes(p.Carried[:n-1]); under >= cfg.BlockTarget {
				t.Errorf("plan %d: carries %d bytes before its last chunk, want under the target %d", i, under, cfg.BlockTarget)
			}
		}
		if i < len(plans)-1 && !full(cfg, p) {
			t.Errorf("plan %d of %d: short, %d bytes in %d chunks, want the target %d or MaxChunks %d",
				i, len(plans), carriedBytes(p.Carried), len(p.Carried), cfg.BlockTarget, cfg.MaxChunks)
		}
		checkContiguous(t, i, p.Carried)
	}

	if len(plans) == 0 {
		return
	}
	last := plans[len(plans)-1]
	age := now.Sub(oldestRef(last))
	switch {
	case held && full(cfg, last):
		t.Errorf("last plan: held though full, %d bytes in %d chunks", carriedBytes(last.Carried), len(last.Carried))
	case held && age >= cfg.MaxAge:
		t.Errorf("last plan: held at age %v, want put at MaxAge %v", age, cfg.MaxAge)
	case !held && !full(cfg, last) && age < cfg.MaxAge:
		t.Errorf("last plan: short and put at age %v, want held under MaxAge %v", age, cfg.MaxAge)
	}
}

// isRef reports whether c is one of p's refs, whole and unchanged.
func isRef(p carver.BlockPlan, c carver.ChunkInfo) bool {
	for _, r := range p.Refs {
		if r == c {
			return true
		}
	}
	return false
}

// checkContiguous asserts that each file's chunks in carried sit together, in
// offset order.
func checkContiguous(t *testing.T, plan int, carried []carver.ChunkInfo) {
	t.Helper()
	left := map[entity.FileID]bool{}
	for j, c := range carried {
		if j == 0 {
			continue
		}
		prev := carried[j-1]
		switch {
		case c.File != prev.File:
			left[prev.File] = true
			if left[c.File] {
				t.Errorf("plan %d: file %x returns at carried chunk %d after another file", plan, c.File, j)
			}
		case c.Offset <= prev.Offset:
			t.Errorf("plan %d: file %x carried chunk %d at offset %d after offset %d", plan, c.File, j, c.Offset, prev.Offset)
		}
	}
}

// oldestRef is the earliest Oldest among p's refs.
func oldestRef(p carver.BlockPlan) time.Time {
	o := p.Refs[0].Oldest
	for _, r := range p.Refs {
		if r.Oldest.Before(o) {
			o = r.Oldest
		}
	}
	return o
}
