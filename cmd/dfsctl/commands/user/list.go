package user

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all users",
		Long: `List all local users registered on the DittoFS server. The table view
shows each user's username, UID, role, email, group memberships, and whether
the account is enabled. Use -o json or -o yaml to get machine-readable output
suitable for piping into other tools.

Examples:
  # List all users as a table
  dfsctl user list

  # Output the full user list as JSON
  dfsctl user list -o json

  # Output as YAML
  dfsctl user list -o yaml`, RunE: cmdutil.Unavailable,
	}
	return listCmd
}
