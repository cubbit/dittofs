package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The committed CLI reference is what the generator writes from the live
// command trees, so a command or flag changed without regenerating fails here.
func TestReferenceMatchesCommandTrees(t *testing.T) {
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
