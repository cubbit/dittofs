package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
)

func TestLimitsRefusalIsTheShares(t *testing.T) {
	// Review Focus 1: with the pinned budget pair, the filler's refusal
	// must be the SHARE's — the story's whole lesson dies if a wrong
	// constant makes it the journal's or the spare's instead.
	base := t.TempDir()
	st := jdemo.Stories()["limits"]
	if st.Settings.MaxBytes != 2<<30 {
		t.Fatalf("test premise: limits MaxBytes is %d, want 2 GiB", st.Settings.MaxBytes)
	}

	// The filler loops until refused; every beat run by Enter.
	script := strings.Repeat("\n\n", 8) + "q\n"
	d2, out2 := newStoryDemo(t, filepath.Join(base, "journal"), script)
	d2.StoryID = "limits"
	if err := d2.Run(); err != nil {
		t.Fatalf("run: %v (out %q)", err, tailOf(out2.String(), 500))
	}
	screen := out2.String()
	if !strings.Contains(screen, "refused by the share limit") {
		t.Errorf("limits story: the share's refusal never arrived:\n%s", tailOf(screen, 900))
	}
	for _, wrong := range []string{"refused by the journal limit", "refused by the spare limit"} {
		if strings.Contains(screen, wrong) && !strings.Contains(screen, "reserve") {
			t.Errorf("limits story: a wrong refusal %q is the one the story shows", wrong)
		}
	}
}

func TestReserveEscapesTheRefusal(t *testing.T) {
	// The escape order is the story's lesson: the reservation held
	// BEFORE the fill serves a write AFTER the filler was refused.
	base := t.TempDir()
	script := strings.Repeat("\n\n", 8) + "q\n"
	d, out := newStoryDemo(t, filepath.Join(base, "journal"), script)
	d.StoryID = "limits"
	if err := d.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	screen := out.String()
	refuseAt := strings.Index(screen, "refused by the share limit")
	escapeAt := strings.Index(screen, "served through the refusal")
	if refuseAt < 0 {
		t.Fatalf("the share refusal never arrived; screen tail:\n%s", tailOf(screen, 500))
	}
	if escapeAt < refuseAt {
		t.Fatalf("the escape write (at %d) precedes the refusal (at %d) — the order is the lesson", escapeAt, refuseAt)
	}
}

func TestLimitsStoryRunsToQuit(t *testing.T) {
	base := t.TempDir()
	script := strings.Repeat("\n\n", 8) + "q\n"
	d, out := newStoryDemo(t, filepath.Join(base, "journal"), script)
	d.StoryID = "limits"
	if err := d.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	screen := out.String()
	for _, want := range []string{
		"2 GiB",                      // the world, in the banner or the attach
		"512 MiB",                    // the budget
		"refused by the share limit", // the refusal
		"served through the refusal", // the escape
		"unchanged; repack frees",    // truncate's divergence
		"journal: not implemented",   // the honesty ledger's stub
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("limits story: screen lacks %q:\n%s", want, tailOf(screen, 700))
		}
	}
}
