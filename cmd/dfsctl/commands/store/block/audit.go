package block

import (
	"github.com/spf13/cobra"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
)

// auditRefcountsCmd verifies the CAS manifest-consistency invariant for
// the named share: every block referenced by a file's manifest
// (FileAttr.Blocks) must have a backing FileChunk row in the metadata
// store. Persists last-run summary at <localStore>/audit-state/last-inv02.json.
var auditRefcountsCmd = &cobra.Command{
	Use:   "audit-refcounts <share>",
	Short: "Verify every manifest block reference has a backing FileChunk row",
	Long: `Run the CAS manifest-consistency audit for the named share.

Walks every file in the share and checks that each block referenced by the
file's manifest (FileAttr.Blocks) has a backing FileChunk row in the
metadata store. A manifest reference with no backing row is a genuine
DANGLING reference — the file claims a chunk the store has no record of, so
a read would return zeros or fail (the silent-data-loss class). The
invariant is "dangling refs == 0"; a non-zero count is real corruption
worth alerting on, so the command exits non-zero (use it as
` + "`audit-refcounts <share> || alert`" + `).

The legacy per-hash RefCount metric (∑ FileChunk.RefCount) was removed:
RefCount is not maintained in the content-addressed-store model (CAS blocks
are written Pending and never transition to Remote), so that sum was
structurally always 0 and produced false-positive "delta" alarms.

Persists last-run summary at <localStore>/audit-state/last-inv02.json
analogously to GC's last-run.json. Operator-invokable; no periodic schedule.

Examples:
  dfsctl store block audit-refcounts myshare
  dfsctl store block audit-refcounts myshare -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
