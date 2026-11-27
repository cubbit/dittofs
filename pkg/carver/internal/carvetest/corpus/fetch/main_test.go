package main

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest/corpus"
)

// run fills the directory it is given from a real server on 127.0.0.1:0.
func TestRunFetchesIntoTheDirectory(t *testing.T) {
	body := bytes.Repeat([]byte("corpus "), 1<<12)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	e := corpus.Entry{Kind: "media", Name: "film.mp4", URL: srv.URL + "/film.mp4", Download: sha256.Sum256(body), Stored: sha256.Sum256(body)}

	if err := run(t.Context(), dir, []corpus.Entry{e}); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, e.Kind, e.Name))
	if err != nil {
		t.Fatalf("the fetched file: %v", err)
	}

	if !bytes.Equal(got, body) {
		t.Errorf("the fetched file: %d bytes, want the %d served", len(got), len(body))
	}
}

// run refuses an empty directory argument before it fetches anything.
func TestRunRefusesNoDirectory(t *testing.T) {
	if err := run(t.Context(), "", nil); err == nil || err.Error() != "usage: fetch <dir>" {
		t.Errorf("run with no directory: %v, want the usage error", err)
	}
}
