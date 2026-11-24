// Package quota implements the quota commands for dfsctl: a share's quotas
// on its users and groups, and on itself.
package quota

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// quotaTable is quotas as a table.
type quotaTable []apiv1.Quota

// Headers implements output.TableRenderer.
func (quotaTable) Headers() []string {
	return []string{"ON", "HARD-BYTES", "HARD-FILES", "SOFT-BYTES", "SOFT-FILES", "GRACE", "ADVISORY-BYTES", "ADVISORY-FILES"}
}

// Rows implements output.TableRenderer.
func (qs quotaTable) Rows() [][]string {
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	rows := make([][]string, 0, len(qs))
	for _, q := range qs {
		s := q.Spec
		rows = append(rows, []string{q.ID, n(s.Hard.Bytes), n(s.Hard.Files), n(s.Soft.Bytes), n(s.Soft.Files),
			(time.Duration(s.GraceSeconds) * time.Second).String(), n(s.Advisory.Bytes), n(s.Advisory.Files)})
	}
	return rows
}

// New returns the quota command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Manage a share's quotas",
		Long: `Set, remove and list a share's quotas: on a user, on a group, or, with
neither named, on the share itself.

A quota is refused beyond its hard limit, allowed beyond its soft limit for
its grace and then refused, and never refused beyond its advisory limit,
which only raises an event. A limit of 0 limits nothing.

Examples:
  # Limit alice to 10 GiB and 100000 files on a share
  dfsctl quota set archive --user alice --hard-bytes 10737418240 --hard-files 100000

  # Limit the share itself, with a soft limit and a week's grace
  dfsctl quota set archive --soft-bytes 1099511627776 --grace 168h

  # List a share's quotas
  dfsctl quota list archive`,
	}
	cmd.AddCommand(newSetCmd(env), newRemoveCmd(env), newListCmd(env))
	return cmd
}

// quotas reads and writes quotas, a share's on its principals or on
// itself.
type quotas interface {
	Get(ctx context.Context, id string) (client.Tagged[apiv1.QuotaSpec, struct{}], error)
	Put(ctx context.Context, id string, spec apiv1.QuotaSpec, etag string) (client.Tagged[apiv1.QuotaSpec, struct{}], error)
}

// target is the quota cmd names on the share its arguments name: the
// quotas it is one of, and its ID, the principal's or, for the share's own,
// the share's.
func target(cmd *cobra.Command, c *client.Client, args []string) (quotas, string, error) {
	share, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
	if err != nil {
		return nil, "", err
	}
	p, err := cmdutil.Principal(cmd, c)
	switch {
	case err != nil:
		return nil, "", err
	case p == "":
		return c.ShareQuotas(), share, nil
	}
	return c.Quotas(share), p, nil
}

func newSetCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set [share]",
		Short: "Set a quota's limits",
		Long: `Set a quota's limits, creating the quota when there is none; a limit whose
flag is not given keeps its value.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkLimits(cmd); err != nil {
				return err
			}
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			q, id, err := target(cmd, c, args)
			if err != nil {
				return err
			}
			res, err := cmdutil.Set(cmd, env, q, cmdutil.ByPut(q), "quota", id, func(s *apiv1.QuotaSpec) error {
				f := cmd.Flags()
				for flag, into := range map[string]*int64{"hard-bytes": &s.Hard.Bytes, "hard-files": &s.Hard.Files,
					"soft-bytes": &s.Soft.Bytes, "soft-files": &s.Soft.Files,
					"advisory-bytes": &s.Advisory.Bytes, "advisory-files": &s.Advisory.Files} {
					if f.Changed(flag) {
						*into, _ = f.GetInt64(flag)
					}
				}
				if f.Changed("grace") {
					g, _ := f.GetDuration("grace")
					s.GraceSeconds = int64(g / time.Second)
				}
				return nil
			})
			if err != nil {
				return err
			}
			return cmdutil.PrintOutput(cmd, res.Resource.Resource, false, "", quotaTable{res.Resource.Resource})
		},
	}
	cmdutil.NameOrID(cmd, "share")
	cmdutil.PrincipalFlags(cmd, false)
	for _, l := range []string{"hard", "soft", "advisory"} {
		cmd.Flags().Int64(l+"-bytes", 0, "The "+l+" limit on bytes, 0 for none")
		cmd.Flags().Int64(l+"-files", 0, "The "+l+" limit on files, 0 for none")
	}
	cmd.Flags().Duration("grace", 0, "How long the soft limits may be passed before they are refused")
	cmd.MarkFlagsOneRequired("hard-bytes", "hard-files", "soft-bytes", "soft-files", "advisory-bytes", "advisory-files", "grace")
	return cmd
}

// checkLimits refuses a negative limit or grace, and a grace that is not
// whole seconds, which the quota would otherwise store truncated.
func checkLimits(cmd *cobra.Command) error {
	f := cmd.Flags()
	for _, flag := range []string{"hard-bytes", "hard-files", "soft-bytes", "soft-files", "advisory-bytes", "advisory-files"} {
		if v, _ := f.GetInt64(flag); v < 0 {
			return fmt.Errorf("%s: --%s %d: want 0 or more", cmd.CommandPath(), flag, v)
		}
	}
	if g, _ := f.GetDuration("grace"); g < 0 || g%time.Second != 0 {
		return fmt.Errorf("%s: --grace %s: want whole seconds, 0 or more", cmd.CommandPath(), g)
	}
	return nil
}

func newRemoveCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove [share]",
		Short: "Remove a quota",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			q, id, err := target(cmd, c, args)
			if err != nil {
				return err
			}
			// A quota whose limits are all zero limits nothing, and the
			// server deletes it.
			_, err = cmdutil.Update(cmd, env, q, cmdutil.ByPut(q), "quota", id, func(s *apiv1.QuotaSpec) error {
				*s = apiv1.QuotaSpec{}
				return nil
			})
			return err
		},
	}
	cmdutil.NameOrID(cmd, "share")
	cmdutil.PrincipalFlags(cmd, false)
	return cmd
}

func newListCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [share]",
		Short: "List a share's quotas, its own first",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			share, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			qs := quotaTable{}
			own, err := c.ShareQuotas().Get(cmd.Context(), share)
			switch {
			case err == nil:
				qs = append(qs, own.Resource)
			case !cmdutil.HasStatus(err, http.StatusNotFound):
				return fmt.Errorf("%s: share %s's own quota: %w", cmd.CommandPath(), share, err)
			}
			for q, err := range c.Quotas(share).List(cmd.Context(), nil) {
				if err != nil {
					return fmt.Errorf("%s: share %s's quotas: %w", cmd.CommandPath(), share, err)
				}
				qs = append(qs, q)
			}
			return cmdutil.PrintOutput(cmd, qs, len(qs) == 0, "No quotas.", qs)
		},
	}
	cmdutil.NameOrID(cmd, "share")
	return cmd
}
