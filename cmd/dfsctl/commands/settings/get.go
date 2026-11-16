package settings

import (
	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var getCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a setting value",
	Long: `Get the current value of a single server setting by its dot-separated key.

The default output prints key = value to stdout. Pass -o json or -o yaml to get a structured response including the setting description, useful for automation.

Examples:
  # Print the current logging level
  dfsctl settings get logging.level

  # Get the setting as JSON for scripting
  dfsctl settings get logging.level -o json`,
	Args: cobra.ExactArgs(1), RunE: cmdutil.Unavailable,
}
