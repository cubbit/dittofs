package context

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

func newRenameCmd(env cmdutil.Env) *cobra.Command {
	renameCmd := &cobra.Command{
		Use:   "rename <old-name> <new-name>",
		Short: "Rename a context",
		Long: `Rename an existing server context to a new name.

All stored credentials and the active-context pointer are updated atomically; no re-authentication is needed. Use this after promoting a staging server to production, or simply to give a context a more descriptive name.

Examples:
  # Rename the "default" context to "production"
  dfsctl context rename default production

  # Rename a development context to something more descriptive
  dfsctl context rename dev local-dev`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error { return runContextRename(cmd, env, args) },
	}
	return renameCmd
}

func runContextRename(cmd *cobra.Command, env cmdutil.Env, args []string) error {
	oldName := args[0]
	newName := args[1]

	if err := cmdutil.Locked(cmd.Context(), env, func(s *credentials.Store) error { return s.RenameContext(oldName, newName) }); err != nil {
		if err == credentials.ErrContextNotFound {
			return fmt.Errorf("context '%s' not found", oldName)
		}
		return fmt.Errorf("failed to rename context: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Context renamed: %s -> %s\n", oldName, newName)
	return nil
}
