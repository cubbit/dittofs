package share

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newShowCmd() *cobra.Command {
	showCmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show share details",
		Long: `Show a detailed, field-by-field view of a single share.

Unlike 'share list', which shows summary columns, 'share show' displays every
attribute of the share: store IDs, read-only state, ACL settings, retention
policy and TTL, cache size overrides, quota, and creation/update timestamps. Use this command when debugging a misconfigured
share or before editing it.

Examples:
  # Show all fields for a share
  dfsctl share show /archive

  # Emit the full share record as JSON (useful for scripting or diffing)
  dfsctl share show /archive -o json

  # Emit as YAML
  dfsctl share show /archive -o yaml`,
		Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
	}
	return showCmd
}
