package context

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

func newCurrentCmd() *cobra.Command {
	currentCmd := &cobra.Command{
		Use:   "current",
		Short: "Show current context",
		Long: `Display the name and connection details of the currently active context.

The output shows the server URL, logged-in user, and authentication status. Use this command to confirm which server subsequent dfsctl commands will target before running destructive operations.

Examples:
  # Show the active context as a human-readable summary
  dfsctl context current

  # Get the active context as JSON for scripting
  dfsctl context current --output json`,
		RunE: runContextCurrent,
	}
	return currentCmd
}

func runContextCurrent(cmd *cobra.Command, args []string) error {
	// Load credential store
	store, err := credentials.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize credential store: %w", err)
	}

	// Get current context
	contextName := store.GetCurrentContextName()
	if contextName == "" {
		return fmt.Errorf("no current context set\n\n" +
			"Login to a server first:\n" +
			"  dfsctl login --server http://localhost:8080")
	}

	ctx, err := store.GetContext(contextName)
	if err != nil {
		return fmt.Errorf("failed to get context: %w", err)
	}

	info := ContextInfo{
		Name:      contextName,
		Current:   true,
		ServerURL: ctx.ServerURL,
		Username:  ctx.Username,
		LoggedIn:  ctx.AccessToken != "" && !ctx.IsExpired(),
	}

	return cmdutil.PrintOutput(cmd, info, false, "", ContextList{info})
}
