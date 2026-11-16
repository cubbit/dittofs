package share

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var enableCmd = &cobra.Command{
	Use:   "enable <name>",
	Short: "Enable a share (accept new connections)",
	Long: `Enable a share on the DittoFS server.

Re-enabling a share allows new client connections and lifts the drain state
set by 'share disable'. Re-enabling is a deliberate operator act; no
mid-restore safety check is performed — the operator owns the timing.

Examples:
  # Enable a share after a completed metadata-store restore
  dfsctl share enable /archive

  # Emit the updated Share record as JSON
  dfsctl share enable /archive -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
