// Package context implements context management subcommands for dfsctl.
package context

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

// New returns the context subcommand.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage server contexts",
		Long: `Manage named connection contexts for one or more DittoFS servers.

Each context stores a server URL, authentication credentials, and a display name. Contexts work similarly to kubectl contexts: log in once per server, then switch between them with 'context use'. All subsequent dfsctl commands use the active context automatically.

Examples:
  # List all saved contexts
  dfsctl context list

  # Switch to a context named "production"
  dfsctl context use production

  # Show which context is currently active
  dfsctl context current

  # Remove a context that is no longer needed
  dfsctl context remove staging`,
	}
	cmd.AddCommand(newListCmd(env))
	cmd.AddCommand(newUseCmd(env))
	cmd.AddCommand(newCurrentCmd(env))
	cmd.AddCommand(newRenameCmd(env))
	cmd.AddCommand(newRemoveCmd(env))
	return cmd
}
