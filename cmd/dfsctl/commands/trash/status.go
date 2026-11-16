package trash

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// statusCmd prints the recycle-bin roll-up for a share.
var statusCmd = &cobra.Command{
	Use:   "status <share>",
	Short: "Show recycle-bin status for a share",
	Long: `Print a summary of a share's recycle bin: whether trash is enabled, the number of recycled entries, their combined size, and the timestamp of the oldest deletion.

Use this command for a quick health check before deciding whether to empty the bin or restore items. Pass -o json for machine-readable output.

Examples:
  # Show recycle bin status as a summary table
  dfsctl trash status myshare

  # Get status as JSON for scripting
  dfsctl trash status myshare -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
