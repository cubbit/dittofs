// Package share implements the share commands for dfsctl: a share's
// lifecycle, its export policy and mounting it on this host.
package share

import (
	"fmt"
	"net/url"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// shares is shares as a table.
type shares []apiv1.Share

// Headers implements output.TableRenderer.
func (shares) Headers() []string { return []string{"NAME", "PATH", "STATE", "ID"} }

// Rows implements output.TableRenderer.
func (s shares) Rows() [][]string {
	rows := make([][]string, 0, len(s))
	for _, sh := range s {
		rows = append(rows, []string{sh.Spec.Name, sh.Spec.Path, sh.Status.State, sh.ID})
	}
	return rows
}

// New returns the share command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Manage shares",
		Long: `Create, read, change, quiesce, resume and delete shares, set their export
policies and mount them on this host.

A share is named by its name, the one an SMB client tree-connects to, or by
--id. grants and quotas on a share are the grant and quota commands.

Examples:
  # Create a share in a namespace, open to one network
  dfsctl share create archive --path /archive --namespace <namespace-id> --flavor sys --client 10.0.0.0/8=read-write

  # List the shares
  dfsctl share list

  # Move a share's NFS path
  dfsctl share update archive --path /old-archive

  # Delete a share and wait for its deletion to end
  dfsctl share delete archive --force --wait`,
	}
	cmd.AddCommand(newCreateCmd(env), newListCmd(env), newGetCmd(env), newUpdateCmd(env), newDeleteCmd(env),
		newStateCmd(env, "quiesce", "Quiesce a share: refuse new opens and drain its clients"),
		newStateCmd(env, "resume", "Resume a quiesced share"),
		newExportPolicyCmd(env), newMountCmd(env))
	return cmd
}

func newCreateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a share with its export policy",
		Long: `Create a share with its export policy.

The share is named by the name an SMB client tree-connects to; --path is its
path in the NFS namespace. The export policy's flags are those of
'share export-policy set'; a policy states its client rules, so name at least
one with --client (0.0.0.0/0 and ::/0 admit every client).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			path, _ := cmd.Flags().GetString("path")
			ns, _ := cmd.Flags().GetString("namespace")
			s := apiv1.NewShare{Spec: apiv1.ShareSpec{Name: args[0], Path: path, Namespace: ns, CaseSensitivity: "sensitive"}}
			if ci, _ := cmd.Flags().GetBool("case-insensitive"); ci {
				s.Spec.CaseSensitivity = "insensitive"
			}
			if err := applyPolicy(cmd, &s.ExportPolicy); err != nil {
				return err
			}
			sh, err := c.CreateShare(cmd.Context(), s)
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			return cmdutil.PrintOutput(cmd, sh.Resource, false, "", shares{sh.Resource})
		},
	}
	cmd.Flags().String("path", "", "The share's path in the NFS namespace")
	cmd.Flags().String("namespace", "", "The ID of the namespace the share's content is counted in")
	cmd.Flags().Bool("case-insensitive", false, "Fold the case of names; bound when the share is created")
	_ = cmd.MarkFlagRequired("path")
	_ = cmd.MarkFlagRequired("namespace")
	policyFlags(cmd)
	requirePolicy(cmd)
	return cmd
}

func newListCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List shares",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			filter := url.Values{}
			if ns, _ := cmd.Flags().GetString("namespace"); ns != "" {
				filter.Set("namespace", ns)
			}
			shares := shares{}
			for sh, err := range c.Shares().List(cmd.Context(), filter) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				shares = append(shares, sh)
			}
			return cmdutil.PrintOutput(cmd, shares, len(shares) == 0, "No shares.", shares)
		},
	}
	cmd.Flags().String("namespace", "", "Only the shares counted in the namespace of this ID")
	return cmd
}

func newGetCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get [name]",
		Short: "Show a share",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			sh, err := c.Shares().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("%s: share %s: %w", cmd.CommandPath(), id, err)
			}
			return cmdutil.PrintOutput(cmd, sh.Resource, false, "", shares{sh.Resource})
		},
	}
	cmdutil.NameOrID(cmd, "share")
	return cmd
}

func newUpdateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update [name]",
		Short: "Rename a share or move its NFS path",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			res, err := cmdutil.Update(cmd, env, c.Shares(), cmdutil.ByPatch(c.Shares()), "share", id, func(s *apiv1.ShareSpec) error {
				if cmd.Flags().Changed("name") {
					s.Name, _ = cmd.Flags().GetString("name")
				}
				if cmd.Flags().Changed("path") {
					s.Path, _ = cmd.Flags().GetString("path")
				}
				return nil
			})
			if err != nil {
				return err
			}
			return cmdutil.PrintOutput(cmd, res.Resource.Resource, false, "", shares{res.Resource.Resource})
		},
	}
	cmdutil.NameOrID(cmd, "share")
	cmd.Flags().String("name", "", "The share's new name")
	cmd.Flags().String("path", "", "The share's new path in the NFS namespace")
	cmd.MarkFlagsOneRequired("name", "path")
	return cmd
}

func newDeleteCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete [name]",
		Short: "Delete a share",
		Long: `Delete a share. The deletion is an operation: the command prints it, or,
with --wait, follows it to its end.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			sh, err := c.Shares().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("%s: share %s: %w", cmd.CommandPath(), id, err)
			}
			force, _ := cmd.Flags().GetBool("force")
			return cmdutil.RunDeleteWithConfirmation(cmd, env, "share", sh.Spec.Name, force, func() error {
				op, err := c.Shares().Delete(cmd.Context(), id, sh.ETag)
				if err != nil {
					return fmt.Errorf("%s: share %s: %w", cmd.CommandPath(), id, err)
				}
				return cmdutil.Follow(cmd, c, op)
			})
		},
	}
	cmdutil.NameOrID(cmd, "share")
	cmdutil.AddWait(cmd)
	cmd.Flags().BoolP("force", "f", false, "Delete without asking")
	return cmd
}

// newStateCmd is quiesce or resume, verb, an action whose expected state is
// its precondition, so it sends no If-Match.
func newStateCmd(env cmdutil.Env, verb, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   verb + " [name]",
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			id, err := cmdutil.Resolve(cmd, c.Shares(), "share", args)
			if err != nil {
				return err
			}
			res, err := c.Shares().Action(cmd.Context(), id, verb, nil, "")
			if err != nil {
				return fmt.Errorf("%s: share %s: %w", cmd.CommandPath(), id, err)
			}
			return cmdutil.PrintOutput(cmd, res.Resource.Resource, false, "", shares{res.Resource.Resource})
		},
	}
	cmdutil.NameOrID(cmd, "share")
	return cmd
}
