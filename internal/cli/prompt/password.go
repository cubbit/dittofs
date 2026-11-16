package prompt

import (
	"errors"
	"fmt"
	"io"

	"github.com/manifoldco/promptui"
)

// ErrPasswordMismatch indicates passwords don't match.
var ErrPasswordMismatch = errors.New("passwords do not match")

// nopWriteCloser gives promptui the io.WriteCloser it wants without letting it
// close the caller's writer.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// wrapError converts promptui's interrupt and abort errors to ErrAborted.
func wrapError(err error) error {
	if errors.Is(err, promptui.ErrInterrupt) || errors.Is(err, promptui.ErrAbort) {
		return ErrAborted
	}
	return err
}

// Password prompts on out for a password read from in, masking the input.
func Password(in io.Reader, out io.Writer, label string) (string, error) {
	prompt := promptui.Prompt{
		Stdin:  io.NopCloser(in),
		Stdout: nopWriteCloser{out},
		Label:  label,
		Mask:   '*',
	}

	result, err := prompt.Run()
	return result, wrapError(err)
}

// PasswordWithValidation prompts for a password with minimum length validation.
func PasswordWithValidation(in io.Reader, out io.Writer, label string, minLength int) (string, error) {
	prompt := promptui.Prompt{
		Stdin:  io.NopCloser(in),
		Stdout: nopWriteCloser{out},
		Label:  label,
		Mask:   '*',
		Validate: func(input string) error {
			if len(input) < minLength {
				return fmt.Errorf("password must be at least %d characters", minLength)
			}
			return nil
		},
	}

	result, err := prompt.Run()
	return result, wrapError(err)
}

// PasswordWithConfirmation prompts for a password and confirmation.
// Returns error if passwords don't match.
func PasswordWithConfirmation(in io.Reader, out io.Writer, label, confirmLabel string, minLength int) (string, error) {
	password, err := PasswordWithValidation(in, out, label, minLength)
	if err != nil {
		return "", err
	}

	confirm, err := Password(in, out, confirmLabel)
	if err != nil {
		return "", err
	}

	if password != confirm {
		return "", ErrPasswordMismatch
	}

	return password, nil
}

// NewPassword prompts for a new password with confirmation.
// Uses default labels and minimum length of 8.
func NewPassword(in io.Reader, out io.Writer) (string, error) {
	return PasswordWithConfirmation(in, out, "Password", "Confirm password", 8)
}
