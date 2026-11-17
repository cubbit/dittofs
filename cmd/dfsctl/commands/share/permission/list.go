package permission

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	listCmd := &cobra.Command{
		Use:   "list <share>",
		Short: "List permissions on a share",
		Long: `List all user and group permissions configured on a share.

Each row shows the principal type (user or group), the principal's name, and
the permission level (none, read, read-write, or admin). Note that these are
per-principal overrides; clients without an explicit entry fall back to the
share's default permission (see 'dfsctl share show').

Examples:
  # List all permissions on a share
  dfsctl share permission list /archive

  # Emit permissions as JSON for scripting
  dfsctl share permission list /archive -o json

  # Emit as YAML
  dfsctl share permission list /archive -o yaml`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	return listCmd
}
