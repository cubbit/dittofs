package store

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// checkIncludeHoles adds the uncovered ranges no file claims to the detail
// table. They are always counted in the summary; they are off by default in
// the detail because a sparse file legitimately has them.
var checkIncludeHoles bool

// checkRepair turns the scan into a repair run, and checkRepairYes skips the
// confirmation. Without --yes the run is a dry run that ends at the prompt.
var (
	checkRepair    bool
	checkRepairYes bool
)

var checkCmd = &cobra.Command{
	Use:   "check [share]",
	Short: "Scan manifests for ranges no chunk covers",
	Long: `Scan the store for files whose manifest does not describe the whole file.

Per payload the scan compares the byte ranges the manifest rows cover against
the file's recorded size and reports three structural defects:

  * ranges no manifest row covers
  * rows that exist but carry no parseable chunk offset, so no reader can place
    them — a read of such a range refuses rather than serving bytes
  * rows whose content hash the synced-hash store has no record of

The scan is metadata-only. No block is fetched, no file data is read and no
remote object is touched, so it costs a metadata walk regardless of how much
data the store holds and incurs no egress. It answers "how many files are
affected" without reading every file. It does NOT verify block contents —
remote fetches are already hash-verified on the read path.

An uncovered range is only reported as damage when the file's own block list
claims data lives there. A range nothing claims cannot be told apart from a
legitimate sparse hole or from bytes written but not yet rolled up into the
manifest, so it is counted separately and never fails the command. Pass
--include-holes to list those ranges too.

The unknown-hash check is skipped on a share with no remote store: nothing is
ever marked synced there, so every row would be reported.

--repair adds a second half: for every finding, work out whether the store
holds enough evidence to put the manifest back, list what that would take, and
apply it once you confirm. Nothing is written before the prompt, so --repair on
its own is a dry run; --yes answers the prompt for scripted use. Only two kinds
of finding are repairable, and both restore a row the file already claims:

  * a row with no parseable offset whose hash and length match exactly one
    range the file claims and no row covers — the row is moved to that offset,
    keeping everything else about it
  * a range the file claims that no row covers, whose hash the synced-hash
    store resolves — a row is written for it, and the remote already holds the
    bytes it names

A repair only ever adds coverage the file already asked for. It never drops a
row, never widens or narrows a claim, and never marks a hash synced. Findings
it cannot pair with that evidence — a row matching no claim, a claim whose hash
nothing resolves, a hash the synced-hash store does not know — are reported and
left alone: those need the bytes re-synced, not the metadata rewritten.

With no argument every share is scanned. The command exits non-zero when
damage is found, so it can be scripted (` + "`dfsctl store check || alert`" + `).

Examples:
  dfsctl store check
  dfsctl store check myshare
  dfsctl store check myshare --include-holes
  dfsctl store check myshare -o json
  dfsctl store check myshare --repair
  dfsctl store check myshare --repair --yes`,
	Args: cobra.MaximumNArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	checkCmd.Flags().BoolVar(&checkIncludeHoles, "include-holes", false,
		"list uncovered ranges that no file claims (legitimate for sparse files)")
	checkCmd.Flags().BoolVar(&checkRepair, "repair", false,
		"list the findings the store can put back, and apply them once confirmed")
	checkCmd.Flags().BoolVar(&checkRepairYes, "yes", false,
		"Skip confirmation prompt")
}
