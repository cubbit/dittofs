// Package commands implements the CLI commands for dittofs server management.
package commands

import (
	"github.com/cubbit/dittofs/cmd/dfs/commands/config"
	"github.com/spf13/cobra"
)

// Version, Commit and Date describe the build; main sets them from the values
// -ldflags gives it, before building a root.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// NewRoot builds dfs's command tree for one invocation. Its flags live in the
// tree, so the values one run parses never reach a tree built for another.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "dfs",
		Short: "DittoFS - Modular virtual filesystem",
		Long: `DittoFS is an experimental modular virtual filesystem that decouples
file interfaces from storage backends, with pluggable metadata and content
stores. This build serves no file protocol.

Use "dfs [command] --help" for more information about a command.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("config", "", "config file (default: $XDG_CONFIG_HOME/dittofs/config.yaml)")

	root.AddCommand(
		newVersionCmd(),
		newInitCmd(),
		newStartCmd(),
		newResetAdminCmd(),
		config.New(),
		newCompletionCmd(),
	)

	// Hide the default completion command (we provide our own)
	root.CompletionOptions.DisableDefaultCmd = true
	return root
}

// Execute runs one dfs invocation with the process's arguments.
func Execute() error {
	return NewRoot().Execute()
}
