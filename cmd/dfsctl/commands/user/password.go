package user

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newPasswordCmd() *cobra.Command {
	passwordCmd := &cobra.Command{
		Use:   "password <username>",
		Short: "Reset a user's password",
		Long: `Reset a user's password (admin operation). Unlike "dfsctl user
change-password", this command does not require knowledge of the current
password and is intended for administrators recovering access for a user.
After the reset the account is marked as must-change-password, requiring
the user to set a new password on their next login.

Examples:
  # Reset a user's password interactively (password not echoed)
  dfsctl user password alice

  # Reset password non-interactively (use with caution)
  dfsctl user password alice --password newsecret`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	passwordCmd.Flags().StringP("password", "p", "", "New password (prompts if not provided)")
	return passwordCmd
}
