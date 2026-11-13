package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

func TestWriteSink(t *testing.T) {
	dir := t.TempDir()
	content := "hello world"
	f, err := os.Create(filepath.Join(dir, "source.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()

	var rawID [16]byte
	copy(rawID[:], "sink-test-file-1")
	id := journal.NewFileID(rawID)
	offer := journal.Offer{
		ID: id,
		Dirty: []journal.OfferedExtent{
			{Extent: journal.Extent{Off: 0, Len: 5}},
			{Extent: journal.Extent{Off: 8, Len: 3}},
		},
		Offered: ro,
	}

	offDir := filepath.Join(t.TempDir(), "offload")
	wrote, err := writeSink(offDir, offer)
	if err != nil {
		t.Fatalf("writeSink: %v", err)
	}
	if len(wrote) != 2 {
		t.Fatalf("writeSink reported %d extents, want the 2 dirty ones", len(wrote))
	}
	for _, e := range []struct {
		off, ln int64
		want    string
	}{
		{0, 5, "hello"},
		{8, 3, "rld"},
	} {
		name := filepath.Join(offDir, hex.EncodeToString(rawID[:]),
			pad16(e.off)+"-"+pad16(e.off+e.ln)+".dat")
		b, err := os.ReadFile(name)
		if err != nil {
			t.Errorf("extent %d–%d: reading %s: %v", e.off, e.off+e.ln, name, err)
			continue
		}
		if string(b) != e.want {
			t.Errorf("extent %d–%d: got %q, want %q", e.off, e.off+e.ln, b, e.want)
		}
	}
}

func TestOffloadParse(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bad through", []string{"offload", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "x"}, "not a version"},
		{"mark missing extents", []string{"mark-offloaded", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "--oldest", "1", "--newest", "2"}, "--extents"},
		{"bad extent len", []string{"unmark", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "--extents", "5:0"}, "extent"},
		{"bad extent pair", []string{"release", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "--extents", "x:1"}, "x:1"},
		// cobra's ExactArgs(1) refuses the missing budget before RunE.
		{"repack no budget", []string{"repack"}, "accepts 1"},
	}
	for _, c := range cases {
		_, err := exec(t, filepath.Join(t.TempDir(), "nope.sock"), c.args...)
		if err == nil {
			t.Errorf("%s: err nil, want an error containing %q", c.name, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got err %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

func TestOffloadAndRepackPropagateTheStub(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := tmpSocket(t)
	srv := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: socket, OffloadDir: filepath.Join(t.TempDir(), "objects")}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	file := hex.EncodeToString([]byte("offload-file-xyz"))
	out, err := exec(t, socket, "share", "--tag", "1")
	if err != nil {
		t.Fatalf("share: %v (out %q)", err, out)
	}
	out, err = exec(t, socket, "write-at", "--tag", "1", file, "0", "--str", "hello")
	if err != nil {
		t.Fatalf("write-at: %v (out %q)", err, out)
	}

	// On this base Offload and Repack are declared-but-not-built: the
	// command surfaces the journal's own refusal verbatim. The rebase
	// that lands the offload family flips this to an asserted offload:
	// payload files under OffloadDir, and a repack answering freed bytes.
	out, err = exec(t, socket, "offload", "--tag", "1", file, "999999")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("offload: got err %v (out %q), want the journal's not-implemented answer verbatim", err, out)
	}
	out, err = exec(t, socket, "repack", "1024")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("repack: got err %v (out %q), want the journal's not-implemented answer verbatim", err, out)
	}
}

func TestForgetDropsTheHandle(t *testing.T) {
	// Forget is refused while a handle of the tag is open, so the daemon
	// must drop its handle before calling it — otherwise the command could
	// never succeed once the journal builds Forget.
	socket := tmpSocket(t)
	srv := &Server{Options: testJournalOptions(), Dir: filepath.Join(t.TempDir(), "journal"), SocketPath: socket}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	if out, err := exec(t, socket, "share", "--tag", "1"); err != nil {
		t.Fatalf("share: %v (out %q)", err, out)
	}
	srv.mu.Lock()
	open := len(srv.handles)
	srv.mu.Unlock()
	if open != 1 {
		t.Fatalf("share left %d handles, want 1", open)
	}
	if out, err := exec(t, socket, "forget", "--tag", "1"); err == nil ||
		!strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("forget on this base: got err %v (out %q), want the stub refusal verbatim", err, out)
	}
	srv.mu.Lock()
	open = len(srv.handles)
	srv.mu.Unlock()
	if open != 0 {
		t.Errorf("forget left the tag's handle open (%d handles), want it closed first", open)
	}
}
