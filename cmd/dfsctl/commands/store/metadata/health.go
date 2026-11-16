package metadata

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var healthCmd = &cobra.Command{
	Use:   "health",
	Short: "Check metadata store health",
	Long: `Perform a health check on a metadata store.

If the store is loaded in the runtime, calls its native health check method.
Otherwise, reports that the store is not loaded.

Examples:
  # Check health of a metadata store
  dfsctl store metadata health --name fast-meta

  # Output as JSON
  dfsctl store metadata health --name fast-meta -o json`, RunE: cmdutil.Unavailable,
}

func init() {
	healthCmd.Flags().String("name", "", "Metadata store name (required)")
	_ = healthCmd.MarkFlagRequired("name")
}
