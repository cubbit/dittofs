// Package permission implements share permission management commands.
package permission

import (
	"github.com/spf13/cobra"
)

// New returns the parent command for share permission management.
func New() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "permission",
		Short: "Manage share permissions",
		Long: `Manage permissions on shares.

Permission commands allow you to grant, revoke, and list permissions
for users and groups on shares.

Examples:
  # Grant read-write permission to a user
  dfsctl share permission grant /archive --user alice --level read-write

  # Grant read permission to a group
  dfsctl share permission grant /archive --group editors --level read

  # Revoke permission from a user
  dfsctl share permission revoke /archive --user alice

  # List permissions on a share
  dfsctl share permission list /archive`,
	}
	cmd.AddCommand(newGrantCmd())
	cmd.AddCommand(newRevokeCmd())
	cmd.AddCommand(newListCmd())
	return cmd
}
