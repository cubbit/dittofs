package share

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var listMountsCmd = &cobra.Command{
	Use:     "list-mounts [share]",
	Aliases: []string{"lm", "mounts"},
	Short:   "List mounted DittoFS shares",
	Long: `List all currently mounted DittoFS shares.

This command shows NFS and SMB mounts from localhost that are likely DittoFS shares.
Optionally filter by share name.

Examples:
  # List all mounted DittoFS shares
  dfsctl share list-mounts

  # Filter by share name
  dfsctl share list-mounts /export

  # Short alias
  dfsctl share mounts`,
	Args: cobra.MaximumNArgs(1), RunE: cmdutil.Unavailable,
}
