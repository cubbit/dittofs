// Package netgroup implements the netgroup commands for dfsctl: the named
// sets of hosts an export policy admits, created, read, replaced and
// deleted through the management API.
package netgroup

import (
	"fmt"
	"strings"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// Netgroups is netgroups as a table.
type Netgroups []apiv1.Netgroup

// Headers implements output.TableRenderer.
func (Netgroups) Headers() []string { return []string{"NAME", "MEMBERS"} }

// Rows implements output.TableRenderer.
func (ns Netgroups) Rows() [][]string {
	rows := make([][]string, 0, len(ns))
	for _, n := range ns {
		rows = append(rows, []string{n.ID, strings.Join(n.Spec.Members, ",")})
	}
	return rows
}

// New returns the netgroup command. A netgroup's name is its ID, so each
// command takes the name alone.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "netgroup",
		Short: "Manage netgroups",
		Long: `Create, list, read, change and delete netgroups: named sets of host names,
addresses and CIDRs that an export policy admits.

An update replaces the netgroup's members with those --member names, and
needs at least one.

Examples:
  # Create a netgroup of two hosts and a subnet
  dfsctl netgroup create builders --member build1.example.com --member 10.0.0.7 --member 10.1.0.0/16

  # Replace its members
  dfsctl netgroup update builders --member 10.1.0.0/16`,
	}
	cmd.AddCommand(newCreateCmd(env), newListCmd(env), newGetCmd(env), newUpdateCmd(env), newDeleteCmd(env))
	return cmd
}

// members is the members cmd's --member flags name.
func members(cmd *cobra.Command) []string {
	m, _ := cmd.Flags().GetStringArray("member")
	if m == nil {
		m = []string{}
	}
	return m
}

func newCreateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a netgroup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			n, err := c.Netgroups().Put(cmd.Context(), args[0], apiv1.NetgroupSpec{Members: members(cmd)}, "*")
			if err != nil {
				return fmt.Errorf("%s: creating netgroup %q: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.PrintOutput(cmd, n.Resource, false, "", Netgroups{n.Resource})
		},
	}
	cmd.Flags().StringArray("member", nil, "A host name, address or CIDR; repeat for each")
	return cmd
}

func newListCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List netgroups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			ns := Netgroups{}
			for n, err := range c.Netgroups().List(cmd.Context(), nil) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				ns = append(ns, n)
			}
			return cmdutil.PrintOutput(cmd, ns, len(ns) == 0, "No netgroups.", ns)
		},
	}
}

func newGetCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Show a netgroup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			n, err := c.Netgroups().Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("%s: netgroup %q: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.PrintOutput(cmd, n.Resource, false, "", Netgroups{n.Resource})
		},
	}
}

func newUpdateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Replace a netgroup's members",
		Long:  `Replace a netgroup's members with those --member names. To empty it, delete it.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			res, err := cmdutil.Update(cmd, env, c.Netgroups(), cmdutil.ByPut(c.Netgroups()), "netgroup", args[0], func(spec *apiv1.NetgroupSpec) error {
				spec.Members = members(cmd)
				return nil
			})
			if err != nil {
				return err
			}
			n := res.Resource.Resource
			return cmdutil.PrintOutput(cmd, n, false, "", Netgroups{n})
		},
	}
	cmd.Flags().StringArray("member", nil, "A host name, address or CIDR; repeat for each")
	// Required, so a forgotten flag never empties every policy's allowlist.
	_ = cmd.MarkFlagRequired("member")
	return cmd
}

func newDeleteCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a netgroup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			n, err := c.Netgroups().Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("%s: netgroup %q: %w", cmd.CommandPath(), args[0], err)
			}
			force, _ := cmd.Flags().GetBool("force")
			return cmdutil.RunDeleteWithConfirmation(cmd, env, "netgroup", args[0], force, func() error {
				if _, err := c.Netgroups().Delete(cmd.Context(), args[0], n.ETag); err != nil {
					return fmt.Errorf("%s: deleting netgroup %q: %w", cmd.CommandPath(), args[0], err)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolP("force", "f", false, "Delete without asking")
	return cmd
}
