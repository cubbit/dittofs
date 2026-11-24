package index

import "github.com/cubbit/dittofs/pkg/journal/internal/core"

// RepointMove is one piece move of a batched repoint. Window is the file
// extent whose pieces move, each kept to the part of it inside the window;
// From and To are the records the move reads from and lands at; Skip is the
// offset in To's payload the window's first byte reads from. Seq is the
// sequence number the moved pieces answer at To — the number of the record
// they land in, which a read's header check demands they carry; zero keeps
// each piece's own number, the move of a re-appended record, whose copy
// keeps its original's.
type RepointMove struct {
	Window core.Extent
	From   RecordAt
	To     RecordAt
	Skip   uint32
	Seq    core.Seq
}

// RepointFile moves, as one index step for every reader of the file (RFC 1
// §8.4), the pieces its moves name: held and retained alike, each keeping its
// extent, version, sequence number, offloaded bit and superseder — a moved
// piece reads from To's payload at Skip plus its offset past the window's
// start. It takes the file's lock exclusively, once for every move, so a
// reader sees each piece at its old record or its new one, never neither. A
// file with no entry, or none of whose moves names a record a piece sits at,
// changes nothing and creates no entry. A nil or empty move list changes
// nothing. It returns the number of pieces moved.
func (ix *Index) RepointFile(id core.FileID, moves []RepointMove) (moved int, hd HeldDelta, err error) {
	if len(moves) == 0 {
		return 0, HeldDelta{}, nil
	}
	var any bool
	if !ix.fs.view(id, func(f *file) {
		for _, m := range moves {
			if f.sitsAt(m.From) {
				any = true
				return
			}
		}
	}) || !any {
		return 0, HeldDelta{}, nil
	}
	hd, err = ix.fs.update(id, func(f *file) error {
		for _, m := range moves {
			moved += f.held.repoint(m.Window, m.From, m.To, m.Skip, m.Seq)
			moved += f.retained.repoint(m.Window, m.From, m.To, m.Skip, m.Seq)
		}
		return nil
	})
	return moved, hd, err
}

// RepointRecord moves every held and retained piece of id that sits at record
// from to record to, keeping each piece's extent, skip, version, sequence
// number, offloaded bit and superseder: the record is the same one, read from
// another place, and a copy carries its original's sequence number. It
// returns the number of pieces moved, and runs as the one-step batch the
// record's pieces spell out. It takes the file's lock exclusively. A
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
// own extent and skip, as one batch of RepointMove over the record's pieces:
// the set-level repoint rewrites the skip of what it moves from the window it
// is given, so a whole-file window would not keep it.
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
		n += f.held.repoint(a.e, from, to, a.skip, 0)
	}
	for _, a := range kept {
		n += f.retained.repoint(a.e, from, to, a.skip, 0)
	}
	return n
}
