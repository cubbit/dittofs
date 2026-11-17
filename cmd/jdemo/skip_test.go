package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSkipKeyMovesTheStoryForward(t *testing.T) {
	// A fumbled beat mid-talk must not force a restart: the `s` line at
	// any prompt announces the skip and the story continues at the next
	// beat, visibly, without touching the journal.
	script := "\n\n"      // beat 1 online, run + leave chooser
	script += "\n"        // chooser: Enter... no — skip DURING beat 2's prompt
	script += "\ns\n\n\n" // beat 2: run skipped via s? — spec: s at the RUN prompt skips the beat
	script += "\n\n\n\nq\n"
	base := t.TempDir()
	d, out := newStoryDemo(t, filepath.Join(base, "journal"), script)
	d.StoryID = "basic"
	if err := d.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	screen := out.String()
	if !strings.Contains(screen, "skip") {
		t.Errorf("the skip key left no trace; want the announced skip:\n%s", screen)
	}
	if !strings.Contains(screen, "STEP 3 OF 13") {
		t.Errorf("the story did not continue to the next beat after the skip:\n%s", screen)
	}
}
