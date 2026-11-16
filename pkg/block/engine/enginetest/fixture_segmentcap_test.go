package enginetest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/engine"
)

// WithSegmentCap bounds every segment file the fixture's journal directory
// holds: the cap a row names reaches the journal's own settings, so the
// preallocated segments a row reopens over are as small as the row chose,
// not the format's own. A row that names none keeps the format's size.
func TestWithSegmentCapBoundsEverySegmentFile(t *testing.T) {
	const cap = int64(4 << 20) // 4 MiB

	f := New(t, WithSegmentCap(cap))
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
		if size := fileSizeOf(t, s); size > cap {
			t.Errorf("segment %s is %d bytes, want at most the cap %d", s, size, cap)
		}
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
