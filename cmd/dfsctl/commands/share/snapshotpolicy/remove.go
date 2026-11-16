package snapshotpolicy

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeYes bool

var removeCmd = &cobra.Command{
	Use:   "remove <share>",
	Short: "Remove a share's snapshot policy",
	Long: `Remove the snapshot policy for a share.

Existing snapshots are not removed; only the schedule and automatic pruning
stop. After removal, no new scheduled snapshots will be created and old
scheduled snapshots will no longer be pruned. Use 'snapshot-policy set' to
recreate a policy at any time.

Examples:
  # Remove the policy, with a confirmation prompt
  dfsctl share snapshot-policy remove /archive

  # Remove without a confirmation prompt (useful in scripts)
  dfsctl share snapshot-policy remove /archive --yes`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	removeCmd.Flags().BoolVar(&removeYes, "yes", false, "Skip confirmation prompt")
}
