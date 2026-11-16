package metadata

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// recomputeUsageCmd rebuilds a metadata store's used-bytes counters from its
// file rows and reports what the named share held before and after.
var recomputeUsageCmd = &cobra.Command{
	Use:   "recompute-usage <share>",
	Short: "Rebuild a share's used-bytes counter from its files",
	Long: `Rebuild the used-bytes counters of the metadata store backing the named share.

The counters are maintained transactionally as files are written and removed, so
they are normally already correct. This repairs a store where they are not: a
share carrying bytes it no longer holds reports itself fuller than it is, and
because that figure is what the share quota is checked against, it can refuse
writes to a share that is actually empty.

The rebuild scans every file row in the store, so it takes time in proportion to
the store's size, and it repairs every share that store serves — not only the
one named here. Nothing runs it automatically; a per-file walk on every server
start is a cost every share would pay forever to fix a number that is almost
always already right.

--dry-run answers "are these numbers actually wrong" without repairing
anything. It derives the same figures from the file rows, writes nothing, and
names every usage bucket whose counter disagrees with them, with both numbers.
The repair replaces the counters, so running it to find out destroys the
evidence of what was wrong.

A dry run against a store that is taking writes reports small transient deltas:
the file rows and the counters are read at different instants, so a write in
between shows up as a difference, and a file written during the scan can show up
under one scope and not the other. A drift bug does not look like that — it
persists across runs and does not track live traffic.

A "share" row compares the share's own total rather than one owner's bucket.
That total is what a share quota is checked against and what df reports, and it
can drift on its own, so it is reported on its own.

Examples:
  dfsctl store metadata recompute-usage myshare --dry-run
  dfsctl store metadata recompute-usage myshare
  dfsctl store metadata recompute-usage myshare -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

// recomputeUsageDryRun reports drift without repairing it.
var recomputeUsageDryRun bool

func init() {
	recomputeUsageCmd.Flags().BoolVar(&recomputeUsageDryRun, "dry-run", false,
		"Report which usage counters disagree with the file rows, and repair nothing")
}
