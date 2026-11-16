package snapshotpolicy

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <share>",
	Short: "Show a share's snapshot policy",
	Long: `Show the snapshot policy configured for a share.

Displays the interval, retention bounds (keep-last and TTL), name prefix,
enabled state, and next/last run times. Use this command before editing a
policy to review its current configuration, or to confirm that a policy is
active and when it is next scheduled to run.

Examples:
  # Show the policy for a share
  dfsctl share snapshot-policy show /archive

  # Emit the policy record as JSON
  dfsctl share snapshot-policy show /archive -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
