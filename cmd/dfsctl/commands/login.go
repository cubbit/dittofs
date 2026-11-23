package commands

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

func newLoginCmd(env cmdutil.Env) *cobra.Command {
	loginCmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with DittoFS server",
		Long: `Sign in to a DittoFS server and keep the session in a named context.

The server is an https URL, an http URL on a loopback address, or the path of
the node's local socket. The password is read from a prompt, or from the file
--password-file names; --token-file signs in with an API token instead. The
context keeps the server, the account, the CA bundle --cacert names and the
session's tokens, in a file only its owner can read; later commands refresh
the session themselves. Without --server, --cacert or --username, login reuses
the context's.

Examples:
  # Sign in to a node through its local socket (prompts for the password)
  dfsctl login --server /run/dittofs/api.sock --username admin

  # Sign in to a remote server whose certificate a private CA signs
  dfsctl login --server https://dfs.example.com --username admin --cacert ca.pem

  # Sign in from a script
  dfsctl login --server https://dfs.example.com -u admin --password-file pw.txt

  # Use an API token in a context of its own
  dfsctl login --server https://dfs.example.com --token-file token.txt -c ci

  # Sign in again to the current context (password prompt only)
  dfsctl login`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runLogin(cmd, env) },
	}
	loginCmd.Flags().String("server", "", "Server URL or local socket path (defaults to the context's)")
	loginCmd.Flags().StringP("username", "u", "", "Username")
	loginCmd.Flags().String("password-file", "", "File holding the password (prompts without it)")
	loginCmd.Flags().String("token-file", "", "File holding an API token to sign in with instead of a password")
	loginCmd.Flags().StringP("context", "c", "", "Context name (defaults to the current one, or \"default\")")
	loginCmd.Flags().String("cacert", "", "Path to a PEM CA bundle trusted for the server certificate")
	loginCmd.MarkFlagsMutuallyExclusive("password-file", "token-file")
	return loginCmd
}

// staticToken is an API token's source: it never refreshes.
type staticToken string

func (t staticToken) Token(context.Context) (string, error) { return string(t), nil }

func (staticToken) Refresh(context.Context, string) (string, error) {
	return "", errors.New("the server refused the API token")
}

func runLogin(cmd *cobra.Command, env cmdutil.Env) error {
	s, err := credentials.Read(env.ConfigPath)
	if err != nil {
		return err
	}
	name, _ := cmd.Flags().GetString("context")
	if name == "" {
		name = cmp.Or(s.GetCurrentContextName(), "default")
	}
	prev := &credentials.Context{}
	if c, err := s.GetContext(name); err == nil {
		prev = c
	}
	server, _ := cmd.Flags().GetString("server")
	user, _ := cmd.Flags().GetString("username")
	cacert, _ := cmd.Flags().GetString("cacert")
	if server, err = normalServer(server); err != nil {
		return err
	}
	stored, err := normalServer(prev.ServerURL)
	if err != nil {
		return err
	}
	if cacert != "" {
		if cacert, err = filepath.Abs(cacert); err != nil {
			return err
		}
	} else if server == "" || server == stored {
		cacert = prev.CACert
	}
	server = cmp.Or(server, stored)
	if server == "" {
		return fmt.Errorf("%s: context %q names no server; pass --server", cmd.CommandPath(), name)
	}
	next := &credentials.Context{ServerURL: server, Username: cmp.Or(user, prev.Username), CACert: cacert}

	mustChange := false
	sec := cmdutil.NewSecrets(env)
	if cmd.Flags().Changed("token-file") {
		// An API token names its account to the server, not to dfsctl: the
		// context keeps only a --username given with it.
		next.Username = user
		b, err := sec.Read(cmd, "token-file", "API token")
		if err != nil {
			return err
		}
		next.AccessToken = string(b)
		c, err := cmdutil.Dial(server, cacert, client.WithTokenSource(staticToken(next.AccessToken)))
		if err != nil {
			return err
		}
		// The health read checks the server's version and that it accepts
		// the token before the token is kept.
		if _, err := c.Health(cmd.Context()); err != nil {
			return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
		}
	} else {
		if next.Username == "" {
			return fmt.Errorf("%s: no account to sign in; pass --username", cmd.CommandPath())
		}
		password, err := sec.Read(cmd, "password-file", "password")
		if err != nil {
			return err
		}
		c, err := cmdutil.Dial(server, cacert)
		if err != nil {
			return err
		}
		tk, err := c.SignIn(cmd.Context(), next.Username, password)
		if err != nil {
			return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
		}
		next.AccessToken, next.RefreshToken, mustChange = tk.Access, tk.Refresh, tk.MustChange
	}

	if err := cmdutil.Locked(cmd.Context(), env, func(st *credentials.Store) error { return st.SetContext(name, next) }); err != nil {
		return err
	}
	who := next.Username
	if next.RefreshToken == "" {
		who = "an API token"
	}
	_, _ = fmt.Fprintf(env.Out, "Logged in to %s as %s (context %s)\n", server, who, name)
	if mustChange {
		_, _ = fmt.Fprintln(env.Err, "The password must be changed before any other call: run 'dfsctl password change'")
	}
	return nil
}

// normalServer is server as a context stores it, so two spellings of one
// server compare equal: a URL without its trailing "/", and a path, which
// has a separator and no scheme, made absolute. Anything else, as host:port,
// is left for the client to refuse by name.
func normalServer(server string) (string, error) {
	switch {
	case strings.Contains(server, "://"):
		return strings.TrimRight(server, "/"), nil
	case strings.ContainsAny(server, `/`+string(filepath.Separator)):
		return filepath.Abs(server)
	}
	return server, nil
}
