package group

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var getCmd = &cobra.Command{
	Use:   "get <name|id>",
	Short: "Get group details",
	Long: `Get detailed information about a specific group on the DittoFS server.
Accepts either the group name or the group's full ID (see 'dfsctl group list').
The output includes the group's GID, description, member list, and creation
timestamp. Use -o json or -o yaml for machine-readable output.

Examples:
  # Show group details as a table
  dfsctl group get editors

  # By ID
  dfsctl group get 7a1e9f02-...

  # Output as JSON (useful for scripting)
  dfsctl group get editors -o json

  # Output as YAML
  dfsctl group get editors -o yaml`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
