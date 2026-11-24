// Package installation implements the installation commands for dfsctl:
// the actions on the installation as a whole that no other noun owns, the
// signing key's rotation among them.
package installation

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

// New returns the installation command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "installation",
		Short: "Act on the installation as a whole",
	}
	rotate := &cobra.Command{
		Use:   "rotate-signing-key",
		Short: "Rotate the key access tokens are signed with",
		Long: `Rotate the installation's signing key, the key every node signs and verifies
access tokens with. The rotation is an operation: a new key signs from its
start, and the old one verifies the tokens it signed until they have expired,
about 15 minutes, when the operation ends. The command prints the operation's
ID, or, with --wait, follows it to its end.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			op, err := c.InstallationAction(cmd.Context(), "rotate-signing-key", nil)
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			return cmdutil.Follow(cmd, c, op)
		},
	}
	cmdutil.AddWait(rotate)
	cmd.AddCommand(rotate)
	return cmd
}
