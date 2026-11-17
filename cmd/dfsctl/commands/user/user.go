// Package user implements user management commands for dfsctl.
package user

import (
	"github.com/spf13/cobra"
)

// New returns the parent command for user management.
func New() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "User management",
		Long: `Manage local user accounts on the DittoFS server. Local users are
distinct from identities resolved via Kerberos or LDAP — they are accounts
stored in the DittoFS control plane and used for direct authentication.
Most subcommands require admin privileges; "change-password" operates on the
currently authenticated account and is available to all users.

Examples:
  # List all registered users
  dfsctl user list

  # Create a new user interactively
  dfsctl user create

  # Create a user with an explicit UID for NFS access
  dfsctl user create --username alice --password secret --uid 1000 --role user

  # Edit a user's group membership
  dfsctl user edit alice --groups editors,viewers

  # Reset a user's password as an admin
  dfsctl user password alice

  # Remove a user (prompts for confirmation)
  dfsctl user remove alice`,
	}
	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newCreateCmd())
	cmd.AddCommand(newRemoveCmd())
	cmd.AddCommand(newGetCmd())
	cmd.AddCommand(newEditCmd())
	cmd.AddCommand(newPasswordCmd())
	cmd.AddCommand(newChangePasswordCmd())
	return cmd
}
