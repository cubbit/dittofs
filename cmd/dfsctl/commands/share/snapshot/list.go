package snapshot

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	listState      string
	listNamePrefix string
	listNoRelative bool
)

var listCmd = &cobra.Command{
	Use:   "list <share>",
	Short: "List snapshots for a share",
	Long: `List snapshots for a share, newest-first.

Examples:
  # List as table
  dfsctl share snapshot list /archive

  # Filter by state
  dfsctl share snapshot list /archive --state ready

  # Filter by name prefix
  dfsctl share snapshot list /archive --name-prefix weekly

  # JSON output
  dfsctl share snapshot list /archive -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	listCmd.Flags().StringVar(&listState, "state", "", "Filter by state (creating|ready|failed)")
	listCmd.Flags().StringVar(&listNamePrefix, "name-prefix", "", "Filter by name prefix")
	listCmd.Flags().BoolVar(&listNoRelative, "no-relative", false, "Print absolute timestamps instead of relative")
}
