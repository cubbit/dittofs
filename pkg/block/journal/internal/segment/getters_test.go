package segment_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// TestGettersSetTagBytes checks TagFigures' bytes equals ShareBytes per tag and is a
// copy the caller may change.
func TestGettersSetTagBytes(t *testing.T) {
	e := streamOver(t, 64<<10, func(streamEnv) {})
	for tag, size := range map[core.ShareTag]int{7: 100, 9: 300} {
		payload := streamTestBody(size, 0x11)
		h, b := streamWriteHeader(streamFileID(1), 0, payload, tag, core.Version{Counter: 1})
		streamAppendWrite(t, e, h, b)
	}
	got, _ := e.set.TagFigures()
	if len(got) != 2 {
		t.Fatalf("TagFigures bytes has %d tags, want 2: %v", len(got), got)
	}
	for _, tag := range []core.ShareTag{7, 9} {
		if want := e.set.ShareBytes(tag); got[tag] != want || want == 0 {
			t.Fatalf("TagFigures bytes[%d] = %d, want ShareBytes %d (non-zero)", tag, got[tag], want)
		}
	}
	got[7] = -1
	if e.set.ShareBytes(7) == -1 {
		t.Fatal("TagFigures bytes shares its map with the set, want a copy")
	}
}
