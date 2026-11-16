package user

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newEditCmd() *cobra.Command {
	editCmd := &cobra.Command{
		Use:   "edit <username>",
		Short: "Edit a user",
		Long: `Edit an existing user on the DittoFS server. When run without flags, an
interactive prompt walks you through each editable field, showing the current
value so you can press Enter to keep it unchanged. When flags are provided,
only the specified fields are updated and no prompt appears.

Examples:
  # Edit all fields interactively (shows current values)
  dfsctl user edit alice

  # Move alice to the admin role
  dfsctl user edit alice --role admin

  # Update alice's primary GID to match a new group
  dfsctl user edit alice --gid 1002

  # Disable an account and change its group membership in one command
  dfsctl user edit alice --enabled false --groups viewers

  # Update display name and email
  dfsctl user edit alice --display-name "Alice Smith" --email alice@newdomain.com`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	editCmd.Flags().String("email", "", "Email address")
	editCmd.Flags().String("display-name", "", "Display name")
	editCmd.Flags().String("role", "", "Role (user|admin)")
	editCmd.Flags().Uint32("uid", 0, "Unix user ID")
	editCmd.Flags().Uint32("gid", 0, "Unix primary group ID")
	editCmd.Flags().String("groups", "", "Comma-separated list of groups")
	editCmd.Flags().String("enabled", "", "Enable/disable account (true|false)")
	return editCmd
}
