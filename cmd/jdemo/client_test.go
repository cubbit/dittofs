package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestClientDaemonDown(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "missing.sock")
	c := Dial(socket)
	err := c.Do(context.Background(), "GET", "/health", nil, nil)
	if err == nil {
		t.Fatalf("Do against no daemon: err nil, want an unreachable error")
	}
	var buf bytes.Buffer
	friendly(&buf, err)
	want := "jdemo: daemon not reachable at " + socket + " — run 'jdemo start' first"
	if got := buf.String(); got != want+"\n" {
		t.Errorf("friendly(unreachable): got %q, want %q", got, want)
	}
}
