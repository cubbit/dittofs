package metadata

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List metadata stores",
	Long: `List all metadata stores on the DittoFS server.

Displays the name, ID, and type of every registered metadata store. Other
sub-commands accept either form, so this is where you find both. Use it to
confirm which stores are configured before adding or removing one, or to map the
store IDs emitted by 'share show -o json' back to a store name ('share show'
table output already resolves them to names).

Examples:
  # List as table
  dfsctl store metadata list

  # List as JSON
  dfsctl store metadata list -o json

  # List as YAML
  dfsctl store metadata list -o yaml`, RunE: cmdutil.Unavailable,
}
