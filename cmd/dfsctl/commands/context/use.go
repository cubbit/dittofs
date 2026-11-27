package context

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

func newUseCmd(env cmdutil.Env) *cobra.Command {
	useCmd := &cobra.Command{
		Use:   "use <name>",
		Short: "Switch to a different context",
		Long: `Switch the active context so that subsequent dfsctl commands target a different server.

The new active context is saved to the local credential file. Run 'context current' afterwards to confirm the switch, or 'context list' to see all available context names.

Examples:
  # Switch to the "production" context
  dfsctl context use production

  # Switch to a local development server context
  dfsctl context use local-dev`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runContextUse(cmd, env, args) },
	}
	return useCmd
}

func runContextUse(cmd *cobra.Command, env cmdutil.Env, args []string) error {
	contextName := args[0]

	if err := cmdutil.Locked(cmd.Context(), env, func(s *credentials.Store) error { return s.UseContext(contextName) }); err != nil {
		if err == credentials.ErrContextNotFound {
			return fmt.Errorf("context '%s' not found\n\n"+
				"List available contexts:\n"+
				"  dfsctl context list", contextName)
		}
		return fmt.Errorf("failed to switch context: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Switched to context: %s\n", contextName)
	return nil
}
