// Package prompt provides interactive terminal prompts for CLI commands.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrAborted is returned when the reader ends before the user answers.
var ErrAborted = errors.New("aborted")

// Confirm writes label to out and reads one line from in as a yes/no answer.
// An empty answer takes defaultYes; a reader that ends before any input is
// ErrAborted.
func Confirm(in io.Reader, out io.Writer, label string, defaultYes bool) (bool, error) {
	hint := "y/N"
	if defaultYes {
		hint = "Y/n"
	}
	if _, err := fmt.Fprintf(out, "%s [%s] ", label, hint); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return false, err
		}
		if line == "" {
			return false, ErrAborted
		}
	}
	return parseConfirm(line, defaultYes), nil
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
