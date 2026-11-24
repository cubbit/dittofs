// Package token implements the API token commands for dfsctl: creating,
// listing and revoking an account's API tokens, the signed-in account's
// unless --user names another.
package token

import (
	"fmt"
	"net/url"
	"time"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/output"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// Tokens is API tokens as a table.
type Tokens []apiv1.APIToken

// Headers implements output.TableRenderer.
func (Tokens) Headers() []string { return []string{"ID", "CREATED", "EXPIRES"} }

// Rows implements output.TableRenderer.
func (ts Tokens) Rows() [][]string {
	rows := make([][]string, 0, len(ts))
	for _, t := range ts {
		rows = append(rows, []string{t.ID, t.Status.Created.Format(time.RFC3339), t.Spec.Expires.Format(time.RFC3339)})
	}
	return rows
}

// tokenTag is an API token's tag.
//
// decision: an API token has no read route, so revoke cannot read its tag
// first; it sends the one tag a token ever has, which the API fixes because
// a token is never rewritten. Overturned if a token gains a field a write
// changes, which would give it versions and a read.
const tokenTag = `"1"`

// New returns the token command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens",
		Long: `Create, list and revoke API tokens: credentials a script or a service signs
in with, through 'dfsctl login --token-file', in place of a password.

Each command acts on the signed-in account's tokens; an admin names another
account with --user, and creates one for another account only when that
account is a service account, one holding no password. A context signed in
with an API token names no account to dfsctl, so it needs --user, and it
creates no token.

Examples:
  # Create a token valid for 30 days, kept in a file
  (umask 077; dfsctl token create --expires-in 720h > token.txt)

  # Sign a context in with it
  dfsctl login --server https://dfs.example.com --token-file token.txt -c ci

  # Revoke it
  dfsctl token revoke 1a92dcf1-bdbb-49eb-acc1-a78ef6b4e5f1`,
	}
	cmd.PersistentFlags().String("user", "", "The account whose tokens to act on, by its name (defaults to the signed-in one)")
	cmd.AddCommand(newCreateCmd(env), newListCmd(env), newRevokeCmd(env))
	return cmd
}

// tokensOf returns the API tokens of the account cmd acts on:
// the one --user names, or the context's own.
func tokensOf(cmd *cobra.Command, env cmdutil.Env) (client.APITokenResource, error) {
	var none client.APITokenResource
	c, target, err := cmdutil.NewClient(env)
	if err != nil {
		return none, err
	}
	id := target.Dialled.UserID
	if name, _ := cmd.Flags().GetString("user"); name != "" {
		if id, err = cmdutil.Resolve(cmd, c.Users().Resource, "user", []string{name}); err != nil {
			return none, err
		}
	} else if id == "" {
		return none, fmt.Errorf("%s: context %q names no account ID, as one signed in with an API token or by an older dfsctl does; pass --user, or run 'dfsctl login' again", cmd.CommandPath(), target.Name)
	}
	return c.APITokens(id), nil
}

func newCreateCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an API token",
		Long: `Create an API token expiring --expires-in from now. The token is shown this
once: as a table, stdout holds the token alone, so it can be written to a file;
-o json or -o yaml print the whole record, the token in its status.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The format is checked before the token is made, which no later
			// call can show again.
			f, _ := cmd.Flags().GetString("output")
			format, err := output.ParseFormat(f)
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			tokens, err := tokensOf(cmd, env)
			if err != nil {
				return err
			}
			now := time.Now
			if env.Now != nil {
				now = env.Now
			}
			in, _ := cmd.Flags().GetDuration("expires-in")
			t, err := tokens.Create(cmd.Context(), apiv1.APITokenSpec{Expires: now().Add(in).Truncate(time.Second)})
			if err != nil {
				return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
			}
			if format != output.FormatTable {
				return cmdutil.PrintOutput(cmd, t.Resource, false, "", nil)
			}
			_, _ = fmt.Fprintf(env.Err, "API token %s, expiring %s; it is not shown again\n", t.ID, t.Spec.Expires.Format(time.RFC3339))
			_, err = fmt.Fprintln(cmd.OutOrStdout(), t.Status.Token)
			return err
		},
	}
	cmd.Flags().Duration("expires-in", 30*24*time.Hour, "How long the token is valid, at most the server's api.token_max_lifetime")
	return cmd
}

func newListCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tokens, err := tokensOf(cmd, env)
			if err != nil {
				return err
			}
			ts := Tokens{}
			for t, err := range tokens.List(cmd.Context(), url.Values{}) {
				if err != nil {
					return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
				}
				ts = append(ts, t)
			}
			return cmdutil.PrintOutput(cmd, ts, len(ts) == 0, "No API tokens.", ts)
		},
	}
}

// newRevokeCmd takes the token's ID as its argument: a token has no name to
// resolve, so the ID is the only way to name it.
func newRevokeCmd(env cmdutil.Env) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an API token",
		Long:  `Revoke an API token, named by its ID, so the server refuses it from then on.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tokens, err := tokensOf(cmd, env)
			if err != nil {
				return err
			}
			_, err = tokens.Delete(cmd.Context(), args[0], tokenTag)
			// A token's tag is fixed, so a 412 says only that the account
			// holds no token of that ID.
			if cmdutil.IsPreconditionFailed(err) {
				return fmt.Errorf("%s: no API token %s for this account", cmd.CommandPath(), args[0])
			}
			if err != nil {
				return fmt.Errorf("%s: API token %s: %w", cmd.CommandPath(), args[0], err)
			}
			return nil
		},
	}
}
