package share

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	mountProtocol   string
	mountUsername   string
	mountPassword   string
	mountFileMode   string
	mountDirMode    string
	mountNFSVersion string
)

var mountCmd = &cobra.Command{
	Use:   "mount [share] [mountpoint]",
	Short: "Mount a share via NFS or SMB",
	Long: `Mount a DittoFS share at a local mount point using NFS or SMB protocol.

For SMB mounts, credentials are resolved in order:

1. --username/--password flags
2. DITTOFS_PASSWORD environment variable (for password)
3. Current login context username
4. Interactive password prompt

Mount commands typically require sudo/root privileges on Unix systems.

Platform differences for SMB with sudo:

- Linux: mount owner set to your user via uid/gid options (default mode 0755)
- macOS: mount owned by root (uid/gid removed in Catalina), default mode 0777
- macOS alternative: mount to ~/mnt without sudo for a user-owned mount
- Windows: uses 'net use' to map network drives (e.g. dfsctl share mount /export --protocol smb Z:)

Examples:
  # Mount via NFS
  dfsctl share mount /export --protocol nfs /mnt/dittofs

  # Mount via SMB
  dfsctl share mount /export --protocol smb /mnt/dittofs

  # Mount via SMB with explicit credentials
  dfsctl share mount /export --protocol smb --username alice /mnt/dittofs

  # Mount via SMB with password from environment
  DITTOFS_PASSWORD=secret dfsctl share mount /export --protocol smb /mnt/dittofs

  # Mount to user directory without sudo (macOS only, recommended)
  mkdir -p ~/mnt/dittofs && dfsctl share mount /export --protocol smb ~/mnt/dittofs`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("requires share path and mount point\n\nUsage: dfsctl share mount [share] [mountpoint] --protocol <nfs|smb>\n\nExample: dfsctl share mount --protocol nfs /export /mnt/dittofs")
		}
		if len(args) > 2 {
			return fmt.Errorf("accepts 2 args, received %d", len(args))
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("file-mode") {
			mountFileMode = defaultMode()
		}
		if !cmd.Flags().Changed("dir-mode") {
			mountDirMode = defaultMode()
		}
		return cmdutil.Unavailable(cmd, args)
	},
}

func init() {
	mountCmd.Flags().StringVarP(&mountProtocol, "protocol", "p", "", "Protocol to use (nfs or smb) (required)")
	mountCmd.Flags().StringVarP(&mountUsername, "username", "u", "", "Username for SMB mount (defaults to login username)")
	mountCmd.Flags().StringVarP(&mountPassword, "password", "P", "", "Password for SMB mount (will prompt if not provided)")
	mountCmd.Flags().StringVar(&mountNFSVersion, "nfs-version", "3", "NFS protocol version for NFS mounts (3, 4, 4.0, 4.1, 4.2). v4 carries locking in-protocol; v3 locking needs the server UDP transport + portmapper")
	mountCmd.Flags().StringVar(&mountFileMode, "file-mode", "", "File permissions for SMB mount (octal; default 0755 on Linux, 0777 on macOS)")
	mountCmd.Flags().StringVar(&mountDirMode, "dir-mode", "", "Directory permissions for SMB mount (octal; default 0755 on Linux, 0777 on macOS)")

	_ = mountCmd.MarkFlagRequired("protocol")
}
