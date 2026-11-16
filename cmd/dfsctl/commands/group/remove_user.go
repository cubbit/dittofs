package group

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newRemoveUserCmd() *cobra.Command {
	removeUserCmd := &cobra.Command{
		Use:   "remove-user <group> <username>",
		Short: "Remove a user from a group",
		Long: `Remove a user from a group on the DittoFS server. The user loses any
permissions that were derived solely from that group membership. Both the
group name and the username are required positional arguments.

Examples:
  # Remove alice from the editors group
  dfsctl group remove-user editors alice

  # Remove a user from the admins group
  dfsctl group remove-user admins bob

  # Verify membership after removal
  dfsctl group get editors`,
		Args: cobra.ExactArgs(2), RunE: cmdutil.Unavailable,
	}
	return removeUserCmd
}
