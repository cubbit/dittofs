package block

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// gcCmd triggers an on-demand block-store GC run for the named share and
// prints the gc.GCStats summary.
var gcCmd = &cobra.Command{
	Use:   "gc <share>",
	Short: "Run garbage collection for a block store share",
	Long: `Trigger an on-demand GC run for the named share.

The mark phase enumerates every live ContentHash across all shares whose
remote-store config matches the named share (cross-share aggregation).
The sweep phase reclaims storage absent from the live set: it decrements
the refcount of each dead chunk's enclosing packed block and, once a
block holds no live chunks, deletes it from the remote and evicts its
local copy. Chunks must be dead and older than the configured grace
period (default 1h) before their block is decremented. The
last-run.json summary is persisted under the
share's gc-state directory and can be inspected with:

  dfsctl store block gc-status <share>

The run executes asynchronously on the server (the mark phase can take
minutes on a large or snapshot-heavy deployment). By default this command
polls until the job finishes, rendering progress; pass --no-wait to print
the job id and return immediately.

Use --dry-run to skip deletes and print up to dry_run_sample_size
candidate keys (default 1000). Recommended for first-time deployment
confidence and for debugging suspected mark-phase bugs.

Use --reconcile to additionally reap stranded file_blocks rows — rows
whose owning file was deleted before the unlink-refcount fix, which a
plain GC cannot reclaim because they keep their hashes in the live set.
Reconcile is server-wide (all shares) and the recommended way to recover
space leaked by older versions. Combine with --dry-run to preview.

Use --grace-period to override the configured sweep grace for this run
only. A zero grace (--grace-period 0) reaps every eligible orphan with no
age guard, bypassing the server's 5-minute floor — useful to reclaim
just-orphaned chunks immediately in tests or one-off cleanups. Cannot be
combined with --reconcile.

Examples:
  dfsctl store block gc myshare
  dfsctl store block gc myshare --dry-run
  dfsctl store block gc myshare --reconcile
  dfsctl store block gc myshare --grace-period 0
  dfsctl store block gc myshare --grace-period 30m
  dfsctl store block gc myshare --no-wait
  dfsctl store block gc myshare -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	gcCmd.Flags().Bool("dry-run", false, "Run mark + sweep enumeration but skip deletes; print candidate keys")
	gcCmd.Flags().Bool("reconcile", false, "Also reap stranded file_blocks rows leaked by older versions (server-wide), then sweep")
	gcCmd.Flags().Bool("no-wait", false, "Start the job and print its id without waiting for completion")
	gcCmd.Flags().Duration("grace-period", 0, "Override the sweep grace for this run (e.g. 30m, 0 to reap immediately); bypasses the server's 5m floor. Unset = server default")
}
