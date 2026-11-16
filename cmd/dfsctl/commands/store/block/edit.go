package block

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	editName   string
	editType   string
	editConfig string
	// S3 specific
	editBucket          string
	editRegion          string
	editEndpoint        string
	editAccessKey       string
	editSecretKey       string
	editParallelUploads int
)

var editCmd = &cobra.Command{
	Use:   "edit <name>",
	Short: "Edit a block store",
	Long: `Edit an existing block store configuration.

When run without flags, opens an interactive editor to modify store properties.
When flags are provided, only the specified fields are updated.

Examples:
  # Edit interactively
  dfsctl store block edit s3-store

  # Update config with JSON
  dfsctl store block edit s3-store --config '{"bucket":"new-bucket"}'

  # Update S3 settings
  dfsctl store block edit s3-store --bucket new-bucket --region us-west-2

  # Rename a store
  dfsctl store block edit s3-store --name archive-blocks`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	editCmd.Flags().StringVar(&editName, "name", "", "Rename the block store (names must be unique)")
	editCmd.Flags().StringVar(&editType, "type", "", "Store type: s3, memory")
	editCmd.Flags().StringVar(&editConfig, "config", "", "Store configuration as JSON")
	editCmd.Flags().StringVar(&editBucket, "bucket", "", "S3 bucket name (for s3)")
	editCmd.Flags().StringVar(&editRegion, "region", "", "AWS region (for s3)")
	editCmd.Flags().StringVar(&editEndpoint, "endpoint", "", "Custom S3 endpoint")
	editCmd.Flags().StringVar(&editAccessKey, "access-key", "", "AWS access key ID (for s3)")
	editCmd.Flags().StringVar(&editSecretKey, "secret-key", "", "AWS secret access key (for s3)")
	editCmd.Flags().IntVar(&editParallelUploads, "parallel-uploads", 0, "Max parallel chunk uploads to this remote (0 = adaptive: auto-tune to saturate the uplink)")
}
