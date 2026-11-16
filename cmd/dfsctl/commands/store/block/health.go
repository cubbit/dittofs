package block

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var healthCmd = &cobra.Command{
	Use:   "health",
	Short: "Check block store health",
	Long: `Perform a health check on a block store configuration.

For S3 stores, performs a HeadBucket call to verify connectivity.
For memory stores, always reports healthy.

Examples:
  # Check health of a block store
  dfsctl store block health --name s3-store

  # Output as JSON
  dfsctl store block health --name s3-store -o json`, RunE: cmdutil.Unavailable,
}

func init() {
	healthCmd.Flags().String("name", "", "Block store name (required)")
	_ = healthCmd.MarkFlagRequired("name")
}
