package block

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List block stores",
	Long: `List all block stores on the DittoFS server.

Shows the name, ID, type (s3 or memory), and configuration of each registered
block store. Other sub-commands accept either form, so this is where you
find both. Use it to confirm which stores exist before adding, editing, or
running health checks against one, or to map the store IDs emitted by
'share show -o json' back to a store name ('share show' table output already
resolves them to names).

Examples:
  # List as table
  dfsctl store block list

  # List as JSON
  dfsctl store block list -o json

  # List as YAML
  dfsctl store block list -o yaml`, RunE: cmdutil.Unavailable,
}
