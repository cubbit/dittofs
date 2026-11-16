package share

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a share",
	Long: `Permanently remove a share from the DittoFS server.

Removing a share deletes its configuration, its records in its metadata store
and its local journal, so a share created again under the same name starts
empty. Blocks already synced to a remote block store are left to garbage
collection, and the metadata and block stores themselves are kept. If the
share's data cannot be deleted, the removal fails and keeps the share, so it
can be repeated. This operation is irreversible: you will be prompted for
confirmation unless --force is specified. Disable the share first
('dfsctl share disable') if you want to drain active clients before removing.

Examples:
  # Remove a share, prompted for confirmation
  dfsctl share remove /archive

  # Remove without a confirmation prompt (useful in scripts)
  dfsctl share remove /archive --force

  # Drain clients first, then remove without prompting
  dfsctl share disable /archive && dfsctl share remove /archive --force`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
}
