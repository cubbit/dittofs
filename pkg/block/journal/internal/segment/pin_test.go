package segment_test

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/index"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/reclaim"
	"github.com/cubbit/dittofs/pkg/block/journal/internal/segment"
)

// TestAppendRollPinsTheSegmentBeforeASealCanDrainIt: the pin is raised inside
// the append, so a seal that runs the moment the append leaves the drain finds
// the segment already held. A pin raised after AppendRoll returned would leave
// the segment unheld in that gap, and a retirement there would drop a record
// its writer has yet to publish.
func TestAppendRollPinsTheSegmentBeforeASealCanDrainIt(t *testing.T) {
	e := streamOver(t, streamCap, func(streamEnv) {})
	tr := reclaim.New(index.New(1<<40), nil)
	fid := core.NewFileID([16]byte{9})
	head, body := streamWriteHeader(fid, 0, make([]byte, 4<<10), 1, core.Version{Counter: 1})

	var sealErr error
	var held, ran bool
	e.s.SetPinnedHook(func() {
		if ran {
			return
		}
		ran = true
		seg, ok := e.s.OpenSegment()
		if !ok {
			return
		}
		_, sealErr = e.s.Seal(false)
		held = tr.Held(seg)
	})
	t.Cleanup(func() { e.s.SetPinnedHook(nil) })

	if _, err := e.s.AppendRoll(head, body, segment.Roll{Pin: tr}); err != nil {
		t.Fatalf("AppendRoll: %v", err)
	}
	if !ran {
		t.Fatal("the append never reached the pinned hook")
	}
	if sealErr != nil {
		t.Fatalf("the seal that ran as the append left the drain: %v", sealErr)
	}
	if !held {
		t.Fatal("the segment reads as unheld once sealed in the gap after the append, want it pinned inside the append")
	}
}
