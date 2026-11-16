package snapshotpolicy

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <share>",
	Short: "Trigger a share's snapshot policy now (manual override)",
	Long: `Run a share's snapshot policy immediately, ignoring its interval.

This creates a scheduled snapshot now, advances the policy's run clock, and
prunes per the retention bounds (keep-last / TTL). It is useful to take an
out-of-band snapshot before a maintenance window without changing the schedule
or creating a permanent manual snapshot that will never be pruned.

Examples:
  # Trigger the policy for a share immediately
  dfsctl share snapshot-policy run /archive

  # Trigger and then confirm the snapshot was created
  dfsctl share snapshot-policy run /archive && dfsctl share snapshot list /archive --state ready`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
