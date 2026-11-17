package commands

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newResetAdminCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset-admin",
		Short: "Reset the administrator's credentials",
		Long: `Reset the credentials of the installation's administrator, on the host
that holds its metadata store, for when no administrator can sign in.

Examples:
  # Reset the administrator of the installation the default configuration names
  dfs reset-admin`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("resetting the administrator: %w", errors.ErrUnsupported)
		},
	}
}
