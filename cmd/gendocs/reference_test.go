package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The committed CLI reference is what the generator writes from the live
// command trees, so a command or flag changed without regenerating fails here.
func TestReferenceMatchesCommandTrees(t *testing.T) {
	// decision: the reference is compared on Linux only, because dfsctl share
	// mount's mode flags default per OS and the committed page is the Linux
	// render. Drop the skip once no flag's default or help depends on the OS.
	if runtime.GOOS != "linux" {
		t.Skipf("docs/guide/cli.md is the Linux render; running on %s", runtime.GOOS)
	}
	out := filepath.Join(t.TempDir(), "cli.md")
	if err := generate(out); err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(sourceRoot, "docs", "guide", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("docs/guide/cli.md differs from the generated reference (%d bytes generated, %d committed): run `go run ./cmd/gendocs`", len(got), len(want))
	}
}
