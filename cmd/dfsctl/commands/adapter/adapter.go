// Package adapter implements the adapter commands for dfsctl: the protocol
// adapters, NFS and SMB, each named by its protocol.
package adapter

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// table is an adapter as a table, a row for each node that reports it.
func table(a apiv1.Adapter) cmdutil.Table {
	settings := strings.Join(slices.Sorted(maps.Keys(a.Spec.Settings)), ",")
	t := cmdutil.Table{{"PROTOCOL", "ENABLED", "SETTINGS", "NODE", "STATE"}}
	if len(a.Status.Nodes) == 0 {
		return append(t, []string{a.ID, cmdutil.BoolToYesNo(a.Spec.Enabled), settings, "", ""})
	}
	for _, n := range a.Status.Nodes {
		t = append(t, []string{a.ID, cmdutil.BoolToYesNo(a.Spec.Enabled), settings, n.Node, string(n.State)})
	}
	return t
}

// New returns the adapter command. No route lists the adapters, so each
// command names one by its protocol.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adapter",
		Short: "Manage protocol adapters",
		Long: `Read and set the protocol adapters, each named by its protocol: nfs or smb.

An adapter's settings are its fields below adapters.<protocol>, its port among
them; a node override of one, such as one host's port, is a setting at that
node's scope, set through 'dfsctl setting set node:<id>'.

Examples:
  # Show the NFS adapter on every node
  dfsctl adapter get nfs

  # Enable the SMB adapter on port 1445
  dfsctl adapter set smb --enabled --setting port=1445`,
	}
	get := &cobra.Command{
		Use:   "get <protocol>",
		Short: "Show an adapter",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			a, err := c.Adapters().Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("%s: adapter %s: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.PrintOutput(cmd, a.Resource, false, "", table(a.Resource))
		},
	}
	set := &cobra.Command{
		Use:   "set <protocol>",
		Short: "Change an adapter",
		Long: `Change an adapter: --enabled or --enabled=false, each --setting key=value
set, a value that is JSON taken as it is and any other as a string, and each
--unset key removed, so its default holds. What is not named stays as it is.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pairs, _ := cmd.Flags().GetStringArray("setting")
			settings, err := cmdutil.Settings(pairs)
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			unset, _ := cmd.Flags().GetStringArray("unset")
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			res, err := cmdutil.Update(cmd, env, c.Adapters(), cmdutil.ByPut(c.Adapters()), "adapter", args[0], func(s *apiv1.AdapterSpec) error {
				if cmd.Flags().Changed("enabled") {
					s.Enabled, _ = cmd.Flags().GetBool("enabled")
				}
				if s.Settings == nil {
					s.Settings = map[string]json.RawMessage{}
				}
				maps.Copy(s.Settings, settings)
				for _, k := range unset {
					delete(s.Settings, k)
				}
				return nil
			})
			if err != nil {
				return err
			}
			return cmdutil.PrintOutput(cmd, res.Resource.Resource, false, "", table(res.Resource.Resource))
		},
	}
	set.Flags().Bool("enabled", false, "Enable the adapter, or disable it with --enabled=false")
	set.Flags().StringArray("setting", nil, "A setting of the adapter, key=value (repeatable)")
	set.Flags().StringArray("unset", nil, "A setting of the adapter to remove (repeatable)")
	cmd.AddCommand(get, set)
	return cmd
}
