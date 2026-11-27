package journal

import (
	"slices"
	"testing"

	"github.com/cubbit/dittofs/pkg/journal/internal/format"
)

// TestOpenRetiresTheSegmentNothingHolds holds the open's retire step at its
// own caller: the public Open retires a sealed segment whose one record a
// newer version supersedes — nothing holds it — through the journal's own
// retirement path, its file leaving the directory, while the segment still
// holding the serving record stands. The recovery-side halves of the row —
// the candidate class, the exemptions, the recycle — pin at the recovery
// package; this row is the wiring the journal's own open consumes, so an
// emptying the open never runs fails here.
func TestOpenRetiresTheSegmentNothingHolds(t *testing.T) {
	f := seededJournal(t)
	file := fileOf(4)
	plantSealed(t, f.seam, 88, plantRecord(1, ownerTag, file, 50, 60, 0, 32))
	plantSegment(t, f.seam, 89, plantRecord(1, ownerTag, file, 51, 64, 0, 32))

	j := f.publicOpen(t, nil)

	names := f.segmentFileNames(t)
	if slices.Contains(names, format.SegmentName(88)) {
		t.Errorf("the directory holds the superseded segment %v after the open, want it retired: %v", 88, names)
	}
	if !slices.Contains(names, format.SegmentName(89)) {
		t.Errorf("the directory lost the segment holding the serving record 89, want it standing: %v", 89)
	}
	if got := j.Stats().Counters.FreedBytes; got == 0 {
		t.Errorf("FreedBytes after the open of a directory holding a dead segment got %d, want the retired segment's accounted size counted", got)
	}
}
