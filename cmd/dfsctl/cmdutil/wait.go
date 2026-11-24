package cmdutil

import (
	"fmt"
	"time"

	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// Operations is operations as a table.
type Operations []apiv1.Operation

// Headers implements output.TableRenderer.
func (Operations) Headers() []string { return []string{"ID", "KIND", "TARGET", "STATE", "UPDATED"} }

// Rows implements output.TableRenderer.
func (ops Operations) Rows() [][]string {
	rows := make([][]string, 0, len(ops))
	for _, op := range ops {
		rows = append(rows, []string{op.ID, string(op.Kind), op.Target.Kind + "/" + op.Target.ID, string(op.State), op.Updated.Format(time.RFC3339)})
	}
	return rows
}

// AddWait gives cmd, which may start an operation, the --wait flag.
func AddWait(cmd *cobra.Command) {
	cmd.Flags().Bool("wait", false, "Follow the operation the command starts to its end")
}

// Follow prints op, an operation cmd started, at once, or, with --wait, as
// Wait follows it to its end.
func Follow(cmd *cobra.Command, c *client.Client, op apiv1.Operation) error {
	if wait, _ := cmd.Flags().GetBool("wait"); wait {
		return Wait(cmd, c, op)
	}
	return PrintOutput(cmd, op, false, "", Operations{op})
}

// Wait follows op to its end with ?wait= and prints it as it ended,
// refusing one that ended failed or cancelled. A wait that ends first, cmd's
// context ending or a poll failing, leaves the operation running and is
// refused naming the command that resumes it, from this machine or another.
func Wait(cmd *cobra.Command, c *client.Client, op apiv1.Operation) error {
	path := cmd.CommandPath()
	ended, err := c.Wait(cmd.Context(), op)
	if err != nil {
		if cmd.Context().Err() != nil {
			err = cmd.Context().Err()
		}
		return fmt.Errorf("%s: waiting for operation %s: %w; it goes on, and 'dfsctl operation wait %s' resumes the wait", path, op.ID, err, op.ID)
	}
	if err := PrintOutput(cmd, ended, false, "", Operations{ended}); err != nil {
		return err
	}
	switch ended.State {
	case apiv1.OpFailed:
		if ended.Error != nil {
			return fmt.Errorf("%s: operation %s failed: %w", path, ended.ID, ended.Error)
		}
		return fmt.Errorf("%s: operation %s failed", path, ended.ID)
	case apiv1.OpCancelled:
		return fmt.Errorf("%s: operation %s was cancelled", path, ended.ID)
	}
	return nil
}
