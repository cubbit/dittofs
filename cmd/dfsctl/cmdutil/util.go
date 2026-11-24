// Package cmdutil provides shared utilities for dfsctl commands.
package cmdutil

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/internal/cli/prompt"
	"github.com/spf13/cobra"
)

// Unavailable is the body of every v1 command dfsctl still lists: it refuses,
// naming the command, and the command exits non-zero.
//
// decision: dfsctl keeps the v1 commands a v2 route will serve, each body
// refusing until it is rebuilt on that route; the commands no route serves are
// deleted. A command stops calling this when it is rebuilt; once none does,
// delete it.
func Unavailable(cmd *cobra.Command, _ []string) error {
	return fmt.Errorf("%s: not available until the CLI is rebuilt on the v2 API", cmd.CommandPath())
}

// outputFormat is the format the root's --output flag names for cmd.
func outputFormat(cmd *cobra.Command) (output.Format, error) {
	f, _ := cmd.Flags().GetString("output")
	return output.ParseFormat(f)
}

// PrintOutput prints data to cmd's output in the format --output names. For a
// table it prints emptyMsg when isEmpty, and tableRenderer otherwise.
func PrintOutput(cmd *cobra.Command, data any, isEmpty bool, emptyMsg string, tableRenderer output.TableRenderer) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	switch format {
	case output.FormatJSON:
		return output.PrintJSON(w, data)
	case output.FormatYAML:
		return output.PrintYAML(w, data)
	default:
		if isEmpty {
			_, _ = fmt.Fprintln(w, emptyMsg)
			return nil
		}
		return output.PrintTable(w, tableRenderer)
	}
}

// BoolToYesNo converts a boolean to "yes" or "no" string.
func BoolToYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// printSuccess prints msg to cmd's output when the output is a table.
func printSuccess(cmd *cobra.Command, msg string) {
	if format, err := outputFormat(cmd); err != nil || format != output.FormatTable {
		return
	}
	noColor, _ := cmd.Flags().GetBool("no-color")
	output.NewPrinter(cmd.OutOrStdout(), !noColor).Success(msg)
}

// RunDeleteWithConfirmation asks env's reader to confirm, unless force is set,
// and then runs deleteFn. The prompt goes to env.Err, so the command's output
// stays its own. Without a person at the reader it refuses, naming --force,
// rather than wait on a prompt nobody answers.
func RunDeleteWithConfirmation(cmd *cobra.Command, env Env, resourceType, name string, force bool, deleteFn func() error) error {
	if !force {
		if !env.Interactive {
			return fmt.Errorf("%s: removing %s '%s' needs confirmation; pass --force", cmd.CommandPath(), resourceType, name)
		}
		confirmed, err := prompt.Confirm(cmd.Context(), env.In, env.Err, fmt.Sprintf("Remove %s '%s'?", resourceType, name), false)
		if errors.Is(err, prompt.ErrAborted) {
			_, _ = fmt.Fprintln(env.Err, "\nAborted.")
			return nil
		}
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(env.Err, "Aborted.")
			return nil
		}
	}

	if err := deleteFn(); err != nil {
		return err
	}

	printSuccess(cmd, fmt.Sprintf("%s '%s' removed successfully", resourceType, name))
	return nil
}
