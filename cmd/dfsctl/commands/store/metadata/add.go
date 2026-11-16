package metadata

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	addName     string
	addType     string
	addConfig   string
	addDBPath   string
	addInMemory bool
)

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a metadata store",
	Long: `Add a new metadata store to the DittoFS server.

The only metadata store type is badger (BadgerDB, embedded):
  --db-path:   Path to the BadgerDB directory (or prompted interactively)
  --in-memory: Keep the whole store in RAM instead; its contents are lost
               when the server stops. Meant for tests and throwaway servers.

Examples:
  # Add a BadgerDB store with flags
  dfsctl store metadata add --name persistent-meta --db-path /data/meta

  # Add a BadgerDB store interactively
  dfsctl store metadata add --name persistent-meta

  # Add an in-memory store
  dfsctl store metadata add --name scratch-meta --in-memory`, RunE: cmdutil.Unavailable,
}

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Store name (required)")
	addCmd.Flags().StringVar(&addType, "type", "badger", "Store type (only badger is supported)")
	addCmd.Flags().StringVar(&addConfig, "config", "", "Store configuration as JSON (for advanced config)")
	addCmd.Flags().StringVar(&addDBPath, "db-path", "", "BadgerDB directory path")
	addCmd.Flags().BoolVar(&addInMemory, "in-memory", false, "Keep the store in RAM only (contents lost on restart)")
	addCmd.MarkFlagsMutuallyExclusive("db-path", "in-memory")
	_ = addCmd.MarkFlagRequired("name")
}
