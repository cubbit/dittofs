package commands

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

func newStatusCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the node's readiness and the installation's health",
		Long: `Show the readiness of the node the current context names, ready, starting,
draining or not_ready, and every health condition it holds, its own and the
installation's. Use -o json or -o yaml for machine-readable output.

Examples:
  # Show the status of the current context's node
  dfsctl status

  # Emit machine-readable JSON output
  dfsctl status -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			h, err := c.Health(cmd.Context())
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			t := cmdutil.Table{{"READY", "CONDITION", "SCOPE", "SUBJECT", "CAUSE", "DETAIL"}}
			if len(h.Conditions) == 0 {
				t = append(t, []string{h.Ready, "", "", "", "", ""})
			}
			for _, cd := range h.Conditions {
				subject := cd.Subject
				if subject == "" {
					subject = cd.ID
				}
				t = append(t, []string{h.Ready, cd.Name, cd.Scope, subject, cd.Cause, cd.Detail})
			}
			return cmdutil.PrintOutput(cmd, h, false, "", t)
		},
	}
}
