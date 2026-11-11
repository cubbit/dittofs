package engine_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
)

// spans is what the store's content view says file f holds in [off, off+n).
func (r *rig) spans(t *testing.T, f entity.FileID, off, n int64) []entity.Span {
	t.Helper()

	var out []entity.Span

	for s, err := range r.fs.Store.Content().Covering(t.Context(), r.one.ID, f, off, n) {
		if err != nil {
			t.Fatal(err)
		}

		out = append(out, s)
	}

	return out
}

// wantOverwrites fails unless the spans of f over [off, off+n) are exactly
// want: each an extent made uncarved by an overwrite record of its version.
func (r *rig) wantOverwrites(t *testing.T, f entity.FileID, off, n int64, want []entity.Span) {
	t.Helper()

	got := r.spans(t, f, off, n)

	if len(got) != len(want) {
		t.Fatalf("Covering [%d, +%d) = %+v, want %+v", off, n, got, want)
	}

	for i, w := range want {
		g := got[i]

		if g.Extent != w.Extent || g.Class != entity.Uncarved || g.Overwrite != w.Overwrite {
			t.Errorf(
				"span %d = %v class %v overwrite %v, want %v uncarved overwrite %v",
				i, g.Extent, g.Class, g.Overwrite, w.Extent, w.Overwrite,
			)
		}
	}
}

// A write that overwrote one the crash left out of the journal's index is
// committed with the run Since names, and the store writes the overwrite
// record there, though no write of the batch covers it.
func TestStartReplaysAWriteWithTheOverSinceYields(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	r.write(t, f, 0, 4096, writeMtime)
	v2 := r.write(t, f, 0, 4096, otherMtime)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v2),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 4096},
			Mtime:   otherMtime,
			Ctime:   restartAt,
			Over:    []entity.Extent{{Offset: 0, Length: 4096}},
		},
	})

	r.wantOverwrites(t, f, 0, 4096, []entity.Span{
		{Extent: entity.Extent{Offset: 0, Length: 4096}, Overwrite: jver(v2)},
	})
	wantNoLoud(t, r)
}

// The pieces of one write that a later write cut in two carry their runs
// each, and the replay commits their union, never the gap between them.
func TestStartReplaysTheOverOfEveryPieceOfAWrite(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	r.write(t, f, 0, 16384, writeMtime)
	v2 := r.write(t, f, 0, 16384, writeMtime)
	v3 := r.write(t, f, 4096, 4096, otherMtime)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v2),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 16384},
			Mtime:   writeMtime,
			Ctime:   restartAt,
			Over: []entity.Extent{
				{Offset: 0, Length: 4096},
				{Offset: 8192, Length: 8192},
			},
		},
		{
			Version: jver(v3),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 4096, Length: 4096},
			Mtime:   otherMtime,
			Ctime:   restartAt,
			Over:    []entity.Extent{{Offset: 4096, Length: 4096}},
		},
	})

	r.wantOverwrites(t, f, 0, 16384, []entity.Span{
		{Extent: entity.Extent{Offset: 0, Length: 4096}, Overwrite: jver(v2)},
		{Extent: entity.Extent{Offset: 4096, Length: 4096}, Overwrite: jver(v3)},
		{Extent: entity.Extent{Offset: 8192, Length: 8192}, Overwrite: jver(v2)},
	})
}

// A removal marker between two writes splits the replay into two commits,
// and each carries its own write's runs: the second write's, never the
// first's.
func TestStartPassesEachBatchItsOwnOver(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	r.write(t, f, 0, 8192, writeMtime)
	v2 := r.write(t, f, 0, 8192, writeMtime)

	if _, err := r.jh.Deallocate(
		t.Context(), journal.NewFileID(f), journal.Extent{Off: 16384, Len: 4096},
	); err != nil {
		t.Fatal(err)
	}

	v4 := r.write(t, f, 4096, 4096, otherMtime)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 2 {
		t.Fatalf("%d Commit calls, want 2", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v2),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 4096},
			Mtime:   writeMtime,
			Ctime:   restartAt,
			Over:    []entity.Extent{{Offset: 0, Length: 4096}},
		},
	})
	sameOps(t, ops[1], []kvstore.PendingOp{
		{
			Version: jver(v4),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 4096, Length: 4096},
			Mtime:   otherMtime,
			Ctime:   restartAt,
			Over:    []entity.Extent{{Offset: 4096, Length: 4096}},
		},
	})
	wantNoLoud(t, r)
}

// A write the journal lost whole inside an earlier intact write is committed
// lost, with the run it superseded, so the older bytes keep their overwrite
// record: a loss carries no run of its own in Since.
func TestStartCommitsALossInsideAnEarlierWriteWithItsOver(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 12288, writeMtime)
	v2 := r.write(t, f, 4096, 4096, otherMtime)
	r.j.Lose(journal.NewFileID(f), journal.Extent{Off: 4096, Len: 4096}, journal.LossCorrupt)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 12288},
			Mtime:   writeMtime,
			Ctime:   restartAt,
		},
		{
			Version: jver(v2),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 4096, Length: 4096},
			Ctime:   restartAt,
			Over:    []entity.Extent{{Offset: 4096, Length: 4096}},
			Lost:    []entity.Extent{{Offset: 4096, Length: 4096}},
		},
	})
}

// A write lost in its middle commits the runs its held pieces superseded,
// beside its lost run.
func TestStartCommitsALossInTheMiddleOfAWriteWithItsOver(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 12288, writeMtime)
	v2 := r.write(t, f, 4096, 12288, otherMtime)
	r.j.Lose(journal.NewFileID(f), journal.Extent{Off: 8192, Len: 4096}, journal.LossCorrupt)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 4096},
			Mtime:   writeMtime,
			Ctime:   restartAt,
		},
		{
			Version: jver(v2),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 4096, Length: 12288},
			Mtime:   otherMtime,
			Ctime:   restartAt,
			Over:    []entity.Extent{{Offset: 4096, Length: 4096}},
			Lost:    []entity.Extent{{Offset: 8192, Length: 4096}},
		},
	})
}

// A write over the range of a write the journal lost takes its Over from the
// journal alone: the journal names no run for it, so it commits none, while
// the loss step keeps the run it superseded.
func TestStartCommitsAWriteAfterALossWithTheJournalsOver(t *testing.T) {
	r := newRig(t)
	_, f := r.newFile(t, "a")
	v1 := r.write(t, f, 0, 12288, writeMtime)
	v2 := r.write(t, f, 4096, 4096, otherMtime)
	r.j.Lose(journal.NewFileID(f), journal.Extent{Off: 4096, Len: 4096}, journal.LossCorrupt)
	v3 := r.write(t, f, 4096, 4096, otherMtime)

	r.begin(t)

	ops := r.ex.committedOps()

	if len(ops) != 1 {
		t.Fatalf("%d Commit calls, want 1", len(ops))
	}

	sameOps(t, ops[0], []kvstore.PendingOp{
		{
			Version: jver(v1),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 0, Length: 12288},
			Mtime:   writeMtime,
			Ctime:   restartAt,
		},
		{
			Version: jver(v2),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 4096, Length: 4096},
			Ctime:   restartAt,
			Over:    []entity.Extent{{Offset: 4096, Length: 4096}},
			Lost:    []entity.Extent{{Offset: 4096, Length: 4096}},
		},
		{
			Version: jver(v3),
			Kind:    kvstore.OpWrite,
			Extent:  entity.Extent{Offset: 4096, Length: 4096},
			Mtime:   otherMtime,
			Ctime:   restartAt,
		},
	})
}
