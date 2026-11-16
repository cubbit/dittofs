package group

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var removeForce bool

var removeCmd = &cobra.Command{
	Use:   "remove <name|id>",
	Short: "Remove a group",
	Long: `Remove a group from the DittoFS server. Accepts either the group name
or the group's full ID (see 'dfsctl group list'). This action is irreversible:
the group record is permanently removed and any users that had it as their
primary group will lose that association. You will be prompted for
confirmation unless --force is specified.

Examples:
  # Remove a group (prompts for confirmation)
  dfsctl group remove editors

  # Remove a group non-interactively (for scripts and automation)
  dfsctl group remove editors --force`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}

func init() {
	removeCmd.Flags().BoolVarP(&removeForce, "force", "f", false, "Skip confirmation prompt")
}
