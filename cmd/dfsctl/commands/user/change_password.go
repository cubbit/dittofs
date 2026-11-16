package user

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newChangePasswordCmd() *cobra.Command {
	changePasswordCmd := &cobra.Command{
		Use:   "change-password",
		Short: "Change your own password",
		Long: `Change your own password as the currently authenticated user. This is
distinct from the admin-only "dfsctl user password" command: it verifies your
current password before accepting the new one, and updates the stored session
tokens automatically. You will be prompted for both passwords unless flags are
provided (flags are less secure as passwords may appear in shell history).

Examples:
  # Change password interactively (recommended — passwords are not echoed)
  dfsctl user change-password

  # Change password non-interactively (use with caution)
  dfsctl user change-password --current oldpass --new newpass`, RunE: cmdutil.Unavailable,
	}
	changePasswordCmd.Flags().StringP("current", "c", "", "Current password (prompts if not provided)")
	changePasswordCmd.Flags().StringP("new", "n", "", "New password (prompts if not provided)")
	return changePasswordCmd
}
