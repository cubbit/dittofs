package commands

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var switchUserPassword string

var switchUserCmd = &cobra.Command{
	Use:   "switch-user <username>",
	Short: "Switch to a different user on the current server",
	Long: `Switch to a different user on the current server without changing the server URL.

Authenticates as the given username against the server in the active context and
stores the resulting tokens under a new context named username@host. If a context
for that user already exists with a non-expired token, it activates it immediately
without re-authenticating. Use dfsctl context to inspect or remove stored contexts.

Examples:
  # Switch to a different user (prompts for password)
  dfsctl switch-user operator

  # Switch to a user providing the password inline
  dfsctl switch-user operator -p secret

  # Switch back to admin on the same server
  dfsctl switch-user admin`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	switchUserCmd.Flags().StringVarP(&switchUserPassword, "password", "p", "", "Password (will prompt if not provided)")
}
