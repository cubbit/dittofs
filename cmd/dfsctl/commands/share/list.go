package share

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all shares",
		Long: `List all shares configured on the DittoFS server.

Each row shows the share's name, the metadata and block stores it uses, its
storage quota and current usage, the default permission level, the block
retention policy, and whether the share is currently enabled. Use this command
to get a quick overview of all shares before running share-specific commands.

USED is the share's logical size: the sum of its file sizes, the same figure
'du --apparent-size' would report over the mounted share. It is what QUOTA is
measured against. It is not a count of bytes on disk, and will not match one:
blocks are deduplicated and compressed.

Examples:
  # List all shares as a table
  dfsctl share list

  # List shares and pipe to grep to find disabled ones
  dfsctl share list | grep " -$"

  # Output the full share list as JSON for scripting
  dfsctl share list -o json

  # Output as YAML
  dfsctl share list -o yaml`, RunE: cmdutil.Unavailable,
	}
	return listCmd
}
