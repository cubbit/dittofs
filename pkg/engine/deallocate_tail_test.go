package engine_test

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/engine"
	"github.com/cubbit/dittofs/pkg/engine/enginetest"
	"github.com/cubbit/dittofs/pkg/journal"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
)

// tailFile is a file of the fixture's share whose store records no write: the
// journal alone holds its bytes.
type tailFile struct {
	f  *enginetest.Fixture
	h  entity.Handle
	id entity.FileID
}

func newTailFile(t *testing.T) tailFile {
	t.Helper()

	f := enginetest.New(t)
	h, id := f.File("a")

	return tailFile{f: f, h: h, id: id}
}

func (w tailFile) write(t *testing.T, off int64, p []byte) {
	t.Helper()

	o := engine.WriteOpts{Done: func() {}}
	if _, err := w.f.E.Write(t.Context(), w.f.Share.ID, w.id, off, p, o); err != nil {
		t.Fatalf("write %d bytes at %d: %v", len(p), off, err)
	}
}

// commit makes the journal durable for the file: the group commit has not run,
// so the store still records nothing.
func (w tailFile) commit(t *testing.T) {
	t.Helper()

	if _, _, err := w.f.E.Commit(t.Context(), w.f.Share.ID, w.id, 0, 0); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// deallocateRefused deallocates [off, off+n) with the store failing every
// update: the journal step runs and phase 1 answers a retry.
func (w tailFile) deallocateRefused(t *testing.T, off, n int64) {
	t.Helper()
	w.deallocateRefusedIn(t, t.Context(), off, n)
}

// deallocateRefusedIn is deallocateRefused with the call's context.
func (w tailFile) deallocateRefusedIn(t *testing.T, ctx context.Context, off, n int64) {
	t.Helper()

	w.f.FailUpdatesAfter(0)

	err := w.f.E.Deallocate(ctx, w.f.Share.ID, w.id, off, n)

	if _, ok := errors.AsType[*vfserr.Delay](err); !ok {
		t.Fatalf("deallocate [%d, %d): %v, want a retry-later delay", off, off+n, err)
	}
}

// since is every change the journal holds for the file.
func (w tailFile) since(t *testing.T) iter.Seq2[journal.Change, error] {
	t.Helper()

	return w.f.Handle().Since(t.Context(), journal.NewFileID(w.id), journal.Version{})
}

func (w tailFile) allocation(t *testing.T, off int64) entity.Span {
	t.Helper()

	sp, err := w.f.E.Allocation(t.Context(), w.f.Share.ID, w.id, off)
	if err != nil {
		t.Fatalf("allocation at %d: %v", off, err)
	}

	return sp
}

func (w tailFile) read(t *testing.T, off int64, n int) []byte {
	t.Helper()

	p := make([]byte, n)
	if got, err := w.f.E.Read(t.Context(), w.f.Share.ID, w.id, off, p); err != nil || got != n {
		t.Fatalf("read %d bytes at %d: %d, %v", n, off, got, err)
	}

	return p
}

// Deallocating the tail of a size only the journal records, then a crash
// before phase 1 commits, leaves the file at its size, ending in a hole.
func TestADeallocatedTailKeepsItsSizeAcrossACrash(t *testing.T) {
	w := newTailFile(t)
	w.write(t, 0, bytes.Repeat([]byte{'a'}, 8192))
	w.commit(t)
	w.deallocateRefused(t, 4096, 4096)
	w.f.Crash()

	if got := w.f.Committed(w.h).Size; got != 8192 {
		t.Errorf("size after the crash %d, want 8192", got)
	}

	if sp := w.allocation(t, 4096); sp.Class != entity.Hole || sp.Offset != 4096 ||
		sp.Length != 4096 {
		t.Errorf("allocation at 4096: %+v, want the hole [4096, 8192)", sp.Extent)
	}

	if got := w.read(t, 0, 4096); !bytes.Equal(got, bytes.Repeat([]byte{'a'}, 4096)) {
		t.Errorf("read [0, 4096) after the crash: %q, want 4096 'a'", got)
	}
}

// A deallocate past the end leaves the size where the write put it, and one of
// the whole file keeps the size and leaves a hole of it.
func TestADeallocatedTailKeepsItsSizeWhateverTheExtent(t *testing.T) {
	for _, c := range []struct {
		name     string
		off, n   int64
		wantHole entity.Extent
	}{
		{"past the end", 4096, 12288, entity.Extent{Offset: 4096, Length: 4096}},
		{"the whole file", 0, 8192, entity.Extent{Offset: 0, Length: 8192}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newTailFile(t)
			w.write(t, 0, bytes.Repeat([]byte{'a'}, 8192))
			w.commit(t)
			w.deallocateRefused(t, c.off, c.n)
			w.f.Crash()

			if got := w.f.Committed(w.h).Size; got != 8192 {
				t.Errorf("size after the crash %d, want 8192", got)
			}

			sp := w.allocation(t, c.wantHole.Offset)
			if sp.Class != entity.Hole || sp.Extent != c.wantHole {
				t.Errorf(
					"allocation at %d: %+v, want the hole %+v",
					c.wantHole.Offset,
					sp.Extent,
					c.wantHole,
				)
			}
		})
	}
}

