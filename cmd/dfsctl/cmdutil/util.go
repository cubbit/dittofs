// Package cmdutil provides shared utilities for dfsctl commands.
package cmdutil

import (
	"fmt"
	"io"
	"os"

	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/internal/cli/prompt"
	"github.com/spf13/cobra"
)

// Flags stores global flag values accessible by subcommands.
var Flags = &GlobalFlags{}

// GlobalFlags holds the global flag values a command reads. The root's other
// persistent flags stay registered, so every invocation of them still parses,
// and return to the struct with the commands that read them.
type GlobalFlags struct {
	Output  string
	NoColor bool
}

// Unavailable is the body of every v1 command dfsctl still lists: it refuses,
// naming the command, and the command exits non-zero.
//
// decision: dfsctl keeps v1's command tree, its names, arguments and flags,
// after the server those commands spoke to is gone, with each body refusing.
// Deleting the commands would leave every invocation of them in the docs and
// in Go strings naming a command that does not exist, and the CLI's rebuild
// on the v2 API rewrites those invocations anyway, command by command. A
// command stops calling this when it is rebuilt; once none does, delete it.
func Unavailable(cmd *cobra.Command, _ []string) error {
	return fmt.Errorf("%s: not available until the CLI is rebuilt on the v2 API", cmd.CommandPath())
}

// GetOutputFormatParsed returns the parsed output format.
func GetOutputFormatParsed() (output.Format, error) {
	return output.ParseFormat(Flags.Output)
}

// IsColorDisabled returns whether color output is disabled.
func IsColorDisabled() bool {
	return Flags.NoColor
}

// PrintOutput prints data in the specified format (JSON, YAML, or table).
// For table format, it displays emptyMsg if data is empty, otherwise uses the tableRenderer.
func PrintOutput(w io.Writer, data any, isEmpty bool, emptyMsg string, tableRenderer output.TableRenderer) error {
	format, err := GetOutputFormatParsed()
	if err != nil {
		return err
	}

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

// PrintSuccess prints a success message if the output format is table.
func PrintSuccess(msg string) {
	format, err := GetOutputFormatParsed()
	if err != nil || format != output.FormatTable {
		return
	}
	printer := output.NewPrinter(os.Stdout, !IsColorDisabled())
	printer.Success(msg)
}

// RunDeleteWithConfirmation prompts for confirmation (unless force is true) and runs deleteFn.
func RunDeleteWithConfirmation(resourceType, name string, force bool, deleteFn func() error) error {
	confirmed, err := prompt.ConfirmWithForce(fmt.Sprintf("Remove %s '%s'?", resourceType, name), force)
	if err != nil {
		if prompt.IsAborted(err) {
			fmt.Println("\nAborted.")
			return nil
		}
		return err
	}
	if !confirmed {
		fmt.Println("Aborted.")
		return nil
	}

	if err := deleteFn(); err != nil {
		return err
	}

	PrintSuccess(fmt.Sprintf("%s '%s' removed successfully", resourceType, name))
	return nil
}
