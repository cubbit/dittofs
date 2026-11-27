package prompt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
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
	got, err := Confirm(t.Context(), strings.NewReader("y"), &out, "Remove it?", false)
	if err != nil || !got {
		t.Errorf("Confirm(\"y\" then EOF): got %v, %v, want true, nil", got, err)
	}
	if out.String() != "Remove it? [y/N] " {
		t.Errorf("prompt: got %q, want %q", out.String(), "Remove it? [y/N] ")
	}

	if _, err := Confirm(t.Context(), strings.NewReader(""), &out, "Remove it?", true); !errors.Is(err, ErrAborted) {
		t.Errorf("Confirm(empty reader): got %v, want ErrAborted", err)
	}
}

// A confirmation whose context ends returns the context's cause, though
// nobody answered and the reader never ends.
func TestConfirmEndsWithItsContext(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	ctx, cancel := context.WithCancelCause(t.Context())
	interrupted := errors.New("interrupted")
	done := make(chan error, 1)
	go func() {
		_, err := Confirm(ctx, pr, io.Discard, "Remove it?", false)
		done <- err
	}()
	cancel(interrupted)
	// The bound is the assertion: a Confirm that ignores its context never
	// returns, and nothing else ends the read.
	select {
	case err := <-done:
		if !errors.Is(err, interrupted) {
			t.Errorf("Confirm after its context ended: %v, want the context's cause", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Confirm still waiting 10 s after its context ended, want it returned")
	}
}
