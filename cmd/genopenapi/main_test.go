package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/internal/node/api"
)

// TestGenerateWritesTheDocument holds that the file the command writes is
// the document the route table generates.
func TestGenerateWritesTheDocument(t *testing.T) {
	out := filepath.Join(t.TempDir(), "openapi.json")
	if err := generate(out); err != nil {
		t.Fatalf("generate(%s): %v", out, err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want, err := api.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("generate wrote %d bytes, want the %d bytes of api.OpenAPI()", len(got), len(want))
	}
}
