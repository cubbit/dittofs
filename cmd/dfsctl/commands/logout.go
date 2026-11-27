package commands

import (
	"errors"
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

func newLogoutCmd(env cmdutil.Env) *cobra.Command {
	logoutCmd := &cobra.Command{
		Use:   "logout",
		Short: "Clear stored credentials",
		Long: `End the current context's session and clear its tokens.

The server is asked to end the session, then the tokens are removed from the
context; if the server cannot be reached the context keeps them, the only
handle on the live session, and logout fails so it can be run again. A
context signed out keeps its server, account and CA bundle so dfsctl login
signs in again with only a password. A context signed in with an API token only forgets
the token, which stays valid until it is revoked. To switch between contexts or
remove one entirely, use the context subcommand.

Examples:
  # Logout from the current context (clears tokens, keeps server URL)
  dfsctl logout

  # Logout then immediately log back in as a different user
  dfsctl logout && dfsctl login --username operator`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runLogout(cmd, env) },
	}
	return logoutCmd
}

func runLogout(cmd *cobra.Command, env cmdutil.Env) error {
	s, err := credentials.Read(env.ConfigPath)
	if err != nil {
		return err
	}
	name := s.GetCurrentContextName()
	cur, err := s.GetCurrentContext()
	if err != nil {
		return credentials.ErrNotLoggedIn
	}
	// The sign-out runs outside the lock, which a refresh it needs takes.
	signedOut := cur.AccessToken
	if cur.RefreshToken != "" {
		c, target, err := cmdutil.NewClient(env)
		if err != nil {
			return err
		}
		if target.Name != name || !cmdutil.SameTarget(target.Dialled, *cur) || target.Dialled.AccessToken != cur.AccessToken {
			return fmt.Errorf("%s: context %q changed meanwhile; run it again", cmd.CommandPath(), name)
		}
		err = c.SignOut(cmd.Context())
		switch {
		case err == nil:
		case errors.Is(err, credentials.ErrSessionEnded):
			// The sign-out was refused 401 and so was the refresh of its
			// session: the server knows the session no more.
		default:
			return fmt.Errorf("%s: the server did not end the session: %w; the context keeps its tokens, so run 'dfsctl logout' again", cmd.CommandPath(), err)
		}
		signedOut = target.Access()
	}
	// The tokens are cleared only if the context still holds the session
	// signed out: a login made meanwhile keeps its new one.
	if err := cmdutil.Locked(cmd.Context(), env, func(st *credentials.Store) error {
		now, err := st.GetContext(name)
		if err != nil || now.AccessToken != signedOut || !cmdutil.SameTarget(*now, *cur) {
			return fmt.Errorf("%s: context %q changed meanwhile; its new sign-in is kept", cmd.CommandPath(), name)
		}
		return st.SetTokens(name, "", "")
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(env.Out, "Logged out from context: %s\n", name)
	return nil
}
