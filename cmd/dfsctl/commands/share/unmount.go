package share

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var unmountForce bool

var unmountCmd = &cobra.Command{
	Use:   "unmount [mountpoint]",
	Short: "Unmount a mounted share",
	Long: `Unmount a DittoFS share from a local mount point.

Examples:
  # Unmount a share (positional argument is the mount point path)
  dfsctl share unmount /mnt/dittofs

  # Force unmount if busy
  dfsctl share unmount --force /mnt/dittofs

  # Windows: unmount a mapped drive
  dfsctl share unmount Z:

Note: Unmount commands typically require sudo/root privileges on Unix systems.
Unmount identifies the target by mount-point path rather than share name
because a single share can be mounted to multiple local paths; the
` + "`share <name> <verb>`" + ` shape therefore does not extend to unmount.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			return fmt.Errorf("requires mount point path\n\nUsage: dfsctl share unmount [mountpoint]\n\nExample: dfsctl share unmount /mnt/dittofs")
		}
		if len(args) > 1 {
			return fmt.Errorf("accepts 1 arg, received %d", len(args))
		}
		return nil
	}, RunE: cmdutil.Unavailable,
}

func init() {
	unmountCmd.Flags().BoolVarP(&unmountForce, "force", "f", false, "Force unmount even if busy")
}
