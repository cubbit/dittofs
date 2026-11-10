package index

import "github.com/cubbit/dittofs/pkg/block/journal/internal/core"

// RepointRecord moves every held and retained piece of id that sits at record
// from to record to, keeping each piece's extent, skip, version, sequence
// number, offloaded bit and superseder: the record is the same one, read from
// another place, and a copy carries its original's sequence number. It
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

// Names reports whether a held or a retained piece of id sits at record r. It
// takes the file's lock shared and creates no entry.
func (ix *Index) Names(id core.FileID, r RecordAt) bool {
	var at bool
	return ix.fs.view(id, func(f *file) { at = f.sitsAt(r) }) && at
}

// HeldAt returns a copy of id's held pieces at record r, each with its
// offloaded bit, in offset order. It takes the file's lock shared and creates
// no entry.
func (ix *Index) HeldAt(id core.FileID, r RecordAt) []Piece {
	var out []Piece
	ix.fs.view(id, func(f *file) {
		f.held.visit(whole, func(p Piece) bool {
			if p.Record == r {
				out = append(out, p)
			}
			return true
		})
	})
	return out
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
		seq  core.Seq
	}
	var held, kept []at
	f.held.visit(whole, func(p Piece) bool {
		if p.Record == from {
			held = append(held, at{p.Extent, p.Skip, p.Seq})
		}
		return true
	})
	for _, r := range f.retained.list(nil) {
		if r.Record == from {
			kept = append(kept, at{r.Extent, r.Skip, r.Seq})
		}
	}
	n := 0
	for _, a := range held {
		n += f.held.repoint(a.e, from, to, a.skip, a.seq)
	}
	for _, a := range kept {
		n += f.retained.repoint(a.e, from, to, a.skip)
	}
	return n
}

// Owner returns the tag that owns id, and false when none does or the file has
// no entry. It takes the file's lock shared and creates no entry.
func (ix *Index) Owner(id core.FileID) (tag core.ShareTag, ok bool) {
	ix.fs.view(id, func(f *file) { tag, ok = f.owner() })
	return tag, ok
}
