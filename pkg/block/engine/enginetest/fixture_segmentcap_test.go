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

	// A cap the idle sealer's default threshold would outrank - the format's
	// 16 MiB - scales the threshold with it, or no segment under the cap could
	// ever hold enough payload for the sealer to fire: the same sixteenth the
	// journal's own fixture keeps its sealer alive under its small cap.
	if want := min(int64(16<<20), segCap/16); f.settings.IdleSealBytes != want {
		t.Errorf("IdleSealBytes = %d, want the cap's own %d so idle sealing still fires under it", f.settings.IdleSealBytes, want)
	}
}

// A fixture with no segment cap leaves IdleSealBytes at zero, the default's
// own, resolved at the journal's open - the option scales it, only the
// option.
func TestWithNoSegmentCapTheIdleSealThresholdStaysTheDefault(t *testing.T) {
	if plain := New(t); plain.settings.IdleSealBytes != 0 {
		t.Errorf("a fixture with no segment cap has IdleSealBytes %d, want the default's own zero, resolved at open", plain.settings.IdleSealBytes)
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
