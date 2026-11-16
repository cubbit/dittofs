package idmap

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var listProvider string

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List identity mappings",
	Long: `List identity mappings registered on the DittoFS server. Each row shows the
provider name, the external principal, and the local DittoFS username it maps to.
Use --provider to filter the list to a single identity provider, and -o json or
-o yaml for machine-readable output.

Examples:
  # List all identity mappings
  dfsctl idmap list

  # Show only Kerberos mappings
  dfsctl idmap list --provider kerberos

  # Show only AD mappings as JSON
  dfsctl idmap list --provider ad -o json

  # Output all mappings as YAML
  dfsctl idmap list -o yaml`, RunE: cmdutil.Unavailable,
}

func init() {
	listCmd.Flags().StringVar(&listProvider, "provider", "", "Filter by identity provider (e.g., kerberos, oidc, ad)")
}
