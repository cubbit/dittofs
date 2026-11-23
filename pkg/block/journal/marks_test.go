package journal

import (
	"context"
	"testing"
)

// TestStatsSyncMarksFollowASync covers the figure past the open: a Sync that
// returns a mark leaves it as its stream's entry in Stats.SyncMarks, and a
// later Sync replaces it, so the group commit reads the stream's last sync
// and not the open's.
func TestStatsSyncMarksFollowASync(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)

	if got := j.Stats().SyncMarks; len(got) != 0 {
		t.Fatalf("Stats().SyncMarks = %+v on a journal opened over nothing, want no entry", got)
	}
	id := fileOf(1)
	var last SyncMark
	for i := range 2 {
		writeReady(t, h, id, "a record of the stream")
		marks, err := h.Sync(context.Background(), id)
		if err != nil || len(marks) != 1 {
			t.Fatalf("Sync %d = %+v, %v, want the stream's one mark", i, marks, err)
		}
		if i > 0 && marks[0] == last {
			t.Fatalf("Sync %d returned the mark of Sync %d again: %+v", i, i-1, last)
		}
		last = marks[0]
		if got := j.Stats().SyncMarks[last.Stream]; got != last {
			t.Fatalf("after Sync %d the stream's entry is %+v, want the mark Sync returned: %+v", i, got, last)
		}
	}
}

// TestSyncMarksIsTheFigureStatsServes covers the accessor the group commit
// reads: after a Sync it returns the stream's mark as a slice, the one Stats
// serves in its map.
func TestSyncMarksIsTheFigureStatsServes(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	if got := j.SyncMarks(); len(got) != 0 {
		t.Fatalf("SyncMarks() = %+v before any sync, want none", got)
	}
	id := fileOf(3)
	writeReady(t, h, id, "a record of the stream")
	marks, err := h.Sync(context.Background(), id)
	if err != nil || len(marks) != 1 {
		t.Fatalf("Sync = %+v, %v, want one mark", marks, err)
	}
	got := j.SyncMarks()
	if len(got) != 1 || got[0] != marks[0] || j.Stats().SyncMarks[marks[0].Stream] != got[0] {
		t.Fatalf("SyncMarks() = %+v, want the mark Sync returned %+v, the one Stats serves", got, marks[0])
	}
	got[0].Sequence = 999
	if again := j.SyncMarks(); again[0] != marks[0] {
		t.Fatalf("a change to the returned slice reached the next SyncMarks(): %+v", again)
	}
}

// TestMarksRecordedLeavesTheFigure covers a recorded mark's effect on what
// SyncMarks serves: none. The figure is raised by a Sync alone, so a recorded
// mark, the entry's own, an older one or one no Sync returned, is not taken as
// proof.
func TestMarksRecordedLeavesTheFigure(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	withLimit(t, j)
	h := shareOf(t, j, 1)
	id := fileOf(3)
	writeReady(t, h, id, "a record of the stream")
	marks, err := h.Sync(context.Background(), id)
	if err != nil || len(marks) != 1 {
		t.Fatalf("Sync = %+v, %v, want one mark", marks, err)
	}
	m := marks[0]

	older := m
	older.Through, older.Offset = m.Through-1, m.Offset-1
	invented := m
	invented.Through, invented.Offset, invented.Sequence = m.Through+4096, m.Offset+4000, m.Sequence+1
	for name, rec := range map[string]SyncMark{"the entry itself": m, "an older mark": older, "a mark no Sync returned": invented} {
		j.MarksRecorded([]SyncMark{rec})
		if got := j.SyncMarks(); len(got) != 1 || got[0] != m {
			t.Fatalf("after MarksRecorded of %s SyncMarks() = %+v, want the entry %+v unchanged", name, got, m)
		}
	}
}
