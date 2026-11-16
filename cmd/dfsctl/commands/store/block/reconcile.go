package block

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// reconcileCmd reports orphaned block storage across all remote-backed shares
// WITHOUT deleting anything.
var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Report orphaned block storage (read-only; no deletes)",
	Long: `Scan every remote-backed share for orphaned block storage and print a
classified report. This is READ-ONLY: it deletes nothing, decrements nothing,
and changes no markers. Use it to review what the later reclaim stages would
act on before running them.

Three orphan classes are reported:

  Zero-ref records       Block records with no live locator and a zero live
                         chunk count — a crash between decrementing the count
                         and deleting the record.
  Leaked blocks          Block records with no live locator but a non-zero live
                         chunk count — a re-carve moved the hash onto a new
                         block without decrementing this one.
  Orphan remote objects  blocks/<id> objects with no backing record, older than
                         the grace window — the upload succeeded but the commit
                         failed. Objects within the grace window are preserved
                         (they may be freshly uploaded, commit pending).

Each class reports an exact count plus a bounded sample of IDs (truncated is
flagged when the full set is larger than the sample).

Examples:
  # Report orphans as a table
  dfsctl store block reconcile

  # As JSON for scripting
  dfsctl store block reconcile -o json`,
	Args: cobra.NoArgs, RunE: cmdutil.Unavailable,
}
