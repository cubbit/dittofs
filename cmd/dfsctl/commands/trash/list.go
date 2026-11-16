package trash

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// listCmd prints the recycle-bin entries for a share.
var listCmd = &cobra.Command{
	Use:   "list <share>",
	Short: "List recycle-bin entries for a share",
	Long: `List the recycled entries in a share's recycle bin.

Each row shows the current path under #recycle, the original path before deletion, who deleted it, when, its size, and whether it is a file or directory subtree. Use -o json to get structured output for scripting.

Examples:
  # List the recycle bin for a share as a table
  dfsctl trash list myshare

  # Get the bin contents as JSON
  dfsctl trash list myshare -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
