package commands

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newLoginCmd() *cobra.Command {
	loginCmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with DittoFS server",
		Long: `Authenticate with a DittoFS server and store credentials locally.

Not available until the CLI is rebuilt on the v2 API: signing in returns with
that API's accounts. Credentials are kept in ~/.config/dfsctl/config.json under
a named context, which the context subcommand manages.

Examples:
  # First login to a local server (prompts for password)
  dfsctl login --server http://localhost:8080 --username admin

  # Login to a remote server whose certificate a private CA signs
  dfsctl login --server https://dfs.example.com --username admin --cacert ca.pem

  # Login passing password on the command line (less secure; avoid in shared environments)
  dfsctl login --server http://localhost:8080 -u admin -p secret

  # Re-login to the already-stored server (password prompt only)
  dfsctl login`, RunE: cmdutil.Unavailable,
	}
	loginCmd.Flags().String("server", "http://localhost:8080", "Server URL")
	loginCmd.Flags().StringP("username", "u", "", "Username")
	loginCmd.Flags().StringP("password", "p", "", "Password")
	loginCmd.Flags().StringP("context", "c", "", "Context name (defaults to current or auto-generated)")
	loginCmd.Flags().String("cacert", "", "Path to a PEM CA bundle trusted for the server certificate")
	return loginCmd
}
