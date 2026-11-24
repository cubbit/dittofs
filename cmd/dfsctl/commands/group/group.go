// Package group implements the group commands for dfsctl: the server's
// groups, created, read, changed and deleted through the management API,
// and their members added and removed.
package group

import (
	"fmt"
	"strconv"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// Groups is groups as a table.
type Groups []apiv1.Group

// Headers implements output.TableRenderer.
func (Groups) Headers() []string { return []string{"NAME", "ID", "GID"} }

// Rows implements output.TableRenderer.
func (gs Groups) Rows() [][]string {
	rows := make([][]string, 0, len(gs))
	for _, g := range gs {
		gid := ""
		if g.Spec.GID != nil {
			gid = strconv.FormatUint(uint64(*g.Spec.GID), 10)
		}
		rows = append(rows, []string{g.Spec.Name, g.ID, gid})
	}
	return rows
}

// memberTag is a membership's tag while it exists.
//
// decision: a membership has no read route, so remove-member cannot read its
// tag first; it sends the one tag a membership ever has, which the API
// fixes because a membership never changes: it exists or it does not.
// Overturned if a membership gains a field a write changes, which would
// give it versions and a read.
const memberTag = `"1"`

// New returns the group command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "group",
		Short: "Manage groups",
		Long: `Create, list, read, change and delete the server's groups, and add and remove
their members.

Each command names a group by its name, or by its ID with --id; add-member
and remove-member name the group and the user by their names.

Examples:
  # Create a group with gid 1001
  dfsctl group create editors --gid 1001

  # Add alice to it
  dfsctl group add-member editors alice

  # Delete it and wait for the deletion to end
  dfsctl group delete editors --force --wait`,
	}
	cmd.AddCommand(newCreateCmd(env), newListCmd(env), newGetCmd(env), newUpdateCmd(env), newDeleteCmd(env),
		newMemberCmd(env, "add-member", "Add a user to a group"), newMemberCmd(env, "remove-member", "Remove a user from a group"))
	return cmd
}

func newCreateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			spec := apiv1.GroupSpec{Name: args[0]}
			if cmd.Flags().Changed("gid") {
				gid, _ := cmd.Flags().GetUint32("gid")
				spec.GID = &gid
			}
			g, err := c.Groups().Create(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("%s: creating group %q: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.PrintOutput(cmd, g.Resource, false, "", Groups{g.Resource})
		},
	}
	cmd.Flags().Uint32("gid", 0, "Unix group ID, which a create leaving it out allocates")
	return cmd
}

func newListCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List groups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			gs := Groups{}
			for g, err := range c.Groups().List(cmd.Context(), nil) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				gs = append(gs, g)
			}
			return cmdutil.PrintOutput(cmd, gs, len(gs) == 0, "No groups.", gs)
		},
	}
}

func newGetCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a group",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Groups(), "group", args)
			if err != nil {
				return err
			}
			g, err := c.Groups().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("%s: group %s: %w", cmd.CommandPath(), id, err)
			}
			return cmdutil.PrintOutput(cmd, g.Resource, false, "", Groups{g.Resource})
		},
	}
	cmdutil.NameOrID(cmd, "group")
	return cmd
}

func newUpdateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Change a group",
		Long:  `Change the fields of a group that the flags name: its name and its gid.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Groups(), "group", args)
			if err != nil {
				return err
			}
			res, err := cmdutil.Update(cmd, env, c.Groups(), cmdutil.ByPatch(c.Groups()), "group", id, func(spec *apiv1.GroupSpec) error {
				if cmd.Flags().Changed("name") {
					spec.Name, _ = cmd.Flags().GetString("name")
				}
				if cmd.Flags().Changed("gid") {
					gid, _ := cmd.Flags().GetUint32("gid")
					spec.GID = &gid
				}
				return nil
			})
			if err != nil {
				return err
			}
			g := res.Resource.Resource
			return cmdutil.PrintOutput(cmd, g, false, "", Groups{g})
		},
	}
	cmdutil.NameOrID(cmd, "group")
	cmd.Flags().String("name", "", "New name")
	cmd.Flags().Uint32("gid", 0, "Unix group ID")
	return cmd
}

func newDeleteCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a group",
		Long: `Delete a group. The deletion is an operation: the command prints its ID, or,
with --wait, follows it to its end.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Groups(), "group", args)
			if err != nil {
				return err
			}
			g, err := c.Groups().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("%s: group %s: %w", cmd.CommandPath(), id, err)
			}
			force, _ := cmd.Flags().GetBool("force")
			return cmdutil.RunDeleteWithConfirmation(cmd, env, "group", g.Spec.Name, force, func() error {
				op, err := c.Groups().Delete(cmd.Context(), id, g.ETag)
				if err != nil {
					return fmt.Errorf("%s: deleting group %s: %w", cmd.CommandPath(), id, err)
				}
				return cmdutil.Follow(cmd, c, op)
			})
		},
	}
	cmdutil.NameOrID(cmd, "group")
	cmd.Flags().BoolP("force", "f", false, "Delete without asking")
	cmdutil.AddWait(cmd)
	return cmd
}

// newMemberCmd is add-member or remove-member, verb, which names the group
// and the user by their names.
func newMemberCmd(env cmdutil.Env, verb, short string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <group> <user>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			gid, err := cmdutil.Resolve(cmd, c.Groups(), "group", args[:1])
			if err != nil {
				return err
			}
			uid, err := cmdutil.Resolve(cmd, c.Users().Resource, "user", args[1:])
			if err != nil {
				return err
			}
			members := c.Members(gid)
			if verb == "add-member" {
				_, err = members.Put(cmd.Context(), uid, struct{}{}, "*")
			} else {
				_, err = members.Delete(cmd.Context(), uid, memberTag)
			}
			if err != nil {
				return fmt.Errorf("%s: user %q in group %q: %w", cmd.CommandPath(), args[1], args[0], err)
			}
			return nil
		},
	}
}
