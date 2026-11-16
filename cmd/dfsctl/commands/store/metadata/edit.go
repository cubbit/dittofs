package metadata

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	editConfig string
	editDBPath string
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a metadata store",
	Long: `Edit an existing metadata store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit interactively (default)
  dfsctl store metadata edit default

  # Update config with JSON
  dfsctl store metadata edit default --config '{"path":"/new/path"}'

  # Update BadgerDB path
  dfsctl store metadata edit default --db-path /new/path`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	editCmd.Flags().StringVar(&editConfig, "config", "", "Store configuration as JSON")
	editCmd.Flags().StringVar(&editDBPath, "db-path", "", "BadgerDB directory path")
}
