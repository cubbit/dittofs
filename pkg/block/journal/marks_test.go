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

// TestMarksRecordedNeverLeavesStatsOlder covers MarksRecorded's guarantee:
// after MarksRecorded(m) no later Stats names m's stream with an older entry,
// whether m is the entry itself, older than it, or past it.
func TestMarksRecordedNeverLeavesStatsOlder(t *testing.T) {
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
	j.MarksRecorded([]SyncMark{older})
	if got := j.Stats().SyncMarks[m.Stream]; got != m {
		t.Fatalf("after MarksRecorded of an older mark Stats names %+v, want the entry %+v unchanged", got, m)
	}
	j.MarksRecorded([]SyncMark{m})
	if got := j.Stats().SyncMarks[m.Stream]; got != m {
		t.Fatalf("after MarksRecorded of the entry itself Stats names %+v, want %+v", got, m)
	}
	past := m
	past.Through, past.Offset, past.Sequence = m.Through+4096, m.Offset+4000, m.Sequence+1
	j.MarksRecorded([]SyncMark{past})
	if got := j.Stats().SyncMarks[m.Stream]; got != past {
		t.Fatalf("after MarksRecorded of a mark past the entry Stats names %+v, want %+v: an entry never older than a recorded mark", got, past)
	}
}

// TestMarksRecordedAfterCloseIsInert covers a MarksRecorded the group commit
// makes after the journal closed: it returns, and Stats stays the zero value.
func TestMarksRecordedAfterCloseIsInert(t *testing.T) {
	f := newAssembledJournal(t)
	j := f.open(t)
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	j.MarksRecorded([]SyncMark{{Stream: 0, Segment: 1, Through: 10, Offset: 9, Sequence: 1}})
	if got := j.Stats().SyncMarks; len(got) != 0 {
		t.Fatalf("Stats().SyncMarks = %+v after Close, want none", got)
	}
}
