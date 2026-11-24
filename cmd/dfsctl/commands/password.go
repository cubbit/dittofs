package commands

import (
	"bytes"
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
)

func newPasswordCmd(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "password",
		Short: "Manage your own password",
	}
	change := &cobra.Command{
		Use:   "change",
		Short: "Change the signed-in account's password",
		Long: `Change the password of the account the current context is signed in as.

The current password and the new one are read from prompts, the new one twice,
or from the files --current-password-file and --new-password-file name. The
change ends every other session of the account; the context keeps the new
session the server answers. An account whose password must change can make
this call, and sign out, and nothing else until it does.

Examples:
  # Change the password interactively
  dfsctl password change

  # Change it from a script
  dfsctl password change --current-password-file old.txt --new-password-file new.txt`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runPasswordChange(cmd, env) },
	}
	change.Flags().String("current-password-file", "", "File holding the current password (prompts without it)")
	change.Flags().String("new-password-file", "", "File holding the new password (prompts twice without it)")
	cmd.AddCommand(change)
	return cmd
}

func runPasswordChange(cmd *cobra.Command, env cmdutil.Env) error {
	snap, err := credentials.Read(env.ConfigPath)
	if err != nil {
		return err
	}
	if cur, err := snap.GetCurrentContext(); err == nil && cur.AccessToken != "" && cur.RefreshToken == "" {
		return fmt.Errorf("%s: context %q holds an API token; it has no password to change", cmd.CommandPath(), snap.GetCurrentContextName())
	}
	c, target, err := cmdutil.NewClient(env)
	if err != nil {
		return err
	}
	sec := cmdutil.NewSecrets(env)
	current, err := sec.Read(cmd, "current-password-file", "current password")
	if err != nil {
		return err
	}
	next, err := sec.Read(cmd, "new-password-file", "new password")
	if err != nil {
		return err
	}
	if !cmd.Flags().Changed("new-password-file") {
		again, err := sec.Prompt(cmd, "new-password-file", "new password again")
		if err != nil {
			return err
		}
		if !bytes.Equal(next, again) {
			return fmt.Errorf("%s: the new passwords differ", cmd.CommandPath())
		}
	}
	tk, err := c.ChangePassword(cmd.Context(), current, next)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
	}
	// The change's tokens are saved only to the context the change was sent
	// for: one signed in elsewhere meanwhile keeps its own.
	if err := cmdutil.Locked(cmd.Context(), env, func(st *credentials.Store) error {
		now, err := st.GetContext(target.Name)
		if err != nil || !cmdutil.SameTarget(*now, target.Dialled) {
			return fmt.Errorf("%s: the password changed, but context %q changed meanwhile; run 'dfsctl login' again", cmd.CommandPath(), target.Name)
		}
		return st.SetTokens(target.Name, tk.Access, tk.Refresh)
	}); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(env.Out, "Password changed")
	return nil
}
