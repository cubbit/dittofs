package trash

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// restoreCmd moves a recycled root back out of the recycle bin.
var restoreCmd = &cobra.Command{
	Use:   "restore <share> <bin-path>",
	Short: "Restore a recycled file or directory",
	Long: `Restore the recycled entry at <bin-path> back into the share.

Without --to the entry is moved back to the path it occupied before deletion. If that location is now taken, use --to to restore it elsewhere. The bin-path argument is the value shown in the PATH column of 'trash list'.

Examples:
  # Restore a file to its original location
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt"

  # Restore to a different path when the original location is occupied
  dfsctl trash restore myshare "#recycle/2026-06-01/report.txt" --to /archive/report.txt`,
	Args: cobra.ExactArgs(2), RunE: cmdutil.Unavailable,
}

func init() {
	restoreCmd.Flags().String("to", "", "Restore to this share-relative path instead of the original location")
}
