package prompt

import (
	"errors"

	"github.com/manifoldco/promptui"
)

// ErrAborted is returned when the user aborts a prompt (Ctrl+C).
var ErrAborted = errors.New("aborted")

// IsAborted returns true if the error indicates the user aborted (Ctrl+C).
func IsAborted(err error) bool {
	return errors.Is(err, promptui.ErrInterrupt) || errors.Is(err, promptui.ErrAbort) || errors.Is(err, ErrAborted)
}
