package group

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newCreateCmd() *cobra.Command {
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new group",
		Long: `Create a new group on the DittoFS server. Groups are used to organise
users and can be referenced in share permissions to grant access to multiple
users at once. The group's Unix GID is auto-assigned from the server's
allocation range unless you provide one explicitly with --gid.

Examples:
  # Create a group with an auto-assigned GID
  dfsctl group create --name editors

  # Create a group with an explicit GID
  dfsctl group create --name editors --gid 1001

  # Create a group with a description
  dfsctl group create --name editors --gid 1001 --description "Content editors"`, RunE: cmdutil.Unavailable,
	}
	createCmd.Flags().String("name", "", "Group name (required)")
	createCmd.Flags().Uint32("gid", 0, "Group ID (auto-generated if not set)")
	createCmd.Flags().String("description", "", "Group description")
	_ = createCmd.MarkFlagRequired("name")
	return createCmd
}
