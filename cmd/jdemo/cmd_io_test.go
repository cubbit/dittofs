package main

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

// exec runs the jdemo CLI with args against socket, returning the command's
// output and error.
func exec(t *testing.T, socket string, args ...string) (string, error) {
	t.Helper()
	root := newRoot()
	var buf strings.Builder
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"--socket", socket}, args...))
	err := root.Execute()
	return buf.String(), err
}

func TestWriteAtParse(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string // a fragment of the expected error
	}{
		{"no tag", []string{"write-at", "file", "0", "--str", "hi"}, "tag"},
		{"bad file id", []string{"write-at", "--tag", "1", "nothex", "0", "--str", "hi"}, "nothex"},
		{"bad offset", []string{"write-at", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "x", "--str", "hi"}, "x"},
		{"negative offset", []string{"write-at", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "-9", "--str", "hi"}, "-9"},
		{"two data sources", []string{"write-at", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "0", "--str", "hi", "--hex", "00"}, "one of"},
		{"no data source", []string{"write-at", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "0"}, "one of"},
		{"missing offset arg", []string{"write-at", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10"}, "accepts 2"},
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

func TestReadAtParse(t *testing.T) {
	id := hex.EncodeToString([]byte("read-at-len-test"))
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"zero len", []string{"read-at", "--tag", "1", id, "0", "0"}, `length "0"`},
		// pflag takes a leading '-' as a flag: cobra refuses it before the
		// RunE sees it, naming it all the same.
		{"negative len", []string{"read-at", "--tag", "1", id, "0", "-9"}, "in -9"},
		{"bad id", []string{"read-at", "--tag", "1", "zz", "0", "9"}, "zz"},
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

func TestWriteReadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := tmpSocket(t)
	srv := &Server{Options: testJournalOptions(), Dir: dir, SocketPath: socket}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	var rawID [16]byte
	copy(rawID[:], "round-trip-file")
	file := hex.EncodeToString(rawID[:])

	if out, err := exec(t, socket, "share", "--tag", "1"); err != nil {
		t.Fatalf("share: %v (out %q)", err, out)
	}
	wout, err := exec(t, socket, "write-at", "--tag", "1", file, "0", "--str", "hello")
	if err != nil {
		t.Fatalf("write-at: %v (out %q)", err, wout)
	}
	// The first write of the journal's first window issues v1.
	if !strings.Contains(wout, "wrote 5 bytes") || !strings.Contains(wout, "v1") {
		t.Errorf("write-at output: got %q, want it to name the byte count and the issued version v1", wout)
	}

	rout, err := exec(t, socket, "read-at", "--tag", "1", file, "0", "5")
	if err != nil {
		t.Fatalf("read-at: %v (out %q)", err, rout)
	}
	if !strings.Contains(rout, "68 65 6c 6c 6f") {
		t.Errorf("read-at output: got %q, want a hexdump of %q", rout, "hello")
	}
}

func TestUnboundedSizesRefused(t *testing.T) {
	socket := tmpSocket(t)
	srv := &Server{Options: testJournalOptions(), Dir: filepath.Join(t.TempDir(), "journal"), SocketPath: socket}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	if out, err := exec(t, socket, "share", "--tag", "1"); err != nil {
		t.Fatalf("share: %v (out %q)", err, out)
	}
	var capRaw [16]byte
	copy(capRaw[:], "cap-check-file")
	file := hex.EncodeToString(capRaw[:])

	// The daemon refuses a read whose allocation is a typo away from a
	// fatal out-of-memory: the cap is 1 GiB.
	var resp struct {
		Error string `json:"error"`
	}
	c := Dial(socket)
	derr := c.Do(context.Background(), "POST", "/read-at", map[string]any{
		"tag": 1, "file": file, "off": int64(0), "len": int64(1) << 40, "full": true,
	}, &resp)
	if derr == nil || !strings.Contains(derr.Error(), "exceeds") {
		t.Errorf("read-at 1 TiB: got err %v, want a refusal naming the cap", derr)
	}

	// And CLI-side, so the daemon never meets the typo at all.
	_, err := exec(t, socket, "read-at", "--tag", "1", file, "0", "1099511627776")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("cli read-at 1 TiB: got err %v, want a client-side refusal before any call", err)
	}
	_, err = exec(t, socket, "write-at", "--tag", "1", file, "0", "--size", "1099511627776")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("cli write-at --size 1 TiB: got err %v, want a client-side refusal before any call", err)
	}
}
