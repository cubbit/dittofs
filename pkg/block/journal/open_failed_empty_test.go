package journal

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/durability"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/format"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// recordSeqs answers the seq of every record of a content kind that
// the directory's segment files hold, durably or as the page cache shows them, with the segment each sits in.
func (f *assembledJournal) recordSeqs(t *testing.T, durable bool) map[core.Seq]core.SegmentID {
	t.Helper()
	out := map[core.Seq]core.SegmentID{}
	for _, name := range f.segmentFileNames(t) {
		kind, id := format.ClassifyName(name)
		if kind != format.NameSegment {
			continue
		}
		var b []byte
		var err error
		if durable {
			b, err = f.seam.Durable(name)
		} else {
			b, err = f.cached(name)
		}
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scan, err := format.ScanSegmentHeaders(bytes.NewReader(b), int64(len(b)), f.id, id)
		if err != nil {
			continue // a spare or a torn file holds no record a read could serve
		}
		for _, r := range scan.Records {
			if r.Header.Kind == format.KindWrite {
				out[r.Header.Seq] = id
			}
		}
	}
	return out
}

// failedJournal runs a journal whose device turns read-only under n unsynced
// writes after one synced record: the n records join the directory's failed
// list, and the journal is closed in place for the caller's reopen. It
// answers the failed seqs, ascending, and the synced record's bytes.
func (f *assembledJournal) failedJournal(t *testing.T, n int) (failed []core.Seq, kept []byte) {
	t.Helper()
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	fa := fileOf(1)
	kept = []byte("the synced record the read-only journal keeps")
	writeAtReady(t, h, fa, 0, kept)
	if _, err := h.Sync(t.Context(), fa); err != nil {
		t.Fatalf("Sync the record the read-only journal keeps: %v", err)
	}
	for i := 0; i < n; i++ {
		writeAtReady(t, h, fa, int64(1024*(i+1)), payload(40))
	}
	// The records' bytes reach the file; only the syncs and the structure
	// writes fail, so the failed records stay readable on disk.
	writingOps := []string{kit.OpSync, kit.OpCreate, kit.OpRename, kit.OpRemove, kit.OpSyncDir}
	rules := make([]kit.Rule, 0, len(writingOps))
	for _, op := range writingOps {
		rules = append(rules, kit.Failing(op, "*", 0, syscall.EROFS))
	}
	if err := f.seam.Schedule(rules...); err != nil {
		t.Fatalf("schedule the read-only device: %v", err)
	}
	if _, err := h.Sync(t.Context(), fa); !errors.Is(err, ErrJournalReadOnly) {
		t.Fatalf("the Sync on the read-only device returned %v, want ErrJournalReadOnly", err)
	}
	waitFor(t, "the failed list to hold the failed records", 30*time.Second, func() bool {
		seqs, err := durability.FailedFor(f.dir, f.id)
		if err != nil {
			t.Fatalf("read the directory's failed list: %v", err)
		}
		failed = failedSeqsOf(seqs)
		return len(failed) == n
	})
	f.seam.SetFault(nil)
	if err := j.Close(); err != nil {
		t.Fatalf("close the read-only journal: %v", err)
	}
	return failed, kept
}

// TestReadOnlyReopenPastTheReserveEmpties holds the open's emptying of the
// segment a read-only journal's failed records sit in: three failed records
// against an open reserve of one. The reopen appends the one loss record the
// reserve holds and none past it, and instead empties the segment, its
// survivors copied out and the failed records left out of the copy, so no
// segment holds a failed record any more and no loss record is owed for it.
func TestReadOnlyReopenPastTheReserveEmpties(t *testing.T) {
	f := newAssembledJournal(t)
	failed, kept := f.failedJournal(t, 3)
	before := f.recordSeqs(t, false)
	holders := map[core.SegmentID]bool{}
	for _, seq := range failed {
		if seg, ok := before[seq]; ok {
			holders[seg] = true
		}
	}
	if len(holders) == 0 {
		t.Fatalf("the failed records %v are on no durable segment before the reopen: the fixture holds nothing the emptying could leave out", failed)
	}

	// Two headroom records keep an open reserve of one.
	j2 := f.publicOpen(t, func(s *Settings) { s.HeadroomRecords = 2 })

	if events := lossEvents(t, j2, core.LossFailedWindow); len(events) != 0 {
		t.Fatalf("the open published %+v, want no failed-window event: their failure was reported when the window failed", events)
	}
	if records := f.lossRecords(t); len(records) != 1 || records[0] != failed[0] {
		t.Fatalf("the loss records on disk = %v, want the one the reserve held, the failed seq %d alone", records, failed[0])
	}
	after := f.recordSeqs(t, false)
	for _, seq := range failed {
		if seg, still := after[seq]; still {
			t.Errorf("the failed record of seq %d is still on disk in segment %d, want the open's emptying to have left it out of the copy", seq, seg)
		}
	}
	for seg := range holders {
		if slices.Contains(f.segmentFileNames(t), format.SegmentName(seg)) {
			t.Errorf("segment %d, which held failed records, is still on disk, want it unlinked once the copy of its other records is durable", seg)
		}
	}

	// The synced record survives the emptying whole, and the failed
	// records' extents serve nothing.
	h := shareOf(t, j2, 1)
	p := make([]byte, len(kept))
	n, missing, _, _, err := h.ReadAt(t.Context(), fileOf(1), 0, p)
	if err != nil || n != len(kept) || len(missing) != 0 || !bytes.Equal(p, kept) {
		t.Fatalf("reading the synced record: n %d, missing %+v, err %v, bytes %q, want %q whole from the copy", n, missing, err, p, kept)
	}
	for i := range failed {
		q := make([]byte, 40)
		n, _, _, _, err := h.ReadAt(t.Context(), fileOf(1), int64(1024*(i+1)), q)
		if err != nil {
			t.Fatalf("reading the failed record %d's extent: %v", i, err)
		}
		if n != 0 {
			t.Errorf("the extent of failed record %d served %d bytes, want none: a failed write is never served", i, n)
		}
	}
}

// cached reads name as the page cache shows it, which the syncs that failed
// never made durable.
func (f *assembledJournal) cached(name string) ([]byte, error) {
	file, err := f.seam.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	b := make([]byte, size)
	n, err := file.ReadAt(b, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:n], nil
}
