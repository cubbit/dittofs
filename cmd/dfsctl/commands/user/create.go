package user

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var (
	createUsername string
	createPassword string
	createEmail    string
	createRole     string
	createUID      uint32
	createHostUID  bool
	createGID      uint32
	createHostGID  bool
	createGroups   string
	createEnabled  bool
)

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new user",
	Long: `Create a new local user on the DittoFS server. The user can be assigned
a Unix UID and primary GID for NFS uid/gid resolution; omitting these causes
the server to auto-assign them from its allocation range. If --username or
--password are not provided as flags, you will be prompted interactively.

Examples:
  # Create a user interactively (prompted for username, password, role, groups)
  dfsctl user create

  # Create a regular user with username and password
  dfsctl user create --username alice --password secret

  # Create an admin user belonging to the editors group
  dfsctl user create --username admin2 --password secret --role admin --groups editors

  # Create a user whose UID/GID match the current host user (useful for NFS mounts)
  dfsctl user create --username alice --password secret --host-uid --host-gid

  # Create a user with an explicit UID, GID, and email
  dfsctl user create --username bob --password secret --uid 1000 --gid 1001 --email bob@example.com`, RunE: cmdutil.Unavailable,
}

func init() {
	createCmd.Flags().StringVarP(&createUsername, "username", "u", "", "Username (required)")
	createCmd.Flags().StringVarP(&createPassword, "password", "p", "", "Password (prompts if not provided)")
	createCmd.Flags().StringVar(&createEmail, "email", "", "Email address")
	createCmd.Flags().StringVar(&createRole, "role", "user", "Role (user|admin)")
	createCmd.Flags().Uint32Var(&createUID, "uid", 0, "Unix user ID (auto-assigned if not specified)")
	createCmd.Flags().BoolVar(&createHostUID, "host-uid", false, "Use current host user's UID (for NFS access)")
	createCmd.Flags().Uint32Var(&createGID, "gid", 0, "Unix primary group ID (auto-assigned if not specified)")
	createCmd.Flags().BoolVar(&createHostGID, "host-gid", false, "Use current host user's GID (for NFS access)")
	createCmd.Flags().StringVar(&createGroups, "groups", "", "Comma-separated list of groups")
	// MarkFlagsMutuallyExclusive panics if flag names don't exist (see Cobra source)
	createCmd.MarkFlagsMutuallyExclusive("uid", "host-uid")
	createCmd.MarkFlagsMutuallyExclusive("gid", "host-gid")
	createCmd.Flags().BoolVar(&createEnabled, "enabled", true, "Enable account")
}
