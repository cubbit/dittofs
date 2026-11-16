package group

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var addUserCmd = &cobra.Command{
	Use:   "add-user <group> <username>",
	Short: "Add a user to a group",
	Long: `Add a user to a group on the DittoFS server. The user will immediately
gain any permissions associated with that group on shares and other resources.
Both the group name and the username are required positional arguments.

Examples:
  # Add alice to the editors group
  dfsctl group add-user editors alice

  # Add a user to the admins group
  dfsctl group add-user admins bob

  # Verify membership after adding
  dfsctl group get editors`,
	Args: cobra.ExactArgs(2), RunE: cmdutil.Unavailable,
}
