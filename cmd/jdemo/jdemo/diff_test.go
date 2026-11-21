package jdemo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// diffDemo is a demo wired for the layer only: no journal, just a screen.
func diffDemo(t *testing.T) (*Demo, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	d := NewDemo(filepath.Join(t.TempDir(), "journal"), strings.NewReader(""), &out)
	return d, &out
}

func TestRenderViewDiffIsTransformed(t *testing.T) {
	cases := []struct {
		name  string
		first []viewRow
		next  []viewRow
		want  []string // glyph-marked fragments the second render must carry
		not   []string // fragments the plain render must not carry
	}{
		{
			name:  "added row is marked and shown",
			first: []viewRow{{key: "a", text: "one"}},
			next:  []viewRow{{key: "a", text: "one"}, {key: "b", text: "two"}},
			want:  []string{"+ two"},
			not:   []string{"+ one"},
		},
		{
			name:  "changed row is marked with what it became",
			first: []viewRow{{key: "a", text: "one"}},
			next:  []viewRow{{key: "a", text: "ONE!"}},
			want:  []string{"~ ONE!"},
		},
		{
			name:  "removed row is named",
			first: []viewRow{{key: "a", text: "one"}, {key: "b", text: "two"}},
			next:  []viewRow{{key: "a", text: "one"}},
			want:  []string{"- removed two"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, _ := diffDemo(t)
			d.recording = true
			d.renderView("v", c.first)
			d.recording = false
			screen := d.renderView("v", c.next)
			for _, w := range c.want {
				if !strings.Contains(screen, w) {
					t.Errorf("%s: second render lacks %q:\n%s", c.name, w, screen)
				}
			}
			for _, n := range c.not {
				if strings.Contains(screen, n) {
					t.Errorf("%s: second render unexpectedly carries %q:\n%s", c.name, n, screen)
				}
			}
			if strings.ContainsRune(screen, 0o33) {
				t.Errorf("%s: plain path emitted an escape byte:\n%q", c.name, screen)
			}
		})
	}
}

func TestFirstLookThenDiffAgainstTheBeatBaseline(t *testing.T) {
	d, _ := diffDemo(t)
	screen := d.renderView("v", []viewRow{{key: "a", text: "one"}})
	if !strings.Contains(screen, "(first look)") {
		t.Errorf("a look with no baseline lacks its marker:\n%s", screen)
	}
	if strings.Contains(screen, "+") {
		t.Errorf("a look with no baseline marks rows as additions:\n%s", screen)
	}

	d.recording = true
	d.renderView("v", []viewRow{{key: "a", text: "one"}})
	d.recording = false

	// Looks within one beat share its baseline: an unchanged look has no
	// marks, and a changed one shows the same diff however often it is shown.
	if screen := d.renderView("v", []viewRow{{key: "a", text: "one"}}); strings.Contains(screen, "(") {
		t.Errorf("unchanged look carries a marker:\n%s", screen)
	}
	changed := []viewRow{{key: "a", text: "ONE!"}, {key: "b", text: "two"}}
	for look := 1; look <= 2; look++ {
		screen = d.renderView("v", changed)
		if !strings.Contains(screen, "~ ONE!") || !strings.Contains(screen, "+ two") {
			t.Errorf("look %d after a change lacks its +/~ marks:\n%s", look, screen)
		}
	}
}

func TestViewDiffShowsOnlyTheCurrentBeatsChanges(t *testing.T) {
	// A view skipped in the first beat must not show that beat's change as
	// new when it is opened in the second: the diff is against the state
	// before the beat that just ran, not against the last time it was shown.
	base := t.TempDir()
	d, out := newTestDemo(t, filepath.Join(base, "journal"), "")
	if err := d.open(context.Background()); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.J.Close() })
	h, err := d.J.Share(1, 0, journal.Version{})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	d.H[1] = h

	d.snapshotViews() // the start of beat 1
	if _, err := h.WriteAt(context.Background(), StoryFile, 0, []byte("aaaa"), 0); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	// beat 1 ends without anyone opening the index
	d.snapshotViews() // the start of beat 2
	if _, err := h.WriteAt(context.Background(), StoryFile, 100, []byte("bbbb"), 0); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	d.viewIndex()

	var first, second string
	for _, l := range strings.Split(out.String(), "\n") {
		switch {
		case strings.Contains(l, "100–104"):
			second = l
		case strings.Contains(l, "  0–4"):
			first = l
		}
	}
	if !strings.HasPrefix(second, "+ ") {
		t.Errorf("beat 2's own write is not marked as new: %q", second)
	}
	if first == "" || strings.HasPrefix(first, "+ ") {
		t.Errorf("beat 1's write is marked as new in beat 2's diff (or missing): %q", first)
	}
}