// The grow is made at the size the file has: a store that records 16384 stays
// at 16384 over a write that ends below it.
func TestADeallocateKeepsTheRecordedSizeAboveItsWrites(t *testing.T) {
	w := newTailFile(t)
	w.write(t, 0, bytes.Repeat([]byte{'a'}, 16384))

	if err := w.f.E.CommitExistence(t.Context(), w.f.Share.ID, w.id); err != nil {
		t.Fatal(err)
	}

	w.write(t, 0, bytes.Repeat([]byte{'b'}, 8192))
	w.deallocateRefused(t, 4096, 4096)
	w.f.Crash()

	if got := w.f.Committed(w.h).Size; got != 16384 {
		t.Errorf("size after the crash %d, want 16384", got)
	}

	if got := w.read(t, 8192, 8192); !bytes.Equal(got, bytes.Repeat([]byte{'a'}, 8192)) {
		t.Errorf("read [8192, 16384) after the crash: %q, want 8192 'a'", got)
	}
}

// A deallocate that does not cover the last byte of a size a write set stages
// nothing but its own marker.
func TestADeallocateStagesNoTruncateItDoesNotNeed(t *testing.T) {
	for _, c := range []struct {
		name     string
		off, n   int64
		recorded bool
	}{
		{"in the middle", 2048, 2048, false},
		{"wholly past the end", 8192, 4096, false},
		{"over a size the store records", 4096, 4096, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newTailFile(t)
			w.write(t, 0, bytes.Repeat([]byte{'a'}, 8192))
			w.commit(t)

			if c.recorded {
				if err := w.f.E.CommitExistence(t.Context(), w.f.Share.ID, w.id); err != nil {
					t.Fatal(err)
				}
			}

			w.deallocateRefused(t, c.off, c.n)

			var kinds []journal.ChangeKind

			for ch, err := range w.since(t) {
				if err != nil {
					t.Fatalf("since: %v", err)
				}

				kinds = append(kinds, ch.Kind)
			}

			for _, k := range kinds {
				if k == journal.ChangeTruncate {
					t.Errorf("the journal yields a truncate marker %v, want none", kinds)
				}
			}

			w.f.Crash()

			if got := w.f.Committed(w.h).Size; got != 8192 {
				t.Errorf("size after the crash %d, want 8192", got)
			}
		})
	}
}

// A size a truncate set is recorded by that truncate: a deallocate over its
// last byte stages no second one.
func TestADeallocateOverAGrownTailStagesNoSecondTruncate(t *testing.T) {
	w := newTailFile(t)
	w.write(t, 0, bytes.Repeat([]byte{'a'}, 8192))
	w.commit(t)

	if err := w.f.E.Truncate(t.Context(), w.f.Share.ID, w.id, 10000); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	w.deallocateRefused(t, 8192, 4096)

	truncates := 0

	for ch, err := range w.since(t) {
		if err != nil {
			t.Fatalf("since: %v", err)
		}

		if ch.Kind == journal.ChangeTruncate {
			truncates++
		}
	}

	if truncates != 1 {
		t.Errorf("the journal yields %d truncate markers, want the truncate's alone", truncates)
	}

	w.f.Crash()

	if got := w.f.Committed(w.h).Size; got != 10000 {
		t.Errorf("size after the crash %d, want 10000", got)
	}
}

// The grow a deallocate stages is no write: a deallocate that suspends the
// modification time leaves the overlay's, which the pending writes set, where
// it was.
func TestADeallocateOverTheTailLeavesTheOverlayMtimeAlone(t *testing.T) {
	w := newTailFile(t)
	w.write(t, 0, bytes.Repeat([]byte{'a'}, 8192))
	w.commit(t)

	before, _, err := w.f.E.Overlay(t.Context(), w.f.Share.ID, w.id)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}

	w.f.Clock.Jump(time.Second)
	w.deallocateRefusedIn(t, engine.WithSuspended(t.Context(), entity.TimeModify), 4096, 4096)

	after, _, err := w.f.E.Overlay(t.Context(), w.f.Share.ID, w.id)
	if err != nil {
		t.Fatalf("overlay: %v", err)
	}

	if !after.Mtime.Equal(before.Mtime) {
		t.Errorf("overlay mtime %v, want %v, the write's", after.Mtime, before.Mtime)
	}
}

// A write the journal lost leaves the grow before the deallocate pending: the
// file keeps the size the lost write had, and no durable write gives it.
func TestADeallocateGrowOutlivesAWriteTheJournalLost(t *testing.T) {
	w := newTailFile(t)
	w.write(t, 0, bytes.Repeat([]byte{'a'}, 8192))
	w.f.FailNextWindow()

	if err := w.f.E.Deallocate(t.Context(), w.f.Share.ID, w.id, 4096, 4096); err != nil {
		t.Fatalf("deallocate: %v", err)
	}

	w.commit(t)

	if err := w.f.E.CommitExistence(t.Context(), w.f.Share.ID, w.id); err != nil {
		t.Fatalf("commit existence: %v", err)
	}

	if got := w.f.Committed(w.h).Size; got != 8192 {
		t.Errorf("size the store records %d, want 8192", got)
	}
}
