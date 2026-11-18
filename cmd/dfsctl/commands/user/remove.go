package user

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newRemoveCmd() *cobra.Command {
	removeCmd := &cobra.Command{
		Use:   "remove <username|id>",
		Short: "Remove a user",
		Long: `Remove a user from the DittoFS server. Accepts either the username or
the user's full ID (see 'dfsctl user list'). This action is irreversible:
the account and its authentication tokens are permanently removed, though
files the user owns are not deleted. You will be prompted for confirmation
unless --force is specified.

Examples:
  # Remove a user (prompts for confirmation)
  dfsctl user remove alice

  # Remove a user non-interactively (for scripts and automation)
  dfsctl user remove alice --force`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	removeCmd.Flags().BoolP("force", "f", false, "Skip confirmation prompt")
	return removeCmd
}
