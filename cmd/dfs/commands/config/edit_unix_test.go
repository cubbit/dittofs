//go:build unix

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// With no --config, edit opens the file DITTOFS_CONFIG names, not the
// default one, and opens a file the start would refuse as well: edit checks
// nothing.
func TestEditFollowsDittofsConfig(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	def := filepath.Join(home, ".config", "dittofs", "config.yaml")

	if err := os.MkdirAll(filepath.Dir(def), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(def, []byte("logging:\n  level: warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{"logging:\n  level: warn\n", fourRefusals} {
		dir := t.TempDir()
		opened := filepath.Join(dir, "opened")
		editor := filepath.Join(dir, "editor.sh")
		script := "#!/bin/sh\nprintf '%s' \"$1\" > '" + opened + "'\n"

		if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}

		path := writeConfig(t, body)
		environ := []string{"DITTOFS_CONFIG=" + path, "HOME=" + home, "EDITOR=" + editor}

		var out bytes.Buffer

		if err := runEdit("", environ, strings.NewReader(""), &out, &out); err != nil {
			t.Fatalf("edit: %v\noutput:\n%s", err, out.String())
		}

		got, err := os.ReadFile(opened)
		if err != nil {
			t.Fatalf("the editor was not run: %v", err)
		}

		if string(got) != path {
			t.Errorf("edit opened %q, want %q (DITTOFS_CONFIG)", got, path)
		}
	}
}

// With EDITOR unset, edit runs the editor VISUAL names in the environment it
// is handed.
func TestEditFallsBackToVisual(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	opened := filepath.Join(dir, "opened")
	editor := filepath.Join(dir, "visual.sh")
	script := "#!/bin/sh\nprintf '%s' \"$1\" > '" + opened + "'\n"

	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	path := writeConfig(t, "logging:\n  level: warn\n")
	environ := []string{"VISUAL=" + editor}

	var out bytes.Buffer

	done := make(chan error, 1)

	go func() {
		done <- runEdit(path, environ, strings.NewReader(""), &out, &out)
	}()

	// The bounded wait is the assertion: the fallback, vi, is looked up in
	// the process's PATH and can wait for a terminal forever.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("edit: %v\noutput:\n%s", err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("edit did not return in 10s: it ran another editor than the one VISUAL names")
	}

	got, err := os.ReadFile(opened)
	if err != nil {
		t.Fatalf("the editor VISUAL names was not run: %v", err)
	}

	if string(got) != path {
		t.Errorf("edit opened %q, want %q", got, path)
	}
}
