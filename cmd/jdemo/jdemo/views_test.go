package jdemo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

func TestPlainPathHasNoANSI(t *testing.T) {
	// Review Focus 1: the plain path — a redirected or NO_COLOR screen —
	// must emit zero escape bytes, so transcripts copy-paste clean.
	c := coloursFor(&strings.Builder{})
	samples := []string{
		c.green("row"), c.yellow("row"), c.red("row"), c.dim("row"),
	}
	for i, s := range samples {
		if strings.ContainsRune(s, 0o33) {
			t.Errorf("colour %d on the plain path emitted an escape byte: %q", i, s)
		}
		if s != "row" {
			t.Errorf("colour %d on the plain path altered the text: %q", i, s)
		}
	}
}

func TestFileSystemViewListsSegments(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "journal")
	d, out := newTestDemo(t, dir, "")
	if err := d.open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.J.Close() }()
	h, err := d.J.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if _, err := h.WriteAt(context.Background(), StoryFile, 0, []byte("hello world"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}

	d.viewFS()
	screen := out.String()
	if !strings.Contains(screen, "00000000000000000001.seg") {
		t.Errorf("filesystem view lacks the first segment's 20-digit name:\n%s", screen)
	}
	if !strings.Contains(screen, "format") {
		t.Errorf("filesystem view lacks the format file:\n%s", screen)
	}
	if !strings.Contains(screen, StatePath(dir)) {
		t.Errorf("filesystem view lacks the state file's label:\n%s", screen)
	}
	// The state file sits outside the journal dir; the view must not be
	// confused into listing it as a journal file.
	if strings.Contains(screen, "jdemo.json\t") {
		t.Errorf("filesystem view seems to list the state file inside the dir:\n%s", screen)
	}
}

func TestIndexViewAnswersRealRows(t *testing.T) {
	// The flip T7.6 made: Inspect().File answers the live index's rows,
	// not a refusal. The returned entry names the same record the dump
	// walk reads — segment 3, offset 48 — the two views agree on the
	// truth of where hello world lives.
	base := t.TempDir()
	dir := filepath.Join(base, "journal")
	d, out := newTestDemo(t, dir, "")
	if err := d.open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = d.J.Close() }()
	h, err := d.J.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if _, err := h.WriteAt(context.Background(), StoryFile, 0, []byte("hello world"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	d.viewIndex()
	screen := out.String()
	for _, want := range []string{
		"held",        // the entry's kind
		"0–11",        // its extent
		"v1",          // its version
		"record 3@48", // its backing record, where the dump walks to it
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("index view after a write lacks %q:\n%s", want, screen)
		}
	}
}

func TestViewsSurviveAClosedJournal(t *testing.T) {
	// A story that closes its journal mid-run (crash's close beat) still
	// offers the chooser; every view must name the closure plainly, not
	// deref the dead journal — this test reproduces the talk-night panic.
	base := t.TempDir()
	d, out := newTestDemo(t, filepath.Join(base, "journal"), "\n")
	if err := d.open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	h, err := d.J.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	d.H[1] = h
	d.viewIndex() // alive: no panic is the floor
	if err := d.J.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("a view through a closed journal panicked: %v", r)
		}
	}()
	for _, v := range []func(*Demo){(*Demo).viewIndex, (*Demo).viewSegments,
		(*Demo).viewFS, (*Demo).viewStats} {
		v(d)
	}
	screen := out.String()
	if !strings.Contains(screen, "closed") {
		t.Errorf("views through a closed journal do not name the closure:\n%s", screen)
	}
}
