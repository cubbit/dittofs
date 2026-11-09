package index

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// RepointRecord moves every held and retained piece of id that sits at record
// from to record to, keeping each piece's extent, skip, version, offloaded bit
// and superseder: the record is the same one, read from another place. It
// returns the number of pieces moved. It takes the file's lock exclusively. A
// file with no entry, or none of whose pieces sit at from, changes nothing and
// creates no entry.
func (ix *Index) RepointRecord(id core.FileID, from, to RecordAt) (moved int, hd HeldDelta, err error) {
	var any bool
	if !ix.fs.view(id, func(f *file) { any = f.sitsAt(from) }) || !any {
		return 0, HeldDelta{}, nil
	}
	hd, err = ix.fs.update(id, func(f *file) error {
		moved = f.repointRecord(from, to)
		return nil
	})
	return moved, hd, err
}

// sitsAt reports whether any held or retained piece of f is at record r.
func (f *file) sitsAt(r RecordAt) bool {
	found := false
	f.held.visit(whole, func(p Piece) bool {
		found = p.Record == r
		return !found
	})
	if found {
		return true
	}
	for _, p := range f.retained.list(nil) {
		if p.Record == r {
			return true
		}
	}
	return false
}

// repointRecord moves f's pieces at from to to, one piece at a time with its
// own extent and skip: the set-level repoint rewrites the skip of what it moves
// from the window it is given, so a whole-file window would not keep it.
func (f *file) repointRecord(from, to RecordAt) int {
	type at struct {
		e    core.Extent
		skip uint32
	}
	var held, kept []at
	f.held.visit(whole, func(p Piece) bool {
		if p.Record == from {
			held = append(held, at{p.Extent, p.Skip})
		}
		return true
	})
	for _, r := range f.retained.list(nil) {
		if r.Record == from {
			kept = append(kept, at{r.Extent, r.Skip})
		}
	}
	n := 0
	for _, a := range held {
		n += f.held.repoint(a.e, from, to, a.skip)
	}
	for _, a := range kept {
		n += f.retained.repoint(a.e, from, to, a.skip)
	}
	return n
}
