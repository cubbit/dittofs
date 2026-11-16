package block

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// ErrNoGCRunYet signals that the share has no `last-run.json` to read —
// either GC has never run for this share, or the local store backend is
// non-persistent (in-memory). The caller can detect this with errors.Is
// to distinguish from genuine HTTP / network failures.
var ErrNoGCRunYet = errors.New("no GC run recorded for share yet")

// gcStatusCmd reads <gcStateRoot>/last-run.json for the named share and
// prints the parsed gc.GCRunSummary so operators can confirm the
// last GC run completed cleanly without tailing logs.
var gcStatusCmd = &cobra.Command{
	Use:   "gc-status <share>",
	Short: "Show the last block-store GC run summary for a share",
	Long: `Print the most recent garbage collection run summary for the named share.

Reads last-run.json persisted by the most recent completed GC run. Use
this to confirm that the last run swept objects cleanly (ErrorCount == 0),
check how many bytes were freed, and review the duration without tailing
logs. Returns a non-zero exit if no run has been recorded yet (the share
has never been GC'd or its local store has no persistent root).

Examples:
  # Show the last GC summary as a table
  dfsctl store block gc-status myshare

  # Show as JSON for scripting
  dfsctl store block gc-status myshare -o json

  # Show as YAML
  dfsctl store block gc-status myshare -o yaml`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
