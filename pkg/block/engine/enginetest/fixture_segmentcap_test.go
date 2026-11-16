package enginetest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/block/engine"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/kit"
)

// WithSegmentCap bounds every segment file the fixture's journal directory
// holds: the cap a row names reaches the journal's own settings, so the
// preallocated segments a row reopens over are as small as the row chose,
// not the format's own. A row that names none keeps the format's size.
func TestWithSegmentCapBoundsEverySegmentFile(t *testing.T) {
	const segCap = int64(4 << 20) // 4 MiB

	f := New(t, WithSegmentCap(segCap))
	_, id := f.File("a")

	opts := engine.WriteOpts{Done: func() {}}
	if _, err := f.E.Write(t.Context(), f.Share.ID, id, 0, make([]byte, 512), opts); err != nil {
		t.Fatalf("a first write under the cap: %v", err)
	}

	segs, err := filepath.Glob(filepath.Join(f.Dir, "*.seg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 2 {
		t.Fatalf("found %d segment files in %s, want the active one and a spare at least", len(segs), f.Dir)
	}
	for _, s := range segs {
		if size := fileSizeOf(t, s); size > segCap {
			t.Errorf("segment %s is %d bytes, want at most the cap %d", s, size, segCap)
		}
	}

	// The idle sealer's threshold scales with the cap - its own sixteenth,
	// the ratio the journal's own fixture uses - or the default would ask
	// for more payload than a segment under the cap can hold.
	if want := segCap / 16; f.settings.IdleSealBytes != want {
		t.Errorf("IdleSealBytes = %d, want the cap's sixteenth %d so idle sealing still fires under it", f.settings.IdleSealBytes, want)
	}
}

// TestWithSegmentCapTheIdleSealerStillFires pins the scaling's point: a
// segment holding more than the scaled threshold still seals once the clock
// passes the idle-seal age - without the scaling, the default asks for more
// payload than a segment under the cap can hold, and the sealer never fires.
// The idle seal rides the fixture's own clock, and the bounded poll below is
// the assertion, for nothing synchronises the sealer's goroutine with this
// one once the clock has moved.
func TestWithSegmentCapTheIdleSealerStillFires(t *testing.T) {
	const segCap = int64(4 << 20) // 4 MiB; the scaled threshold is its sixteenth

	f := New(t, WithSegmentCap(segCap))
	_, id := f.File("a")

	opts := engine.WriteOpts{Done: func() {}}
	if _, err := f.E.Write(t.Context(), f.Share.ID, id, 0, make([]byte, segCap/8), opts); err != nil {
		t.Fatalf("a write past the scaled threshold: %v", err)
	}
	if _, _, err := f.E.Commit(t.Context(), f.Share.ID, id, 0, 0); err != nil {
		t.Fatalf("the write's sync: %v", err)
	}

	truncates := f.Seam.Calls(kit.OpTruncate)
	f.Clock.Advance(journaltest.IdleSealAge)
	for deadline := time.Now().Add(10 * time.Second); f.Seam.Calls(kit.OpTruncate) == truncates; {
		if time.Now().After(deadline) {
			t.Fatalf("no segment was truncated within the 10s backstop of advancing the clock past the idle-seal age: the sealer never fires under a cap of %d", segCap)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fileSizeOf(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
