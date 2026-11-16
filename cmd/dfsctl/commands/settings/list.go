package settings

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all settings",
	Long: `List all available server settings and their current values.

The table shows each setting's key, current value, and a human-readable description. Use this command to discover what can be tuned with 'settings set'. Pass -o json to get the full list in machine-readable form.

Examples:
  # List all settings as a table
  dfsctl settings list

  # Dump all settings as JSON for scripting
  dfsctl settings list -o json`, RunE: cmdutil.Unavailable,
}
