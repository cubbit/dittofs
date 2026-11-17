package main

import (
	"testing"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

func TestRegisteredStoriesAreBasicCrashLimits(t *testing.T) {
	// The three names arrive with their scenarios directory, and an
	// unknown name is told what exists.
	for _, name := range []string{"basic", "crash", "limits"} {
		s, ok := jdemo.Stories()[name]
		if !ok {
			t.Errorf("story %q is not registered (have %v)", name, jdemo.SortedStoryNames())
			continue
		}
		if len(s.Beats) == 0 {
			t.Errorf("story %q registered with no beats", name)
		}
	}
}

func TestLimitsStoryScalesItsWorld(t *testing.T) {
	// The limits story walks a smaller journal: the pool reserves its own
	// quarter of the maximum before any write, so the story's world is
	// two caps of content at most — sized so the SHARE's limit is what
	// the story's writes meet, not the journal's.
	lim := jdemo.Stories()["limits"].Settings
	if lim.MaxBytes != 2<<30 {
		t.Errorf("limits story MaxBytes: got %d, want 2 GiB", lim.MaxBytes)
	}
	bas := jdemo.Stories()["basic"].Settings
	if bas.MaxBytes != 4<<30 {
		t.Errorf("basic story MaxBytes: got %d, want the unchanged 4 GiB", bas.MaxBytes)
	}
	if lim.SpareTarget != bas.SpareTarget {
		t.Errorf("limits story SpareTarget %d differs from basic's %d — the pool's shape is the story's, only the ceiling scales", lim.SpareTarget, bas.SpareTarget)
	}
	_ = journal.Settings{}
}
