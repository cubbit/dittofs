package commands

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show server status",
		Long: `Display the status of the connected DittoFS server.

Calls the /health endpoint on the server configured in the current context and
reports whether the server is running, how long it has been up, and whether the
control-plane database is reachable. When a valid token is present, per-entity
detail (shares, stores) is fetched from the API and rendered as a
color-coded table. Use -o json or -o yaml for machine-readable output.

Examples:
  # Show status of the currently active server
  dfsctl status

  # Emit machine-readable JSON output
  dfsctl status -o json

  # Check a specific server without logging in (token fetched from stored context)
  dfsctl status --server http://dfs.example.com:8080`, RunE: cmdutil.Unavailable,
	}
	return statusCmd
}
