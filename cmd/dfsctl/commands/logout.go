package commands

import (
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
context, which keeps its server, account and CA bundle so dfsctl login signs in
again with only a password. A context signed in with an API token only forgets
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
	if cur.RefreshToken != "" {
		c, _, err := cmdutil.NewClient(env)
		if err == nil {
			err = c.SignOut(cmd.Context())
		}
		if err != nil {
			_, _ = fmt.Fprintf(env.Err, "The server did not end the session: %v\n", err)
		}
	}
	// The tokens are cleared only if the context still names what was
	// signed out: a login made meanwhile keeps its new session.
	if err := cmdutil.Locked(cmd.Context(), env, func(st *credentials.Store) error {
		now, err := st.GetContext(name)
		if err != nil || !cmdutil.SameTarget(*now, *cur) {
			return fmt.Errorf("%s: context %q changed meanwhile; its new sign-in is kept", cmd.CommandPath(), name)
		}
		return st.SetTokens(name, "", "")
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(env.Out, "Logged out from context: %s\n", name)
	return nil
}
