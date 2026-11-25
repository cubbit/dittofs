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
// command takes its ID.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Manage remote stores",
		Long: `Create, list, read, change and delete remote stores, and set a store's
credential.

A remote store is where namespaces keep their content: its endpoint, bucket and
the rest are its settings, and its credential is a secret the server seals and
never prints. The server refuses a change of a bound setting while a namespace
names the store, and a change of how the store is reached, its endpoint among
them, until it can prove the store holds the same content there.

Examples:
  # Create a store, its credential read from a file
  dfsctl store create --setting remote.endpoint=https://s3.example.com --credential-file cred.txt

  # Change a store's region and remove its storage class, so its default holds
  dfsctl store update 0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b --setting remote.region=eu-west-1 --unset remote.storage_class

  # Replace a store's credential (prompts for it)
  dfsctl store set-credential 0b5c1a7e-2f4d-4c8e-9a1b-3c5d7e9f1a2b`,
	}
	cmd.AddCommand(newCreateCmd(env), newListCmd(env), newGetCmd(env), newUpdateCmd(env), newDeleteCmd(env), newSetCredentialCmd(env))
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

func newListCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List remote stores",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := cmdutil.NewClient(env)
			if err != nil {
				return err
			}
			all := []apiv1.Store{}
			for st, err := range c.Stores().List(cmd.Context(), nil) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				all = append(all, st)
			}
			return cmdutil.PrintOutput(cmd, all, len(all) == 0, "No remote stores.", table(all...))
		},
	}
}

func newUpdateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Change a remote store's settings",
		Long: `Change a remote store's settings in one write: each --setting key=value set, a
value that is JSON taken as it is and any other as a string, and each --unset
key removed, so its default holds. What is not named stays as it is. Its
credential is set by 'dfsctl store set-credential'.`,
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
			res, err := cmdutil.Update(cmd, env, c.Stores(), cmdutil.ByPatch(c.Stores()), "store", args[0], func(s *apiv1.StoreSpec) error {
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
	cmd.Flags().StringArray("setting", nil, "A setting of the store, key=value (repeatable)")
	cmd.Flags().StringArray("unset", nil, "A setting of the store to remove (repeatable)")
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
