package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBasicStoryRunsToQuit(t *testing.T) {
	// A script of Enters carries the whole story; the (+) look rides the
	// roll beat's own chooser — the journal's own counter names the roll,
	// where the file names alone cannot (a spare already sits as a .seg).
	script := strings.Repeat("\n\n", 4) // beats 1–4, chooser left on each
	script += "\n+\n\n"                 // run the roll, look at the stats, next
	script += strings.Repeat("\n\n", 12)

	base := t.TempDir()
	d, out := newTestDemo(t, filepath.Join(base, "journal"), script)
	if err := d.run(); err != nil {
		t.Fatalf("run: %v (out %q)", err, out.String())
	}
	screen := out.String()

	if !strings.Contains(screen, `[]byte("hello world")`) {
		t.Errorf("story: the first write's call not shown source-style:\n%s", screen)
	}
	if !strings.Contains(screen, "→ v1") {
		t.Errorf("story: the first write's issued version missing:\n%s", screen)
	}
	// The roll, from the journal's own counter: the sealed segment plus
	// the active one now holding the pattern.
	if !strings.Contains(screen, "2 (1 sealed)") {
		t.Errorf("story: the stats look after the roll does not name segments 2 (1 sealed):\n%s", screen)
	}
	if !strings.Contains(screen, "Truncate(ctx, FILE, 32)") {
		t.Errorf("story: the truncate beat's call missing:\n%s", screen)
	}
	// Review Focus 4: a stub answer prints verbatim and the story goes on
	// through it — the verify beat must run AFTER the stub it passes.
	stubAt := strings.Index(screen, "journal: not implemented")
	verifyAt := strings.Index(screen, "the index checked against the segments")
	if stubAt < 0 {
		t.Errorf("story: no stub answer shown verbatim:\n%s", screen)
	} else if verifyAt <= stubAt {
		t.Errorf("story: the verify beat (at %d) did not run after the stub answer (at %d):\n%s", verifyAt, stubAt, screen)
	}
}

func TestBeatFramingCarriesBannerHintAndChecks(t *testing.T) {
	// The polished beat: a step banner, the look-for hints before the
	// Enter, and after the run the same list confirmed by facts measured
	// from the journal's own stats — ordered as the room sees them.
	script := strings.Repeat("\n\n", 3) // beats 1–3
	base := t.TempDir()
	d, out := newTestDemo(t, filepath.Join(base, "journal"), script)
	if err := d.run(); err != nil {
		t.Fatalf("run: %v (out %q)", err, out.String())
	}
	screen := out.String()

	for _, want := range []string{
		"· STEP 3 OF 12 ·",
		"look for:",      // the hints, before the Enter
		"one held entry", // beat 3's hint prose
		"✓ index +1 entry",
		"✓ dirty +11 B",
		"✓ files +1",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("framing: screen lacks %q:\n%s", want, screen)
		}
	}
	if strings.Index(screen, "look for:") > strings.Index(screen, "✓ index +1 entry") {
		t.Errorf("framing: the confirmation came before the hint it confirms:\n%s", screen)
	}
}
