package snapshot

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var showCmd = &cobra.Command{
	Use:   "show <share> <id>",
	Short: "Show details of a snapshot",
	Long: `Show the full detail record for a single snapshot.

Displays state, remote-durability flag, manifest block count, total dump
size, the ID of any snapshot this was a retry of, and any error message from a
failed run. The snapshot ID can be a full UUID or the 8-character prefix shown
by 'share snapshot list'. Use this to investigate a failed or pending snapshot
before deciding whether to retry or delete it.

Examples:
  # Show a snapshot by its short ID
  dfsctl share snapshot show /archive snap-abc1

  # Show using the full UUID
  dfsctl share snapshot show /archive 3f2a1b4c-0000-0000-0000-000000000001

  # Emit the snapshot record as JSON
  dfsctl share snapshot show /archive snap-abc1 -o json`,
	Args: cobra.ExactArgs(2), RunE: cmdutil.Unavailable,
}
