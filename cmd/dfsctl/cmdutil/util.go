// Package cmdutil provides shared utilities for dfsctl commands.
package cmdutil

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/internal/cli/prompt"
	"github.com/spf13/cobra"
)

// ConfirmInput is the source ConfirmDestructive reads its Y/N answer from.
// Tests override this with a strings.Reader to inject answers.
var ConfirmInput io.Reader = os.Stdin

// ConfirmOutput is the writer ConfirmDestructive prints the prompt to.
// Tests override this with a bytes.Buffer to capture output.
var ConfirmOutput io.Writer = os.Stdout

// ConfirmDestructive prints prompt to ConfirmOutput and reads a Y/N
// answer from ConfirmInput. If yes is true, the prompt is skipped and
// (true, nil) is returned immediately. Empty input or any non-y/yes
// answer returns false (the prompt is biased toward refusing).
func ConfirmDestructive(prompt string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	_, _ = fmt.Fprintln(ConfirmOutput, prompt)
	_, _ = fmt.Fprint(ConfirmOutput, "Type 'y' to confirm: ")

	reader := bufio.NewReader(ConfirmInput)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// Flags stores global flag values accessible by subcommands.
var Flags = &GlobalFlags{}

// GlobalFlags holds the global flag values.
type GlobalFlags struct {
	ServerURL string
	Token     string
	Output    string
	NoColor   bool
	Verbose   bool

	// TLS escape-hatch overrides. When set they take precedence over the TLS
	// material stored in the active login context. TLSSkipVerifySet records
	// whether --tls-skip-verify was explicitly passed, so the stored value is
	// only overridden on an explicit flag (a bare false flag would otherwise
	// silently re-enable verification).
	CACert           string
	ClientCert       string
	ClientKey        string
	TLSSkipVerify    bool
	TLSSkipVerifySet bool
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

// IsVerbose returns whether verbose output is enabled.
func IsVerbose() bool {
	return Flags.Verbose
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

// PrintSuccess prints a success message if the output format is table.
func PrintSuccess(msg string) {
	format, err := GetOutputFormatParsed()
	if err != nil || format != output.FormatTable {
		return
	}
	printer := output.NewPrinter(os.Stdout, !IsColorDisabled())
	printer.Success(msg)
}

// PrintSuccessWithInfo prints a success message followed by additional info lines.
// The info lines are only printed in table format.
func PrintSuccessWithInfo(msg string, infoLines ...string) {
	format, err := GetOutputFormatParsed()
	if err != nil || format != output.FormatTable {
		return
	}
	printer := output.NewPrinter(os.Stdout, !IsColorDisabled())
	printer.Success(msg)
	for _, line := range infoLines {
		fmt.Println(line)
	}
}

// PrintResourceWithSuccess prints a resource in the specified format.
// For table format, it displays a success message. For JSON/YAML, it outputs the resource.
// This is useful for create, update, and similar operations.
func PrintResourceWithSuccess(w io.Writer, data any, successMsg string) error {
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
		PrintSuccess(successMsg)
		return nil
	}
}

// PrintResource prints a resource in the specified format.
// For table format, it uses the provided tableRenderer. For JSON/YAML, it outputs the resource.
func PrintResource(w io.Writer, data any, tableRenderer output.TableRenderer) error {
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
		return output.PrintTable(w, tableRenderer)
	}
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

// ParseCommaSeparatedList parses a comma-separated string into a slice of trimmed strings.
func ParseCommaSeparatedList(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

// BoolToYesNo converts a boolean to "yes" or "no" string.
func BoolToYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// EmptyOr returns the value if not empty, otherwise returns the fallback.
// Useful for table display where empty fields should show "-".
func EmptyOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// GetConfigString extracts a string value from a config map with a default fallback.
func GetConfigString(config map[string]any, key, defaultValue string) string {
	if config == nil {
		return defaultValue
	}
	if v, ok := config[key].(string); ok {
		return v
	}
	return defaultValue
}

// HandleAbort checks if error is an abort (Ctrl+C) and prints a message.
// Returns nil for abort (user cancelled), otherwise returns the original error.
func HandleAbort(err error) error {
	if prompt.IsAborted(err) {
		fmt.Println("\nAborted.")
		return nil
	}
	return err
}
