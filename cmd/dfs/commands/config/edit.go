package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/spf13/cobra"
)

func newEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open configuration in editor",
		Long: `Open the configuration file in your default editor.

The file is --config, else the one DITTOFS_CONFIG names, else the default.
Uses the EDITOR environment variable, then VISUAL, falling back to 'vi'. The
file is not checked: run 'dfs config validate' after editing it.

Examples:
  # Edit default config
  dfs config edit

  # Edit specific config file
  dfs config edit --config /etc/dittofs/config.yaml`,
		RunE: runConfigEdit,
	}
}

func runConfigEdit(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")

	return runEdit(configPath, os.Environ(), os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// runEdit opens the file a start would read in the editor environ names,
// with the editor's input and output on in, out and errOut.
func runEdit(configPath string, environ []string, in io.Reader, out, errOut io.Writer) error {
	configPath = bootstrap.ConfigPath(configPath, environ)

	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("configuration file not found: %s", configPath)
	}

	editor := lookupEnv(environ, "EDITOR")
	if editor == "" {
		editor = lookupEnv(environ, "VISUAL")
	}

	if editor == "" {
		editor = "vi"
	}

	editorCmd := exec.Command(editor, configPath)
	editorCmd.Stdin = in
	editorCmd.Stdout = out
	editorCmd.Stderr = errOut

	if err := editorCmd.Run(); err != nil {
		return fmt.Errorf("failed to run editor: %w", err)
	}

	return nil
}

// lookupEnv is the value of name in environ, the first entry winning as
// os.Getenv reads the process environment; "" when it is not set.
func lookupEnv(environ []string, name string) string {
	for _, kv := range environ {
		if n, v, ok := strings.Cut(kv, "="); ok && n == name {
			return v
		}
	}

	return ""
}
