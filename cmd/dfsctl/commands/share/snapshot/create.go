package snapshot

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	createName     string
	createNoVerify bool
	createRetry    string
	createNoWait   bool
)

var createCmd = &cobra.Command{
	Use:   "create <share>",
	Short: "Create a snapshot of a share",
	Long: `Create a snapshot of a share.

By default the command blocks until the snapshot reaches a terminal
state (ready or failed). Use --no-wait to return immediately after the
snapshot is enqueued.

Examples:
  # Block until snapshot is ready
  dfsctl share snapshot create /archive --name weekly

  # Return immediately with the new snapshot ID
  dfsctl share snapshot create /archive --no-wait

  # Skip the remote-durability verify step
  dfsctl share snapshot create /archive --no-verify

  # Retry a failed previous snapshot
  dfsctl share snapshot create /archive --retry snap-prev123`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	createCmd.Flags().StringVar(&createName, "name", "", "Human-readable name for the snapshot")
	createCmd.Flags().BoolVar(&createNoVerify, "no-verify", false, "Skip the remote-durability verify step")
	createCmd.Flags().StringVar(&createRetry, "retry", "", "Retry a previous failed snapshot by ID")
	createCmd.Flags().BoolVar(&createNoWait, "no-wait", false, "Return immediately instead of waiting for completion")
}
