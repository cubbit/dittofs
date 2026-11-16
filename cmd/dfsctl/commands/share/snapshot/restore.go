package snapshot

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	restoreYes   bool
	restoreForce bool
)

var restoreCmd = &cobra.Command{
	Use:   "restore <share> <id>",
	Short: "Restore a snapshot into a (disabled) share",
	Long: `Restore a snapshot into a share.

The target share must be disabled first; the command refuses with a
hint when the share is still enabled. Restore is destructive: a safety
snapshot is taken before the reset and its ID is printed on success
(delete it once you have verified the restored share).

Examples:
  # Restore with prompt
  dfsctl share disable /archive
  dfsctl share snapshot restore /archive snap-abc123

  # Restore without prompt
  dfsctl share snapshot restore /archive snap-abc123 --yes

  # Restore a snapshot that is not remotely durable
  dfsctl share snapshot restore /archive snap-abc123 --yes --force`,
	Args: cobra.ExactArgs(2), RunE: cmdutil.Unavailable,
}

func init() {
	restoreCmd.Flags().BoolVar(&restoreYes, "yes", false, "Skip confirmation prompt")
	restoreCmd.Flags().BoolVar(&restoreForce, "force", false, "Allow restoring a snapshot that is not remotely durable")
}
