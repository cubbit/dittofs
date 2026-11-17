package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// newStoryDemo wires a demo whose registry the scenarios package filled:
// the story-level tests live above both the machinery and the stories.
func newStoryDemo(t *testing.T, dir, stdin string) (*jdemo.Demo, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	d := jdemo.NewDemo(dir, strings.NewReader(stdin), &out)
	d.Options = []journal.Option{journaltest.SuitableMount()}
	return d, &out
}

func TestBasicStoryRunsToQuit(t *testing.T) {
	// A script of Enters carries the whole story; the (+) look rides the
	// roll beat's own chooser — the journal's own counter names the roll,
	// where the file names alone cannot (a spare already sits as a .seg).
	script := strings.Repeat("\n\n", 4) // beats 1–4, chooser left on each
	script += "\n+\n\n"                 // run the roll, look at the stats, next
	script += strings.Repeat("\n\n", 12)

	base := t.TempDir()
	d, out := newStoryDemo(t, filepath.Join(base, "journal"), script)
	if err := d.Run(); err != nil {
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
	d, out := newStoryDemo(t, filepath.Join(base, "journal"), script)
	if err := d.Run(); err != nil {
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

func TestDemoEndToEndInteractive(t *testing.T) {
	// The whole demo, driven as a presenter would: run every beat, look at
	// every view once, quit. The stub answers print honestly in their
	// beats, and the journal closes clean.
	base := t.TempDir()
	dir := filepath.Join(base, "journal")
	// One beat's script: Enter runs it; each view key gets a look at the
	// chooser; an empty line leaves for the next beat.
	beatScript := "\n" + "i\n" + "s\n" + "f\n" + "d\n" + "+\n" + "\n"
	script := strings.Repeat(beatScript, 12) // basic, before Sync grows it to 13
	script += "q\n"
	d, out := newStoryDemo(t, dir, script)
	if err := d.Run(); err != nil {
		t.Fatalf("run: %v (out tail %q)", err, tailOf(out.String(), 400))
	}
	screen := out.String()
	for _, want := range []string{
		"(first look)", // a view rendered its rows
		"→ v1",         // the first write's version
		"journal: not implemented",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("end-to-end screen lacks %q (tail: %s)", want, tailOf(screen, 400))
		}
	}
	if _, err := os.Stat(jdemo.StatePath(dir)); os.IsNotExist(err) {
		t.Errorf("state file missing after the interactive run")
	}
}

// tailOf is the screen's last n bytes, for failure messages.
func tailOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
