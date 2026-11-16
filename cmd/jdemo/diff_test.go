package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// diffDemo is a demo wired for the layer only: no journal, just a screen.
func diffDemo(t *testing.T) (*demo, *strings.Builder) {
	t.Helper()
	var out strings.Builder
	d := newDemo(filepath.Join(t.TempDir(), "journal"), strings.NewReader(""), &out)
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
			d.renderView("v", c.first)
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

func TestFirstLookThenDiffsSurviveBetweenBeats(t *testing.T) {
	d, _ := diffDemo(t)
	screen := d.renderView("v", []viewRow{{key: "a", text: "one"}})
	if !strings.Contains(screen, "(first look)") {
		t.Errorf("first render lacks its marker:\n%s", screen)
	}
	if strings.Contains(screen, "+") {
		t.Errorf("first render marks rows as additions:\n%s", screen)
	}

	// A second, same render shows no markers now.
	if screen := d.renderView("v", []viewRow{{key: "a", text: "one"}}); strings.Contains(screen, "(") {
		t.Errorf("unchanged second render carries a marker:\n%s", screen)
	}

	// The spec's whole point: a change between one view's consecutive
	// looks — what a mutating beat is — IS the diff shown, not wiped.
	// The look after the change marks the turned row as changed.
	screen = d.renderView("v", []viewRow{{key: "a", text: "ONE!"}, {key: "b", text: "two"}})
	if !strings.Contains(screen, "~ ONE!") || !strings.Contains(screen, "+ two") {
		t.Errorf("look after a change lacks its +/~ marks — the diff layer is not diffing:\n%s", screen)
	}
	if strings.Contains(screen, "(first look)") {
		t.Errorf("look after a change reset to a first look — snapshots must survive between looks:\n%s", screen)
	}
}
