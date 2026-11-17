package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncBeatPrintsItsMark(t *testing.T) {
	// The sync beat is REAL on this base: the mark it prints is the
	// receipt durability hands the caller, its Segment naming where the
	// record the dump walks sits.
	script := strings.Repeat("\n\n", 6) + "q\n" // run all beats to the sync one
	base := t.TempDir()
	d, out := newStoryDemo(t, filepath.Join(base, "journal"), script)
	if err := d.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	screen := out.String()
	for _, want := range []string{
		"h.Sync(ctx, FILE)", // the call, storybook-qualified
		"Through:",          // …the mark's fields
		"Sequence:",
		"Segment:",     // a real mark, naming a real segment
		"STEP 6 OF 13", // the beat's place in the story
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("sync beat: screen lacks %q:\n%s", want, tailOf(screen, 600))
		}
	}
}
