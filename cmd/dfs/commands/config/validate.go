package config

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/cubbit/dittofs/pkg/bootstrap"
	"github.com/spf13/cobra"
)

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration file",
		Long: `Validate the DittoFS configuration file and environment.

Runs the load a start runs, then the checks of the host: both roles, the
journals, the two wrapping keys (each key file is opened with
DITTOFS_ENCRYPTION_PASSPHRASE), the API's certificate off loopback, the socket
path and the state and runtime directories. It refuses what they refuse, one
line per refusal, and starts nothing: no directory is created.

The file is --config, else the one DITTOFS_CONFIG names, else the default.

Examples:
  # Validate default config
  dfs config validate

  # Validate specific config file
  dfs config validate --config /etc/dittofs/config.yaml`,
		RunE: runConfigValidate,
	}
}

func runConfigValidate(cmd *cobra.Command, _ []string) error {
	configPath, _ := cmd.Flags().GetString("config")

	return runValidate(cmd.Context(), configPath, os.Environ(), cmd.OutOrStdout())
}

// runValidate makes the two calls a start will make on its file and environ,
// Load then Validate, and returns the first refusal unchanged: a start that
// fails to load never reaches the second. It never makes the runtime
// directory, which only a start may create.
//
// decision: validate runs Validate, which dfs start does not call yet, so
// for the host's checks it refuses a host a start would still run on. A
// check that refuses only here is still a host the node cannot serve from.
func runValidate(ctx context.Context, configPath string, environ []string, out io.Writer) error {
	loaded, err := bootstrap.LoadWithSources(configPath, environ)
	if err != nil {
		return err
	}

	if err := loaded.Validate(ctx, environ); err != nil {
		return err
	}

	file := loaded.File
	if file == "" {
		file = "none"
	}

	_, _ = fmt.Fprintf(out, "Configuration file: %s\nValidation: OK\n", file)

	return nil
}
