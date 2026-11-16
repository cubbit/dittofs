package share

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

// newNfsConfigCmd returns the parent for per-share NFS adapter config.
func newNfsConfigCmd() *cobra.Command {
	nfsConfigCmd := &cobra.Command{
		Use:   "nfs-config",
		Short: "Manage per-share NFS adapter configuration",
		Long: `View and update the NFS adapter configuration for a share.

This controls the share's export settings: the squash mode, the
authentication flags and the netgroup that restricts which clients may reach
the share. The authentication flags and the netgroup are the share's access
policy, which applies over NFS and SMB alike; the squash mode maps the
identities of SMB sessions too, guest sessions excepted. Changes take effect
immediately.

Examples:
  # Show a share's NFS config
  dfsctl share nfs-config show /export

  # Associate a netgroup with the share's NFS export
  dfsctl share nfs-config set /export --netgroup office-network

  # Remove the netgroup association (allow all clients)
  dfsctl share nfs-config set /export --netgroup ""`,
	}
	nfsConfigCmd.AddCommand(newNfsConfigShowCmd())
	nfsConfigCmd.AddCommand(newNfsConfigSetCmd())
	return nfsConfigCmd
}

func newNfsConfigShowCmd() *cobra.Command {
	nfsConfigShowCmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a share's NFS adapter configuration",
		Long: `Show the NFS adapter configuration for a share.

Displays the netgroup association, squash mode, and authentication settings
(AUTH_SYS and Kerberos). Use this command to inspect the current NFS export
settings before making changes with 'nfs-config set'.

Examples:
  # Show NFS config for a share
  dfsctl share nfs-config show /export

  # Emit as JSON
  dfsctl share nfs-config show /export -o json`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	return nfsConfigShowCmd
}

func newNfsConfigSetCmd() *cobra.Command {
	nfsConfigSetCmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Update a share's NFS adapter configuration",
		Long: `Update the NFS adapter configuration for a share.

Only the flags you supply are changed; omitted flags leave the existing values
intact. Changes take effect immediately, without an adapter restart.

The netgroup allowlist and the authentication flags are the share's access
policy: they are enforced on NFSv3 mounts, on NFSv4 operations and on SMB
requests to the same share. The squash mode is not NFS-only either: it maps the
identities of SMB sessions on the share as well, guest sessions excepted.

Examples:
  # Restrict access to a specific netgroup
  dfsctl share nfs-config set /export --netgroup office-network

  # Remove the netgroup restriction (allow all clients)
  dfsctl share nfs-config set /export --netgroup ""

  # Map root UID to guest on this export
  dfsctl share nfs-config set /export --squash root_to_guest

  # Require Kerberos authentication and disallow AUTH_SYS
  dfsctl share nfs-config set /export --require-kerberos true --allow-auth-sys false`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	nfsConfigSetCmd.Flags().String("netgroup", "", "Netgroup name to associate (empty string clears the association)")
	nfsConfigSetCmd.Flags().String("squash", "", "Squash mode (none|root_to_admin|root_to_guest|all_to_admin|all_to_guest)")
	nfsConfigSetCmd.Flags().String("allow-auth-sys", "", "Allow AUTH_SYS flavor (true|false)")
	nfsConfigSetCmd.Flags().String("require-kerberos", "", "Require Kerberos auth (true|false)")
	return nfsConfigSetCmd
}
