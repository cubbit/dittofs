package permission

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newGrantCmd() *cobra.Command {
	grantCmd := &cobra.Command{
		Use:   "grant <share>",
		Short: "Grant permission on a share",
		Long: `Grant a permission level to a user or group on a share.

Specify exactly one of --user, --group, or --sid together with --level.
Re-running the command on a principal that already has a permission replaces the
existing level. Permission levels in order of increasing access:
  - none:       No access (explicitly blocks the principal)
  - read:       Read-only access
  - read-write: Read and write access
  - admin:      Full administrative access including ACL management

Active Directory principals can be granted directly, with no local DittoFS
account (issue #1528). --user / --group accept a local name, an AD name
(user@REALM or DOMAIN\group, resolved to a SID via the configured LDAP
directory), or a raw Windows SID. A bare name resolves to a local user/group if
one exists, otherwise to the directory. --sid grants to a raw SID explicitly.

Examples:
  # Grant read-write access to a local user
  dfsctl share permission grant /archive --user alice --level read-write

  # Grant read-only access to a local group
  dfsctl share permission grant /archive --group editors --level read

  # Grant directly to an AD group (resolved to its SID via LDAP) — no local group
  dfsctl share permission grant /archive --group 'CUBBIT\Cubbit' --level read-write

  # Grant directly to an AD user by Kerberos principal
  dfsctl share permission grant /archive --user alice@cubbit.local --level read

  # Grant to a raw Windows SID (no directory lookup)
  dfsctl share permission grant /archive --sid S-1-5-21-1111-2222-3333-1104 --level read`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	grantCmd.Flags().String("user", "", "User to grant permission to (local name, AD name, or SID)")
	grantCmd.Flags().String("group", "", "Group to grant permission to (local name, AD name, or SID)")
	grantCmd.Flags().String("sid", "", "Raw Windows SID to grant permission to (e.g. S-1-5-21-...)")
	grantCmd.Flags().String("level", "", "Permission level (none|read|read-write|admin)")
	_ = grantCmd.MarkFlagRequired("level")
	return grantCmd
}
