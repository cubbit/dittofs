package user

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newGetCmd() *cobra.Command {
	getCmd := &cobra.Command{
		Use:   "get <username|id>",
		Short: "Get user details",
		Long: `Get detailed information about a specific user on the DittoFS server.
Accepts either the username or the user's full ID (see 'dfsctl user list').
The output includes the user's role, UID, group memberships, account status,
and last-login timestamp. Use -o json or -o yaml for machine-readable output.

Examples:
  # Show user details as a table
  dfsctl user get alice

  # By ID
  dfsctl user get 3f2b1c4d-...

  # Output as JSON (useful for scripting)
  dfsctl user get alice -o json

  # Output as YAML
  dfsctl user get alice -o yaml`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	return getCmd
}
