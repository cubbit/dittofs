package system

import (
	"time"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

// drainTimeout overrides the client-side wait. 0 keeps the built-in default
// (6m); a bench harness can raise it to bound a slow cold-evict drain per run.
var drainTimeout time.Duration

var drainUploadsCmd = &cobra.Command{
	Use:   "drain-uploads",
	Short: "Wait for all pending uploads to complete",
	Long: `Wait for all in-flight block store uploads to complete across every share.

The command blocks until the server confirms that no blocks are queued for remote upload, or until the client timeout (--timeout, default 6m) is reached. The server can also end the wait early with a 504 if upload progress stalls for controlplane.drain_stall_timeout (default 5m). Use this before running benchmarks or taking snapshots to ensure a clean data boundary.

Examples:
  # Block until all pending uploads are flushed
  dfsctl system drain-uploads

  # Allow a slow cold-evict drain up to 15 minutes
  dfsctl system drain-uploads --timeout 15m

  # Get drain result as JSON (includes duration)
  dfsctl system drain-uploads -o json`, RunE: cmdutil.Unavailable,
}

func init() {
	drainUploadsCmd.Flags().DurationVar(&drainTimeout, "timeout", 0,
		"client-side wait for the drain (0 or negative uses the built-in default, 6m)")
}
