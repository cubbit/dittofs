package quota

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list <share>",
	Short: "List all quotas on a share",
	Long: `List all per-identity quotas configured on a share.

Outputs one row per quota entry, showing the scope (user/group/default-user),
identity ID, hard and soft byte/file limits, current usage, and the grace
period. Use this to audit who has explicit limits before adjusting or removing
them.

Examples:
  # List quotas as a table
  dfsctl quota list /archive

  # List as JSON
  dfsctl quota list /archive -o json

  # List as YAML
  dfsctl quota list /archive -o yaml`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
