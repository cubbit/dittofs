// Package prompt provides interactive terminal prompts for CLI commands.
package prompt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrAborted is returned when the reader ends before the user answers.
var ErrAborted = errors.New("aborted")

// Confirm writes label to out and reads one line from in as a yes/no answer.
// An empty answer takes defaultYes; a reader that ends before any input is
// ErrAborted. When ctx ends first, as on an interrupt, it returns ctx's
// error, leaving the read to end with the process.
func Confirm(ctx context.Context, in io.Reader, out io.Writer, label string, defaultYes bool) (bool, error) {
	hint := "y/N"
	if defaultYes {
		hint = "Y/n"
	}
	if _, err := fmt.Fprintf(out, "%s [%s] ", label, hint); err != nil {
		return false, err
	}
	type read struct {
		line string
		err  error
	}
	done := make(chan read, 1)
	go func() {
		line, err := bufio.NewReader(in).ReadString('\n')
		done <- read{line, err}
	}()
	var r read
	select {
	case r = <-done:
	case <-ctx.Done():
		return false, context.Cause(ctx)
	}
	if r.err != nil {
		if !errors.Is(r.err, io.EOF) {
			return false, r.err
		}
		if r.line == "" {
			return false, ErrAborted
		}
	}
	return parseConfirm(r.line, defaultYes), nil
}

// parseConfirm reads a yes/no answer: y or yes is yes, an empty answer is
// defaultYes, and anything else is no.
func parseConfirm(line string, defaultYes bool) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return defaultYes
	case "y", "yes":
		return true
	default:
		return false
	}
}
