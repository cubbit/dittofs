// Package grant implements the grant commands for dfsctl: a principal's
// access to a share.
package grant

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// Grants is grants as a table.
type Grants []apiv1.Grant

// Headers implements output.TableRenderer.
func (Grants) Headers() []string { return []string{"PRINCIPAL", "ACCESS"} }

// Rows implements output.TableRenderer.
func (gs Grants) Rows() [][]string {
	rows := make([][]string, 0, len(gs))
	for _, g := range gs {
		rows = append(rows, []string{g.ID, g.Spec.Access})
	}
	return rows
}

// New returns the grant command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Manage principals' access to shares",
		Long: `Set, remove and list grants: a user's, a group's or a directory principal's
access to a share, none, read, read-write or admin.

A grant of none is kept and refuses the principal; removing a grant lets the
principal's groups' grants decide again. A directory principal is named by
its qualified name, DOMAIN\name, which the server first resolves.

Examples:
  # Let alice read and write a share
  dfsctl grant set archive --user alice --access read-write

  # Let a directory user read it
  dfsctl grant set archive --directory 'CORP\bob' --access read

  # List a share's grants
  dfsctl grant list archive`,
	}
	set := &cobra.Command{
		Use:   "set [share]",
		Short: "Set a principal's access to a share",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			share, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			p, err := cmdutil.Principal(cmd, c)
			if err != nil {
				return err
			}
			access, _ := cmd.Flags().GetString("access")
			grants := c.Grants(share)
			res, err := cmdutil.Set(cmd, env, grants, cmdutil.ByPut(grants), "grant", p, func(g *apiv1.GrantSpec) error {
				g.Access = access
				return nil
			})
			if err != nil {
				return err
			}
			return cmdutil.PrintOutput(cmd, res.Resource.Resource, false, "", Grants{res.Resource.Resource})
		},
	}
	cmdutil.NameOrID(set, "share")
	cmdutil.PrincipalFlags(set, true)
	set.MarkFlagsOneRequired("user", "group", "directory")
	set.Flags().String("access", "", "The access: none, read, read-write or admin")
	_ = set.MarkFlagRequired("access")

	remove := &cobra.Command{
		Use:   "remove [share]",
		Short: "Remove a principal's grant on a share",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			share, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			p, err := cmdutil.Principal(cmd, c)
			if err != nil {
				return err
			}
			grants := c.Grants(share)
			g, err := grants.Get(cmd.Context(), p)
			if err != nil {
				return fmt.Errorf("%s: grant of %s: %w", cmd.CommandPath(), p, err)
			}
			if _, err := grants.Delete(cmd.Context(), p, g.ETag); err != nil {
				return fmt.Errorf("%s: grant of %s: %w", cmd.CommandPath(), p, err)
			}
			return nil
		},
	}
	cmdutil.NameOrID(remove, "share")
	cmdutil.PrincipalFlags(remove, true)
	remove.MarkFlagsOneRequired("user", "group", "directory")

	list := &cobra.Command{
		Use:   "list [share]",
		Short: "List a share's grants",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			share, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			gs := Grants{}
			for g, err := range c.Grants(share).List(cmd.Context(), nil) {
				if err != nil {
					return fmt.Errorf("%s: share %s's grants: %w", cmd.CommandPath(), share, err)
				}
				gs = append(gs, g)
			}
			return cmdutil.PrintOutput(cmd, gs, len(gs) == 0, "No grants.", gs)
		},
	}
	cmdutil.NameOrID(list, "share")
	cmd.AddCommand(set, remove, list)
	return cmd
}
