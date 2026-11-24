// Package user implements the user commands for dfsctl: the accounts and
// file owners the server holds, created, read, changed and deleted through
// the management API, and their passwords set.
package user

import (
	"context"
	"fmt"
	"strconv"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// Users is users as a table.
type Users []apiv1.User

// Headers implements output.TableRenderer.
func (Users) Headers() []string {
	return []string{"NAME", "ID", "UID", "ROLE", "EMAIL", "DISABLED"}
}

// Rows implements output.TableRenderer.
func (us Users) Rows() [][]string {
	rows := make([][]string, 0, len(us))
	for _, u := range us {
		uid := ""
		if u.Spec.UID != nil {
			uid = strconv.FormatUint(uint64(*u.Spec.UID), 10)
		}
		rows = append(rows, []string{u.Spec.Name, u.ID, uid, string(u.Spec.Role), u.Spec.Email, cmdutil.BoolToYesNo(u.Spec.Disabled)})
	}
	return rows
}

// New returns the user command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users",
		Long: `Create, list, read, change and delete the server's users, and set their
passwords.

A user is an account of the management API when it holds a role, and an owner
of files when it holds a uid; one may be both. Each command names a user by its
name, or by its ID with --id.

Examples:
  # Create a user owning files as uid 1000
  dfsctl user create alice --uid 1000 --email alice@example.com

  # Give it a password, read from a file
  dfsctl user set-password alice --password-file pw.txt

  # Clear its e-mail address
  dfsctl user update alice --email ""

  # Delete it and wait for the deletion to end
  dfsctl user delete alice --force --wait`,
	}
	cmd.AddCommand(newCreateCmd(env), newListCmd(env), newGetCmd(env), newUpdateCmd(env), newDeleteCmd(env), newSetPasswordCmd(env))
	return cmd
}

// specFlags gives cmd the flags that set a user's spec.
func specFlags(cmd *cobra.Command) {
	cmd.Flags().Uint32("uid", 0, "Unix user ID, which a create leaving it out allocates")
	cmd.Flags().String("email", "", "E-mail address; empty clears it")
	cmd.Flags().String("role", "", "Role in the management API (viewer|operator|admin); empty for none")
	cmd.Flags().Bool("disabled", false, "Refuse the user on every protocol and on the API")
}

// applyFlags sets on spec each field whose flag cmd was given, so an
// update changes only those, and an empty --email or --role clears it.
func applyFlags(cmd *cobra.Command, spec *apiv1.UserSpec) {
	f := cmd.Flags()
	if f.Changed("uid") {
		uid, _ := f.GetUint32("uid")
		spec.UID = &uid
	}
	if f.Changed("email") {
		spec.Email, _ = f.GetString("email")
	}
	if f.Changed("role") {
		role, _ := f.GetString("role")
		spec.Role = apiv1.Role(role)
	}
	if f.Changed("disabled") {
		spec.Disabled, _ = f.GetBool("disabled")
	}
}

// printResult prints the user a write answered, or follows the operation
// it started, as a write moving a machine's Kerberos name starts one.
func printResult(cmd *cobra.Command, c *client.Client, res client.Result[apiv1.UserSpec, struct{}]) error {
	if res.Operation != nil {
		return cmdutil.Follow(cmd, c, res.Operation)
	}
	u := res.Resource.Resource
	return cmdutil.PrintOutput(cmd, u, false, "", Users{u})
}

func newCreateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a user",
		Long:  `Create a user. It has no password until 'dfsctl user set-password' gives it one.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			spec := apiv1.UserSpec{Name: args[0]}
			applyFlags(cmd, &spec)
			res, err := c.Users().Create(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("%s: creating user %q: %w", cmd.CommandPath(), args[0], err)
			}
			return printResult(cmd, c, res)
		},
	}
	specFlags(cmd)
	cmdutil.AddWait(cmd)
	return cmd
}

func newListCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			us := Users{}
			for u, err := range c.Users().List(cmd.Context(), nil) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				us = append(us, u)
			}
			return cmdutil.PrintOutput(cmd, us, len(us) == 0, "No users.", us)
		},
	}
}

func newGetCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a user",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Users().Resource, "user", args)
			if err != nil {
				return err
			}
			u, err := c.Users().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("%s: user %s: %w", cmd.CommandPath(), id, err)
			}
			return cmdutil.PrintOutput(cmd, u.Resource, false, "", Users{u.Resource})
		},
	}
	cmdutil.NameOrID(cmd, "user")
	return cmd
}

func newUpdateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Change a user",
		Long: `Change the fields of a user that the flags name, leaving the others as they
are. An empty --email clears the address, and an empty --role takes the role
away.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Users().Resource, "user", args)
			if err != nil {
				return err
			}
			write := func(ctx context.Context, id string, _ apiv1.UserSpec, patch map[string]any, etag string) (client.Result[apiv1.UserSpec, struct{}], error) {
				return c.Users().Patch(ctx, id, patch, etag)
			}
			res, err := cmdutil.Update(cmd, env, c.Users(), write, "user", id, func(spec *apiv1.UserSpec) error {
				applyFlags(cmd, spec)
				return nil
			})
			if err != nil {
				return err
			}
			return printResult(cmd, c, res)
		},
	}
	cmdutil.NameOrID(cmd, "user")
	specFlags(cmd)
	cmdutil.AddWait(cmd)
	return cmd
}

func newDeleteCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a user",
		Long: `Delete a user. The deletion is an operation: the command prints its ID, or,
with --wait, follows it to its end.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Users().Resource, "user", args)
			if err != nil {
				return err
			}
			u, err := c.Users().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("%s: user %s: %w", cmd.CommandPath(), id, err)
			}
			force, _ := cmd.Flags().GetBool("force")
			return cmdutil.RunDeleteWithConfirmation(cmd, env, "user", u.Spec.Name, force, func() error {
				op, err := c.Users().Delete(cmd.Context(), id, u.ETag)
				if err != nil {
					return fmt.Errorf("%s: deleting user %s: %w", cmd.CommandPath(), id, err)
				}
				return cmdutil.Follow(cmd, c, op)
			})
		},
	}
	cmdutil.NameOrID(cmd, "user")
	cmd.Flags().BoolP("force", "f", false, "Delete without asking")
	cmdutil.AddWait(cmd)
	return cmd
}

func newSetPasswordCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-password <name>",
		Short: "Set a user's password",
		Long: `Set a user's password, read from a prompt or from the file --password-file
names. The user's sessions and API tokens are refused from then on. To change
your own password, run 'dfsctl password change'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Users().Resource, "user", args)
			if err != nil {
				return err
			}
			pw, err := cmdutil.NewSecrets(env).Read(cmd, "password-file", "new password")
			if err != nil {
				return err
			}
			defer clear(pw)
			if err := c.Users().SetPassword(cmd.Context(), id, pw); err != nil {
				return fmt.Errorf("%s: user %s: %w", cmd.CommandPath(), id, err)
			}
			return nil
		},
	}
	cmdutil.NameOrID(cmd, "user")
	cmd.Flags().String("password-file", "", "File holding the password (prompts without it)")
	return cmd
}
