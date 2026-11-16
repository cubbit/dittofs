package block

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// reclaimCmd deletes orphaned block storage (zero-ref + leaked records and
// record-less remote objects) across all remote-backed shares. It is the deleting
// counterpart to `reconcile`.
var reclaimCmd = &cobra.Command{
	Use:   "reclaim",
	Short: "Reclaim orphaned block storage (deletes; use --dry-run to preview)",
	Long: `Delete orphaned block storage across every remote-backed share:

  - zero-ref records: no live locator and a zero live chunk count (a crash
    between decrementing the count and deleting the record);
  - leaked records: no live locator but a stale non-zero count, left behind
    when a block re-carve moved the hash without decrementing the old block;
  - record-less remote objects: an uploaded block with no backing record,
    older than the grace window (a commit that never landed).

A record with no live locator is terminally dead — block IDs are never reused —
so reclaiming records needs no grace window. Only record-less objects use one, to
spare an upload whose commit may still be in flight.

This DELETES. Run 'dfsctl store block reconcile' first to review what exists, or
pass --dry-run here to preview the exact set this command would delete without
deleting anything.

Examples:
  # Preview what would be reclaimed
  dfsctl store block reclaim --dry-run

  # Reclaim orphaned block storage
  dfsctl store block reclaim

  # As JSON for scripting
  dfsctl store block reclaim -o json`,
	Args: cobra.NoArgs, RunE: cmdutil.Unavailable,
}

func init() {
	reclaimCmd.Flags().Bool("dry-run", false, "Report the records that would be reclaimed without deleting anything")
}
