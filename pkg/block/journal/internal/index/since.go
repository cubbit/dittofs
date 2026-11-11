package index

import (
	"cmp"
	"math"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// Change is one item of the index's side of Since: a held extent, with the
// record it is read from, or an unsettled removal marker, with its kind and,
// for a clone target, its spec. A held extent is a write whether it was
// written or filled. The record lets the caller read a write's mtime from its
// header, which the index does not keep.
type Change struct {
	Kind    core.ChangeKind // ChangeWrite for a held extent, else the removal or synced loss that left the marker
	Extent  core.Extent     // a held extent, or the extent a marker covers
	Version core.Version
	Record  RecordAt        // a held extent's record; zero for a marker
	Clone   *core.CloneSpec // a clone target's spec, a copy the caller may keep; nil otherwise
	// Over is a write's runs where it superseded another write of the file
	// whose version is above the Since's from, in offset order, disjoint and
	// not adjacent, each inside the write's extent; nil for anything else.
	Over []core.Extent
}

// since appends to out every held extent and every unsettled removal marker
// of id whose version is strictly above from, in version order, and returns
// it. A file with no entry yields nothing.
//
// Of one version, markers come before held extents, so a caller applying the
// changes in order never removes content held at a marker's own version,
// which the marker does not exclude; then extents come in offset order. A
// held extent is one record's contiguous piece: pieces of a record split only
// by their offloaded bit are one extent, since nothing yielded may depend on
// the bit.
//
// It walks the file's pieces under its read lock, as a read of the whole
// file does, copying only the items it yields, and sorts after releasing the
// lock, in O(n log n) for the n items yielded. The walk is O(N) for the N
// pieces held, since nothing orders them by version. The result is a copy,
// so working through it holds no lock. A synced loss's marker is yielded as
// ChangeLoss; a loss record itself is the journal's side, not the index's.
func (fs *files) since(id core.FileID, from core.Version, out []Change) []Change {
	first := len(out)
	fs.view(id, func(f *file) {
		f.held.visit(core.Extent{Off: 0, Len: math.MaxInt64}, func(p Piece) bool {
			if p.Version.Compare(from) <= 0 {
				return true
			}
			// A piece next to the last one, of the same record, extends it:
			// pieces of one record split only by their offloaded bit are one
			// extent. The version test, as in joins, only guards against a
			// caller that names one record with two. Skip is not compared,
			// because Change carries none: give Change a skip and the merge
			// must also require the piece's skip to continue the last one's.
			if n := len(out); n > first {
				if last := &out[n-1]; extentEnd(last.Extent) == p.Extent.Off && last.Record == p.Record &&
					last.Version == p.Version {
					last.Extent.Len += p.Extent.Len
					return true
				}
			}
			out = append(out, Change{Kind: core.ChangeWrite, Extent: p.Extent, Version: p.Version, Record: p.Record})
			return true
		})
		if len(f.held.over) > 0 {
			rs := slices.DeleteFunc(slices.Clone(f.held.over), func(r overRun) bool { return r.was.Compare(from) <= 0 })
			for i := first; i < len(out); i++ {
				out[i].Over = overOf(rs, out[i].Extent, out[i].Version)
			}
		}
		for _, m := range f.markers[f.firstAbove(from):] {
			c := Change{Kind: m.kind, Extent: m.extent, Version: m.version}
			if m.clone != nil {
				spec := *m.clone
				c.Clone = &spec
			}
			out = append(out, c)
		}
	})
	slices.SortFunc(out[first:], func(a, b Change) int {
		if c := a.Version.Compare(b.Version); c != 0 {
			return c
		}
		if aw, bw := a.Kind == core.ChangeWrite, b.Kind == core.ChangeWrite; aw != bw {
			if bw {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Extent.Off, b.Extent.Off)
	})
	return out
}

// overOf returns the runs of rs inside e that the write of version v
// superseded, sorted by offset and merged where they touch. rs holds only runs
// above the caller's from.
//
// decision: only a run whose superseder is v itself counts, with no walk down
// a chain of superseders. A write over a write that had itself superseded
// another covers the bytes that write held, and so its own runs already lie
// over wherever the older run does; the walk could add no extent. A run whose
// superseder left the index, released or removed, waits for a settle at its
// version and is reported by nothing, since no write holds those bytes any
// more. Revisit if a superseder can leave the index while content it
// superseded stays attributable to a later write.
//
// ponytail: each call scans rs whole, so Since over a file costs O(w*r) for w
// yielded writes and r runs kept. Upgrade to runs sorted by superseding
// version, found by binary search, when a profile of Since over a hot file
// shows it.
func overOf(rs []overRun, e core.Extent, v core.Version) []core.Extent {
	var out []core.Extent
	for _, r := range rs {
		lo, hi := max(r.extent.Off, e.Off), min(extentEnd(r.extent), extentEnd(e))
		if r.by == v && lo < hi {
			out = append(out, core.Extent{Off: lo, Len: hi - lo})
		}
	}
	slices.SortFunc(out, func(a, b core.Extent) int { return cmp.Compare(a.Off, b.Off) })
	merged := out[:0]
	for _, c := range out {
		if n := len(merged); n > 0 && c.Off <= extentEnd(merged[n-1]) {
			merged[n-1].Len = max(extentEnd(merged[n-1]), extentEnd(c)) - merged[n-1].Off
			continue
		}
		merged = append(merged, c)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}
