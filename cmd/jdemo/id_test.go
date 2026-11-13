package main

import (
	"strings"
	"testing"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

func TestParseFileID(t *testing.T) {
	id := journal.NewFileID([16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	cases := []struct {
		in   string
		want journal.FileID
		err  string
	}{
		{"0102030405060708090a0b0c0d0e0f10", id, ""},
		{"0102030405060708090a0b0c0d0e0f1", journal.FileID{}, "not 32 hex"},
		{"", journal.FileID{}, "not 32 hex"},
		{"zz02030405060708090a0b0c0d0e0f100", journal.FileID{}, "hex"},
	}
	for _, c := range cases {
		got, err := parseFileID(c.in)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.in) {
				t.Errorf("parseFileID(%q): got err %v, want an error naming the input", c.in, err)
			}
		case err != nil:
			t.Errorf("parseFileID(%q): unexpected err %v", c.in, err)
		case got != c.want:
			t.Errorf("parseFileID(%q): got %x, want %x", c.in, got, c.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want journal.Version
		err  string
	}{
		{"0", journal.Version{}, ""},
		{"42", journal.Version{Counter: 42}, ""},
		// What fmtVersion prints, fmtVersion's reader takes back: the
		// presenter retypes a version the pane showed.
		{"v42", journal.Version{Counter: 42}, ""},
		{"v0", journal.Version{}, ""},
		{"7/v42", journal.Version{Epoch: 7, Counter: 42}, ""},
		{"0/42", journal.Version{Counter: 42}, ""},
		{"7/42", journal.Version{Epoch: 7, Counter: 42}, ""},
		{"", journal.Version{}, "empty"},
		{"x", journal.Version{}, "not a version"},
		{"-1", journal.Version{}, "not a version"},
		{"7/", journal.Version{}, "not a version"},
	}
	for _, c := range cases {
		got, err := parseVersion(c.in)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.in) {
				t.Errorf("parseVersion(%q): got err %v, want an error naming the input", c.in, err)
			}
		case err != nil:
			t.Errorf("parseVersion(%q): unexpected err %v", c.in, err)
		case got != c.want:
			t.Errorf("parseVersion(%q): got %v, want %v", c.in, got, c.want)
		}
	}
}
