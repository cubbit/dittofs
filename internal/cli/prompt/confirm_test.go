package prompt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseConfirm(t *testing.T) {
	for _, tt := range []struct {
		line       string
		defaultYes bool
		want       bool
	}{
		{"y\n", false, true},
		{"YES\r\n", false, true},
		{" yes ", false, true},
		{"n\n", true, false},
		{"no\n", true, false},
		{"maybe\n", true, false},
		{"\n", false, false},
		{"\n", true, true},
	} {
		if got := parseConfirm(tt.line, tt.defaultYes); got != tt.want {
			t.Errorf("parseConfirm(%q, defaultYes=%v): got %v, want %v", tt.line, tt.defaultYes, got, tt.want)
		}
	}
}

// Confirm prompts on out, never on the process's stdout, and a reader that
// ends with no answer aborts rather than taking the default.
func TestConfirm(t *testing.T) {
	var out bytes.Buffer
	got, err := Confirm(strings.NewReader("y"), &out, "Remove it?", false)
	if err != nil || !got {
		t.Errorf("Confirm(\"y\" then EOF): got %v, %v, want true, nil", got, err)
	}
	if out.String() != "Remove it? [y/N] " {
		t.Errorf("prompt: got %q, want %q", out.String(), "Remove it? [y/N] ")
	}

	if _, err := Confirm(strings.NewReader(""), &out, "Remove it?", true); !errors.Is(err, ErrAborted) {
		t.Errorf("Confirm(empty reader): got %v, want ErrAborted", err)
	}
}
