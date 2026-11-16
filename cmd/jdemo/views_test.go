package main

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
	defer func() { _ = d.j.Close() }()
	h, err := d.j.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	if _, err := h.WriteAt(context.Background(), FILE, 0, []byte("hello world"), 0); err != nil {
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
	if !strings.Contains(screen, statePath(dir)) {
		t.Errorf("filesystem view lacks the state file's label:\n%s", screen)
	}
	// The state file sits outside the journal dir; the view must not be
	// confused into listing it as a journal file.
	if strings.Contains(screen, "jdemo.json\t") {
		t.Errorf("filesystem view seems to list the state file inside the dir:\n%s", screen)
	}
}
