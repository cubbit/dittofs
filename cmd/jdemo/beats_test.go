package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBasicStoryRunsToQuit(t *testing.T) {
	// A script of Enters carries the whole story; the (f) look rides the
	// roll beat's own chooser, right after the big write ran.
	script := strings.Repeat("\n\n", 4) // beats 1–4, chooser left on each
	script += "\nf\n\n"                 // run the roll, look at the filesystem, next
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
	// The roll: the filesystem look after the 300 MB write shows at least
	// two segment files.
	segs := strings.Count(screen, ".seg")
	if segs < 2 {
		t.Errorf("story: filesystem look shows %d segment names, want the roll's two or more:\n%s", segs, screen)
	}
	if !strings.Contains(screen, "Truncate(ctx, FILE, 32)") && !strings.Contains(screen, "Truncate(ctx, FILE,  32)") {
		t.Errorf("story: the truncate beat's call missing:\n%s", screen)
	}
	// Review Focus 4: a stub answer prints verbatim and the story goes on.
	stubAt := strings.Index(screen, "journal: not implemented")
	verifyAt := strings.LastIndex(screen, "verify")
	if stubAt < 0 {
		t.Errorf("story: no stub answer shown verbatim:\n%s", screen)
	} else if verifyAt >= 0 && verifyAt < stubAt {
		t.Errorf("story: the verify beat ran before the stub answer; the story should have passed through it:\n%s", screen)
	}
}
