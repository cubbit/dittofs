package group

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all groups",
	Long: `List all groups registered on the DittoFS server. The table view
shows each group's name, GID, member count, and description. Use -o json or
-o yaml to get machine-readable output suitable for piping into other tools.

Examples:
  # List all groups as a table
  dfsctl group list

  # Output the full group list as JSON
  dfsctl group list -o json

  # Output as YAML
  dfsctl group list -o yaml`, RunE: cmdutil.Unavailable,
}
