// Package namespace implements the namespace commands for dfsctl: the
// namespaces shares keep their content in, each on a remote store.
package namespace

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/spf13/cobra"
)

// table is namespaces as a table.
func table(ns ...apiv1.Namespace) cmdutil.Table {
	t := cmdutil.Table{{"ID", "STORE", "SETTINGS"}}
	for _, n := range ns {
		t = append(t, []string{n.ID, n.Spec.Store, strings.Join(slices.Sorted(maps.Keys(n.Spec.Settings)), ",")})
	}
	return t
}

// New returns the namespace command. A namespace has no name, so each
// command takes its ID. A namespace's spec is fixed at its creation.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "namespace",
		Short: "Manage namespaces",
		Long: `Create, list, read and delete namespaces.

A namespace is created on a remote store with its bound settings, such as its
prefix and chunking, which are fixed from then on.

Examples:
  # Create a namespace on a store
  dfsctl namespace create --store 0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b --setting namespace.prefix=builds/

  # List the namespaces
  dfsctl namespace list`,
	}
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a namespace on a remote store",
		Long: `Create a namespace on the remote store --store names, with its settings, each
--setting key=value, a value that is JSON taken as it is and any other as a
string.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, _ := cmd.Flags().GetString("store")
			pairs, _ := cmd.Flags().GetStringArray("setting")
			settings, err := cmdutil.Settings(pairs)
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			n, err := c.Namespaces().Create(cmd.Context(), apiv1.NamespaceSpec{Store: store, Settings: settings})
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			return cmdutil.PrintOutput(cmd, n.Resource, false, "", table(n.Resource))
		},
	}
	create.Flags().String("store", "", "The ID of the remote store the namespace is created on")
	create.Flags().StringArray("setting", nil, "A setting of the namespace, key=value (repeatable)")
	_ = create.MarkFlagRequired("store")

	list := &cobra.Command{
		Use:   "list",
		Short: "List namespaces",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			var all []apiv1.Namespace
			for n, err := range c.Namespaces().List(cmd.Context(), nil) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				all = append(all, n)
			}
			return cmdutil.PrintOutput(cmd, all, len(all) == 0, "No namespaces.", table(all...))
		},
	}

	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a namespace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			n, err := c.Namespaces().Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("%s: namespace %s: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.PrintOutput(cmd, n.Resource, false, "", table(n.Resource))
		},
	}

	del := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a namespace no share names",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			n, err := c.Namespaces().Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("%s: namespace %s: %w", cmd.CommandPath(), args[0], err)
			}
			op, err := c.Namespaces().Delete(cmd.Context(), args[0], n.ETag)
			if err != nil {
				return fmt.Errorf("%s: namespace %s: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.Follow(cmd, c, op)
		},
	}
	cmdutil.AddWait(del)

	cmd.AddCommand(create, list, get, del)
	return cmd
}
