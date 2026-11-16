package commands

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new installation",
	Long: `Initialize a new DittoFS installation: its first administrator and its
signing key, kept in the metadata store the server's configuration names.

Examples:
  # Initialize the installation the default configuration names
  dfs init`,
	Args: cobra.NoArgs,
	RunE: func(*cobra.Command, []string) error {
		return fmt.Errorf("initializing the installation: %w", errors.ErrUnsupported)
	},
}
