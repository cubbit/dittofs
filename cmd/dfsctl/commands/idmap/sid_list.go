package idmap

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var sidListCmd = &cobra.Command{
	Use:   "list",
	Short: "List foreign-SID UID/GID allocations",
	Long: `List all durable foreign-SID to Unix UID/GID allocations on the DittoFS
server. Each row shows the Windows SID, whether it represents a user or group,
the allocated Unix ID, and a display name if one was resolved at allocation time.
Use this command to audit which AD/LDAP principals have been seen by the server
and what Unix IDs they were assigned.

Examples:
  # List all foreign-SID allocations as a table
  dfsctl idmap sid list

  # Output as JSON (includes full SID details)
  dfsctl idmap sid list -o json

  # Output as YAML
  dfsctl idmap sid list -o yaml`, RunE: cmdutil.Unavailable,
}
