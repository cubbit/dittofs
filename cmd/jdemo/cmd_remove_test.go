package main

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
)

func TestCloneTargetParse(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing src", []string{"clone-target", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "--dst-off", "0", "--len", "9"}, "--src"},
		{"missing src-share", []string{"clone-target", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "--src", "0102030405060708090a0b0c0d0e0f10",
			"--dst-off", "0", "--len", "9"}, "--src-share"},
		{"bad asof", []string{"clone-target", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "--src", "0102030405060708090a0b0c0d0e0f10",
			"--src-share", "0102030405060708090a0b0c0d0e0f10",
			"--dst-off", "0", "--len", "9", "--asof", "x"}, "asof"},
		{"negative len", []string{"clone-target", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "--src", "0102030405060708090a0b0c0d0e0f10",
			"--src-share", "0102030405060708090a0b0c0d0e0f10",
			"--dst-off", "0", "--len", "-9", "--asof", "1"}, "len"},
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

func TestReserveParse(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing writes", []string{"reserve", "--tag", "1", "64"}, "accepts 2"},
		// pflag reads a leading '-' as a shorthand: cobra refuses it naming it.
		{"negative writes", []string{"reserve", "--tag", "1", "64", "-1"}, "in -1"},
		{"reserve-write no rid", []string{"reserve-write", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "0", "--str", "x"}, "rid"},
		{"fill no asof", []string{"fill", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "0", "--str", "x", "--ver", "1"}, "--asof"},
		{"bad len", []string{"deallocate", "--tag", "1",
			"0102030405060708090a0b0c0d0e0f10", "0", "0"}, `length "0"`},
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

func TestRemovalsAndReservationsRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "journal")
	socket := tmpSocket(t)
	srv := &Server{Dir: dir, SocketPath: socket}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown(context.Background())

	var raw [16]byte
	copy(raw[:], "removal-file-tes")
	file := hex.EncodeToString(raw[:])

	must := func(what string, out string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v (out %q)", what, err, out)
		}
	}

	var out string
	var err error
	out, err = exec(t, socket, "share", "--tag", "1")
	must("share", out, err)
	out, err = exec(t, socket, "write-at", "--tag", "1", file, "0", "--str", "hello")
	must("write-at", out, err)

	out, err = exec(t, socket, "truncate", "--tag", "1", file, "3")
	must("truncate", out, err)
	if !strings.Contains(out, "v") {
		t.Errorf("truncate output: got %q, want the removal's version", out)
	}

	out, err = exec(t, socket, "read-at", "--tag", "1", file, "0", "5")
	if err != nil {
		t.Fatalf("read-at after truncate: %v (out %q)", err, out)
	}
	if !strings.Contains(out, "n=3") {
		t.Errorf("read-at after truncate: got %q, want n=3 (the extent is narrowed)", out)
	}
	if !strings.Contains(out, "missing 3–5") {
		t.Errorf("read-at after truncate: got %q, want the missing 3–5 reported", out)
	}

	// A reservation takes its bytes up front, a reserved write spends them,
	// and the close returns the rest.
	out, err = exec(t, socket, "reserve", "--tag", "1", "4096", "2")
	must("reserve", out, err)
	if !strings.Contains(out, "rid 1") {
		t.Errorf("reserve output: got %q, want rid 1", out)
	}
	out, err = exec(t, socket, "reserve-write", "--tag", "1", "--rid", "1", file, "512", "--str", "AB")
	must("reserve-write", out, err)
	out, err = exec(t, socket, "read-at", "--tag", "1", file, "512", "2")
	must("read-at reserved", out, err)
	if !strings.Contains(out, "41 42") {
		t.Errorf("read-at of reserved write: got %q, want the hexdump of %q", out, "AB")
	}
	out, err = exec(t, socket, "reserve-close", "--rid", "1")
	must("reserve-close", out, err)
}
