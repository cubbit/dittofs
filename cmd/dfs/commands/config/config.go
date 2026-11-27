// Package config implements configuration management subcommands.
package config

import (
	"github.com/spf13/cobra"
)

// New returns the config subcommand.
func New() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configuration management",
		Long: `Manage the DittoFS server's configuration file.

Subcommands:
  edit      Open configuration in editor
  validate  Validate configuration file
  show      Display current configuration
  schema    Generate JSON schema for IDE/validation`,
	}
	cmd.AddCommand(newEditCmd(), newValidateCmd(), newShowCmd(), newSchemaCmd())
	return cmd
}
