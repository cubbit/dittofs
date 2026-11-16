package snapshotpolicy

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all snapshot policies",
	Long: `List every snapshot policy configured across all shares.

Each row shows the share name, interval, keep-last count, TTL age bound, the
name prefix used for scheduler-created snapshots, whether the policy is enabled,
and the next scheduled run time. Use this command to audit which shares have
automatic snapshots active and when they last ran.

Examples:
  # List all snapshot policies as a table
  dfsctl share snapshot-policy list

  # Emit the full list as JSON for scripting
  dfsctl share snapshot-policy list -o json

  # Emit as YAML
  dfsctl share snapshot-policy list -o yaml`,
	Args: cobra.NoArgs, RunE: cmdutil.Unavailable,
}
