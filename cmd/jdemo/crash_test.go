package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func crashScript() string {
	// Six beats; an index look rides the overwrite beat's chooser, where
	// the retained range shows.
	script := strings.Repeat("\n\n", 3) // online, attach, write+sync
	script += "\ni\n\n"                 // the overwrite, looked at through the index
	script += strings.Repeat("\n\n", 3) // truncate, feed, close+refuse
	return script
}

func TestCrashStoryRunsToQuit(t *testing.T) {
	base := t.TempDir()
	d, out := newStoryDemo(t, filepath.Join(base, "journal"), crashScript())
	d.StoryID = "crash"
	if err := d.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	screen := out.String()
	for _, want := range []string{
		"h.Sync(ctx, FILE)",     // the mark, real
		"Through:", "Sequence:", // its fields
		"retained",                           // the overwrite's index truth
		"journal closed; only the directory", // the beat's guard note
		"reopened: 4 index entries before the close, 4 after",
		`"heXYo world"`, // the bytes the rebuilt journal serves
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("crash story: screen lacks %q:\n%s", want, tailOf(screen, 700))
		}
	}
	if strings.Contains(screen, "STEP 8 OF") {
		t.Errorf("crash story: an eighth beat exists; the story is seven")
	}
}
