// Package node implements the node commands for dfsctl: the nodes of the
// installation, as each registered at its last start.
package node

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// table is nodes as a table.
func table(ns ...apiv1.Node) cmdutil.Table {
	t := cmdutil.Table{{"ID", "ADDRESS", "BINARY", "DOMAIN", "LAPSED"}}
	for _, n := range ns {
		t = append(t, []string{n.ID, n.Status.Address, n.Status.Binary, n.Status.Domain, cmdutil.BoolToYesNo(n.Status.Lapsed)})
	}
	return t
}

// New returns the node command. A node has no name, so get takes its ID.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "List the installation's nodes",
		Long: `List and read the installation's nodes: the address each serves on, the
binary it runs, its failure domain, and whether a takeover marked it lapsed.`,
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List nodes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			all := []apiv1.Node{}
			for n, err := range c.Nodes().List(cmd.Context(), nil) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				all = append(all, n)
			}
			return cmdutil.PrintOutput(cmd, all, len(all) == 0, "No nodes.", table(all...))
		},
	}
	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			n, err := c.Nodes().Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("%s: node %s: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.PrintOutput(cmd, n.Resource, false, "", table(n.Resource))
		},
	}
	cmd.AddCommand(list, get)
	return cmd
}
