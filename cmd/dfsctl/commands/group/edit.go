package group

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newEditCmd() *cobra.Command {
	editCmd := &cobra.Command{
		Use:   "edit <name>",
		Short: "Edit a group",
		Long: `Edit an existing group on the DittoFS server. When run without flags, an
interactive prompt walks you through each editable field (GID and description),
showing the current value so you can press Enter to keep it. When flags are
provided, only those fields are updated and no prompt appears.

Examples:
  # Edit the group interactively (shows current values)
  dfsctl group edit editors

  # Change the group's GID to a specific value
  dfsctl group edit editors --gid 1002

  # Update only the description
  dfsctl group edit editors --description "Senior content editors"

  # Update both GID and description in one command
  dfsctl group edit editors --gid 1002 --description "Senior content editors"`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	editCmd.Flags().Uint32("gid", 0, "Group ID")
	editCmd.Flags().String("description", "", "Group description")
	return editCmd
}
