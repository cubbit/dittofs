package main

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

func TestHoldParse(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"hold no cut", []string{"hold", "--tag", "1",
			"--share", "0102030405060708090a0b0c0d0e0f10"}, "--cut"},
		{"hold bad mark", []string{"hold", "--tag", "1",
			"--share", "0102030405060708090a0b0c0d0e0f10", "--cut", "7",
			"--mark", "0102030405060708090a0b0c0d0e0f10"}, "mark"},
		{"held no share", []string{"held", "--tag", "1"}, "--share"},
		{"unmark-share bad share", []string{"unhold", "--tag", "1",
			"--share", "zz", "--cut", "7"}, "zz"},
		{"stamp missing cut", []string{"stamp", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "3"}, "--cut"},
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

func TestSinceLine(t *testing.T) {
	cases := []struct {
		name string
		ch   journal.Change
		want string
	}{
		{"write", journal.Change{
			Kind: journal.ChangeWrite, Extent: journal.Extent{Off: 0, Len: 5},
			Version: journal.Version{Counter: 1}, Mtime: 1700000000000000000,
		}, "write off=0 len=5 v1 mtime=1700000000000000000"},
		{"truncate", journal.Change{
			Kind: journal.ChangeTruncate, Extent: journal.Extent{Off: 3, Len: 2},
			Version: journal.Version{Counter: 2},
		}, "truncate off=3 len=2 v2"},
		{"loss", journal.Change{
			Kind:    journal.ChangeLoss,
			Extent:  journal.Extent{Off: 8, Len: 1},
			Version: journal.Version{Counter: 9},
		}, "loss off=8 len=1 v9"},
	}
	for _, c := range cases {
		if got := changeLine(c.ch); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSinceSettleAndQueriesRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := tmpSocket(t)
	srv := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: socket}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	var raw [16]byte
	copy(raw[:], "change-file-0123")
	file := hex.EncodeToString(raw[:])

	out, err := exec(t, socket, "share", "--tag", "1")
	if err != nil {
		t.Fatalf("share: %v (out %q)", err, out)
	}
	out, err = exec(t, socket, "write-at", "--tag", "1", file, "0", "--str", "hello")
	if err != nil {
		t.Fatalf("write-at: %v (out %q)", err, out)
	}

	// Since over everything: the held write, v1.
	out, err = exec(t, socket, "since", "--tag", "1", file, "0")
	if err != nil {
		t.Fatalf("since 0: %v (out %q)", err, out)
	}
	if !strings.Contains(out, "write off=0 len=5 v1") {
		t.Errorf("since 0: got %q, want it to report the held write at v1", out)
	}

	// Settle the write's marker-free version, then since from it: strictly
	// above v1 holds nothing.
	out, err = exec(t, socket, "settle", "--tag", "1", file, "1")
	if err != nil {
		t.Fatalf("settle: %v (out %q)", err, out)
	}
	out, err = exec(t, socket, "since", "--tag", "1", file, "1")
	if err != nil {
		t.Fatalf("since 1: %v (out %q)", err, out)
	}
	if strings.Contains(out, "off=") {
		t.Errorf("since from v1: got %q, want no changes (the write is at v1)", out)
	}

	out, err = exec(t, socket, "extents", "--tag", "1", file)
	if err != nil {
		t.Fatalf("extents: %v (out %q)", err, out)
	}
	if !strings.Contains(out, "0") || !strings.Contains(out, "5") {
		t.Errorf("extents: got %q, want the held 0–5", out)
	}

	out, err = exec(t, socket, "files", "--tag", "1")
	if err != nil {
		t.Fatalf("files: %v (out %q)", err, out)
	}
	if !strings.Contains(out, file) {
		t.Errorf("files: got %q, want %q named", out, file)
	}

	out, err = exec(t, socket, "dirty-files", "--tag", "1")
	if err != nil {
		t.Fatalf("dirty-files: %v (out %q)", err, out)
	}
	if !strings.Contains(out, file) {
		t.Errorf("dirty-files: got %q, want %q named (the write holds no offloaded bit)", out, file)
	}

	// The hold family is declared-but-not-built on this base: each command
	// names the journal's own answer verbatim. The rebase that lands
	// holds flips these to succeeded holds.
	out, err = exec(t, socket, "hold", "--tag", "1",
		"--share", "0102030405060708090a0b0c0d0e0f10", "--cut", "7")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("hold: got err %v (out %q), want the journal's not-implemented verbatim", err, out)
	}
	out, err = exec(t, socket, "stamp", "--tag", "1", file, "1", "--cut", "7")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("stamp: got err %v (out %q), want the journal's not-implemented verbatim", err, out)
	}
}
