// Package store implements the store commands for dfsctl: the remote
// stores namespaces are created on.
package store

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

// table is stores as a table.
func table(s ...apiv1.Store) cmdutil.Table {
	t := cmdutil.Table{{"ID", "ENDPOINT", "CREDENTIAL", "SETTINGS"}}
	for _, st := range s {
		var endpoint string
		_ = json.Unmarshal(st.Spec.Settings["remote.endpoint"], &endpoint) // an endpoint that is no string prints empty
		cred := "none"
		if c := st.Spec.Credential; c != nil {
			cred = fmt.Sprintf("%s v%d", c.Ref, c.Version)
		}
		keys := slices.Sorted(maps.Keys(st.Spec.Settings))
		t = append(t, []string{st.ID, endpoint, cred, strings.Join(keys, ",")})
	}
	return t
}

// New returns the store command. A remote store has no name, so each
// command takes its ID. No route lists the stores or changes a store's
// spec: its settings but the endpoint and the credential change one by one
// through 'dfsctl setting set store:<id>'.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Manage remote stores",
		Long: `Create, read and delete remote stores, and set a store's credential.

A remote store is where namespaces keep their content: its endpoint, bucket and
the rest are its settings, and its credential is a secret the server seals and
never prints. A store's endpoint is fixed at its creation; its other settings
change through 'dfsctl setting set store:<id>'.

Examples:
  # Create a store, its credential read from a file
  dfsctl store create --setting remote.endpoint=https://s3.example.com --credential-file cred.txt

  # Replace a store's credential (prompts for it)
  dfsctl store set-credential 0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b`,
	}
	cmd.AddCommand(newCreateCmd(env), newGetCmd(env), newDeleteCmd(env), newSetCredentialCmd(env))
	return cmd
}

func newCreateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a remote store",
		Long: `Create a remote store with its settings, each --setting key=value, a value
that is JSON taken as it is and any other as a string. With --credential-file
the store is created with the credential that file holds.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pairs, _ := cmd.Flags().GetStringArray("setting")
			settings, err := cmdutil.Settings(pairs)
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			spec := apiv1.StoreSpec{Settings: settings}
			if path, _ := cmd.Flags().GetString("credential-file"); path != "" {
				v, err := cmdutil.NewSecrets(env).Read(cmd, "credential-file", "credential")
				if err != nil {
					return err
				}
				spec.Credential = &apiv1.StoreCredential{Value: v}
			}
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			st, err := c.Stores().Create(cmd.Context(), spec)
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			return cmdutil.PrintOutput(cmd, st.Resource, false, "", table(st.Resource))
		},
	}
	cmd.Flags().StringArray("setting", nil, "A setting of the store, key=value (repeatable)")
	cmd.Flags().String("credential-file", "", "File holding the store's credential")
	return cmd
}

func newGetCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show a remote store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			st, err := c.Stores().Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("%s: store %s: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.PrintOutput(cmd, st.Resource, false, "", table(st.Resource))
		},
	}
}

func newDeleteCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a remote store no namespace names",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			st, err := c.Stores().Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("%s: store %s: %w", cmd.CommandPath(), args[0], err)
			}
			force, _ := cmd.Flags().GetBool("force")
			return cmdutil.RunDeleteWithConfirmation(cmd, env, "store", args[0], force, func() (*apiv1.Operation, error) {
				op, err := c.Stores().Delete(cmd.Context(), args[0], st.ETag)
				if err != nil {
					return nil, fmt.Errorf("%s: store %s: %w", cmd.CommandPath(), args[0], err)
				}
				return op, cmdutil.Follow(cmd, c, op)
			})
		},
	}
	cmdutil.AddWait(cmd)
	cmd.Flags().BoolP("force", "f", false, "Delete without asking for confirmation")
	return cmd
}

func newSetCredentialCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-credential <id>",
		Short: "Set a remote store's credential",
		Long: `Set a remote store's credential, read from the file --credential-file names
or, without it, from a prompt. The server proves the credential before it
writes it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := cmdutil.NewSecrets(env).Read(cmd, "credential-file", "credential")
			if err != nil {
				return err
			}
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			// decision: the credential is set under If-None-Match "*", a first
			// set, since its tag is the secret's version, which no read answers;
			// a set over a credential the store holds is a rotation, which the
			// server refuses until rotation is an operation. Send the
			// credential's own tag once a read answers it.
			ref, _, err := c.SetStoreCredential(cmd.Context(), args[0], v, "*")
			if err != nil {
				return fmt.Errorf("%s: store %s: %w", cmd.CommandPath(), args[0], err)
			}
			return cmdutil.PrintOutput(cmd, ref, false, "", cmdutil.Table{{"REF", "VERSION"}, {ref.Ref, fmt.Sprint(ref.Version)}})
		},
	}
	cmd.Flags().String("credential-file", "", "File holding the credential (prompts without it)")
	return cmd
}
