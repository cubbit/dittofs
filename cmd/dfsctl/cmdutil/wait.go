package cmdutil

import (
	"errors"
	"fmt"
	"net/http"
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
// Wait follows it to its end. A nil op, a write that finished in its call,
// prints nothing.
func Follow(cmd *cobra.Command, c *client.Client, op *apiv1.Operation) error {
	if op == nil {
		return nil
	}
	if wait, _ := cmd.Flags().GetBool("wait"); wait {
		return Wait(cmd, c, *op)
	}
	return PrintOutput(cmd, op, false, "", Operations{*op})
}

// WaitFor reads the operation id names and follows it to its end as Wait
// does, its read failing as a poll of Wait's does.
func WaitFor(cmd *cobra.Command, c *client.Client, id string) error {
	op, err := c.Operation(cmd.Context(), id)
	if err != nil {
		return waitFailed(cmd, id, err, false)
	}
	return Wait(cmd, c, op)
}

// Wait follows op to its end with ?wait= and prints it as it ended,
// refusing one that ended failed or cancelled. A wait that ends first, cmd's
// context ending or a poll failing in a way a later poll may not, leaves
// the operation running and is refused naming the command that resumes it,
// from this machine or another; one that a resumed wait would meet again is
// refused as it is.
func Wait(cmd *cobra.Command, c *client.Client, op apiv1.Operation) error {
	path := cmd.CommandPath()
	ended, err := c.Wait(cmd.Context(), op)
	if err != nil {
		return waitFailed(cmd, op.ID, err, true)
	}
	if err := PrintOutput(cmd, ended, false, "", Operations{ended}); err != nil {
		return err
	}
	return endedErr(path, ended)
}

// endedErr refuses ended, an operation followed to its end, unless it
// succeeded, prefixed with path.
func endedErr(path string, ended apiv1.Operation) error {
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

// waitFailed is the refusal of a wait for the operation id names that err
// ended, cmd's context's error in its place once that context has ended and
// err is not a refusal a resumed wait would meet again. Only when resumable
// holds does it carry the hint that resumes the wait, and it says the
// operation goes on only when read, a read of it having answered.
func waitFailed(cmd *cobra.Command, id string, err error, read bool) error {
	if ce := cmd.Context().Err(); ce != nil && resumable(err) {
		err = ce
	}
	path := cmd.CommandPath()
	switch {
	case !resumable(err):
		return fmt.Errorf("%s: waiting for operation %s: %w", path, id, err)
	case !read:
		return fmt.Errorf("%s: waiting for operation %s: %w; 'dfsctl operation wait %s' tries again", path, id, err, id)
	}
	return fmt.Errorf("%s: waiting for operation %s: %w; it goes on, and 'dfsctl operation wait %s' resumes the wait", path, id, err, id)
}

// resumable reports whether a resumed wait may get past err: not past a
// 4xx refusal of the request, such as the operation gone or the caller
// refused, nor past an operation in a state this client does not know; past
// anything else, a 5xx such as a node restarting, a 408, a 429 and any
// refusal the server marks retryable among them.
func resumable(err error) bool {
	if errors.Is(err, client.ErrUnknownState) {
		return false
	}
	p := (*apiv1.Problem)(nil)
	if !errors.As(err, &p) {
		return true
	}
	return p.Status >= 500 || p.Retryable || p.Status == http.StatusRequestTimeout || p.Status == http.StatusTooManyRequests
}
