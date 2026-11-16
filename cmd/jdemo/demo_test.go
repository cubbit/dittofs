package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/cubbit/dittofs/pkg/block/journal/journaltest"
)

// newTestDemo wires a demo over a temp journal directory with the metadata
// of a real run: an in-memory story, an empty stdin, a buffered screen.
func newTestDemo(t *testing.T, dir, stdin string) (*demo, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	d := newDemo(dir, strings.NewReader(stdin), &out)
	d.Options = []journal.Option{journaltest.SuitableMount()}
	return d, &out
}

func TestDemoEOFEndsStoryCleanly(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "journal")
	d, out := newTestDemo(t, dir, "")

	if err := d.run(); err != nil {
		t.Fatalf("run: %v (out %q)", err, out.String())
	}
	screen := out.String()
	if !strings.Contains(screen, "journal online") {
		t.Errorf("screen: init beat's title missing; want \"journal online\" somewhere in:\n%s", screen)
	}
	if !strings.Contains(screen, "SetVersionLimit") {
		t.Errorf("screen: want the version-limit line spelling the call; got:\n%s", screen)
	}
	if _, err := os.Stat(statePath(dir)); os.IsNotExist(err) {
		t.Errorf("state file %s missing after run", statePath(dir))
	}
}

func TestDemoRefusesUsedDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Init(context.Background(), dir, demoInstallID,
		journal.WithSettings(demoSettings)); err != nil {
		t.Fatalf("handcrafted init: %v", err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	d, out := newTestDemo(t, dir, "")
	err = d.run()
	if err == nil || !strings.Contains(err.Error(), "state file") {
		t.Fatalf("run over a used dir without state: got err %v (out %q), want an error naming the state file", err, out.String())
	}
	after, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(after) != len(before) {
		t.Errorf("refused run touched the journal dir: %d entries before, %d after", len(before), len(after))
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
	script := strings.Repeat(beatScript, len(stories["basic"]))
	script += "q\n"
	d, out := newTestDemo(t, dir, script)
	if err := d.run(); err != nil {
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
	if _, err := os.Stat(statePath(dir)); os.IsNotExist(err) {
		t.Errorf("state file missing after the interactive run")
	}
}

func tailOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
