package snapshot

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeYes bool

var removeCmd = &cobra.Command{
	Use:   "remove <share> <id>",
	Short: "Remove a snapshot",
	Long: `Remove a snapshot. This is irreversible.

Examples:
  # Remove with prompt
  dfsctl share snapshot remove /archive snap-abc123

  # Remove without prompt
  dfsctl share snapshot remove /archive snap-abc123 --yes`,
	Args: cobra.ExactArgs(2), RunE: cmdutil.Unavailable,
}

func init() {
	removeCmd.Flags().BoolVar(&removeYes, "yes", false, "Skip confirmation prompt")
}
