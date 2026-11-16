// Package quota implements per-identity quota management commands for dfsctl.
package quota

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for per-identity quota management.
var Cmd = &cobra.Command{
	Use:   "quota",
	Short: "Per-identity quota management",
	Long: `Manage per-identity (user/group/default-user) storage quotas on a share.

Quotas bound both bytes and inode (file) count, with optional soft thresholds
and a grace period before a soft threshold is enforced as hard. These operations
require admin privileges.

Examples:
  # List all quotas on a share
  dfsctl quota list /archive

  # Set a per-user quota (uid 1000)
  dfsctl quota set /archive --scope user --id 1000 --limit-bytes 10GiB --limit-files 100000

  # Set the default-user fallback quota (applies to users without an explicit quota)
  dfsctl quota set /archive --scope default-user --limit-bytes 1GiB

  # Set a per-group quota with soft thresholds and a grace period
  dfsctl quota set /archive --scope group --id 2000 --limit-bytes 50GiB --soft-bytes 45GiB --grace-seconds 604800

  # Remove a per-user quota
  dfsctl quota remove /archive --scope user --id 1000`,
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(setCmd)
	Cmd.AddCommand(removeCmd)
}
