package snapshotpolicy

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	setInterval   string
	setKeepLast   int
	setTTL        string
	setDisabled   bool
	setNamePrefix string
)

var setCmd = &cobra.Command{
	Use:   "set <share>",
	Short: "Create or update a share's snapshot policy",
	Long: `Create or update a share's snapshot policy.

--interval accepts a Go duration ("24h", "6h", "1h30m") or a shorthand
(@hourly, @daily, @weekly). Retention is bounded by --keep-last (0 = no
count bound) and --ttl (Go duration, empty = no age bound); a snapshot is
pruned when it falls outside the newest keep-last OR is older than ttl.

Re-running set on an existing policy updates the config but preserves the
run clock (it does not reset the next-run time).

Examples:
  dfsctl share snapshot-policy set /archive --interval @daily --keep-last 7 --ttl 720h
  dfsctl share snapshot-policy set /archive --interval 6h --disabled`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	setCmd.Flags().StringVar(&setInterval, "interval", "", "Snapshot cadence: Go duration or @hourly/@daily/@weekly (required)")
	setCmd.Flags().IntVar(&setKeepLast, "keep-last", 0, "Keep only the newest N scheduled snapshots (0 = unlimited)")
	setCmd.Flags().StringVar(&setTTL, "ttl", "", "Prune scheduled snapshots older than this Go duration (empty = no age bound)")
	setCmd.Flags().BoolVar(&setDisabled, "disabled", false, "Create the policy disabled (no automatic snapshots)")
	setCmd.Flags().StringVar(&setNamePrefix, "name-prefix", "", "Name prefix for scheduler-created snapshots (default \"scheduled\")")
	_ = setCmd.MarkFlagRequired("interval")
}
