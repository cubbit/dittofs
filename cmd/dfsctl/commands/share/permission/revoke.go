package permission

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newRevokeCmd() *cobra.Command {
	revokeCmd := &cobra.Command{
		Use:   "revoke <share>",
		Short: "Revoke permission from a share",
		Long: `Remove a per-principal permission entry from a share.

After revoking, the user or group falls back to the share's default permission
level (see 'dfsctl share show'). To explicitly block a principal rather than
fall back to the default, use 'dfsctl share permission grant ... --level none'
instead. Specify exactly one of --user, --group, or --sid.

--user / --group accept a local name, an AD name, or a raw SID (matching the
grant command); --sid revokes a raw SID grant directly.

Examples:
  # Revoke a user's explicit permission (they fall back to the share default)
  dfsctl share permission revoke /archive --user alice

  # Revoke a group's explicit permission
  dfsctl share permission revoke /archive --group editors

  # Revoke a direct AD/SID grant
  dfsctl share permission revoke /archive --sid S-1-5-21-1111-2222-3333-1104`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	revokeCmd.Flags().String("user", "", "User to revoke permission from (local name, AD name, or SID)")
	revokeCmd.Flags().String("group", "", "Group to revoke permission from (local name, AD name, or SID)")
	revokeCmd.Flags().String("sid", "", "Raw Windows SID to revoke permission from")
	return revokeCmd
}
