package context

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

func newRemoveCmd(env cmdutil.Env) *cobra.Command {
	removeCmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a context",
		Long: `Remove a saved server context and its stored credentials.

The context's configuration and access token are removed from the local credential store. Use this to clean up after decommissioning a server or when a context was created by mistake.

Examples:
  # Remove the "staging" context with a confirmation prompt
  dfsctl context remove staging

  # Remove without the confirmation prompt (e.g. in a script)
  dfsctl context remove staging --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runContextRemove(cmd, env, args) },
	}
	removeCmd.Flags().BoolP("force", "f", false, "Skip confirmation")
	return removeCmd
}

func runContextRemove(cmd *cobra.Command, env cmdutil.Env, args []string) error {
	contextName := args[0]

	store, err := credentials.Read(env.ConfigPath)
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	if _, err = store.GetContext(contextName); err != nil {
		if err == credentials.ErrContextNotFound {
			return fmt.Errorf("context '%s' not found", contextName)
		}
		return fmt.Errorf("failed to get context: %w", err)
	}

	force, _ := cmd.Flags().GetBool("force")
	return cmdutil.RunDeleteWithConfirmation(cmd, env, "Context", contextName, force, func() error {
		return cmdutil.Locked(cmd.Context(), env, func(s *credentials.Store) error { return s.DeleteContext(contextName) })
	})
}
