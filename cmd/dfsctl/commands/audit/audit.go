// Package audit implements the audit command for dfsctl: the record of
// every management action, applied or refused.
package audit

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// table is audit records as a table.
func table(rs ...apiv1.AuditRecord) cmdutil.Table {
	t := cmdutil.Table{{"TIME", "ACTOR", "TARGET", "OUTCOME"}}
	for _, r := range rs {
		actor := r.ActorName
		if actor == "" {
			actor = r.Actor.Kind + ":" + r.Actor.ID
		}
		t = append(t, []string{r.Time.Format(time.RFC3339), actor, r.Target.Kind + ":" + r.Target.ID, r.Outcome})
	}
	return t
}

// New returns the audit command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Read the audit of management actions",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List audit records",
		Long: `List the audit records the filters admit, oldest first: who acted, on what,
and whether the action was applied, refused or started an operation.

--actor names a user by name, or any account as kind:id, or the schedule as
schedule; --target names a resource as kind:id; --from and --to are RFC 3339
times.

Examples:
  # What alice changed today
  dfsctl audit list --actor alice --from 2026-11-24T00:00:00Z

  # Every action on one share
  dfsctl audit list --target share:0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			filter := url.Values{}
			for _, f := range []string{"from", "to", "target", "actor"} {
				if v, _ := cmd.Flags().GetString(f); v != "" {
					filter.Set(f, v)
				}
			}
			// A user's name, which holds no ':' and is not the schedule,
			// is resolved to the user as kind:id.
			if a := filter.Get("actor"); a != "" && a != apiv1.ScheduleKind && !strings.Contains(a, ":") {
				id, err := cmdutil.Resolve(cmd, c.Users().Resource, "user", []string{a})
				if err != nil {
					return err
				}
				filter.Set("actor", "user:"+id)
			}
			var all []apiv1.AuditRecord
			for r, err := range c.Audit(cmd.Context(), filter) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				all = append(all, r)
			}
			return cmdutil.PrintOutput(cmd, all, len(all) == 0, "No audit records.", table(all...))
		},
	}
	list.Flags().String("actor", "", "Only the records of this account: a user's name, kind:id or schedule")
	list.Flags().String("target", "", "Only the records of this resource, as kind:id")
	list.Flags().String("from", "", "Only the records at or after this RFC 3339 time")
	list.Flags().String("to", "", "Only the records before this RFC 3339 time")
	cmd.AddCommand(list)
	return cmd
}
