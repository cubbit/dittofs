// Package operation implements the operation commands for dfsctl: reading,
// listing, waiting for and cancelling the long-running work a command
// starts.
package operation

import (
	"fmt"
	"net/url"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// New returns the operation command. An operation has no name, so each
// command takes its ID as its argument.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "operation",
		Short: "Follow long-running operations",
		Long: `Read, list, wait for and cancel operations.

A command whose work outlasts its call, such as a deletion, starts an operation
and prints its ID; with --wait it follows the operation to its end. A wait that
is interrupted leaves the operation running, and 'dfsctl operation wait <id>'
resumes it, from this machine or any other signed in to the same server.

Examples:
  # List the operations running
  dfsctl operation list --state running

  # Wait for an operation to end
  dfsctl operation wait 1a92dcf1-bdbb-49eb-acc1-a78ef6b4e5f1`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "get <id>",
			Short: "Show an operation",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, _, err := cmdutil.NewClient(env)
				if err != nil {
					return err
				}
				op, err := c.Operation(cmd.Context(), args[0])
				if err != nil {
					return fmt.Errorf("%s: operation %s: %w", cmd.CommandPath(), args[0], err)
				}
				return cmdutil.PrintOutput(cmd, op, false, "", cmdutil.Operations{op})
			},
		},
		newListCmd(env),
		&cobra.Command{
			Use:   "wait <id>",
			Short: "Wait for an operation to end",
			Long: `Wait for an operation to end and print it as it ended.

The command exits non-zero when the operation failed or was cancelled, and
when the wait itself is interrupted, which leaves the operation running.`,
			Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, _, err := cmdutil.NewClient(env)
				if err != nil {
					return err
				}
				op, err := c.Operation(cmd.Context(), args[0])
				if err != nil {
					return fmt.Errorf("%s: operation %s: %w", cmd.CommandPath(), args[0], err)
				}
				return cmdutil.Wait(cmd, c, op)
			},
		},
		&cobra.Command{
			Use:   "cancel <id>",
			Short: "Cancel an operation",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, _, err := cmdutil.NewClient(env)
				if err != nil {
					return err
				}
				if err := c.CancelOperation(cmd.Context(), args[0]); err != nil {
					return fmt.Errorf("%s: operation %s: %w", cmd.CommandPath(), args[0], err)
				}
				return nil
			},
		},
	)
	return cmd
}

func newListCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List operations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			filter := url.Values{}
			if s, _ := cmd.Flags().GetString("state"); s != "" {
				filter.Set("state", s)
			}
			ops := cmdutil.Operations{}
			for op, err := range c.Operations(cmd.Context(), filter) {
				if err != nil {
					return err
				}
				ops = append(ops, op)
			}
			return cmdutil.PrintOutput(cmd, []apiv1.Operation(ops), len(ops) == 0, "No operations.", ops)
		},
	}
	cmd.Flags().String("state", "", "Only the operations not ended in this state (pending|running|cancelling)")
	return cmd
}
