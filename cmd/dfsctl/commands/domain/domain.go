// Package domain implements the domain commands for dfsctl: the
// installation's actions on its directory and its domain join, each an
// operation.
package domain

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/pkg/node/apiv1"
	"github.com/cubbit/dittofs/pkg/node/apiv1/client"
	"github.com/spf13/cobra"
)

// New returns the domain command.
func New(env cmdutil.Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domain",
		Short: "Resolve principals, test the directory and join the domain",
		Long: `Act on the installation's directory and its domain join.

Each command starts an operation and prints it; with --wait it follows the
operation to its end, and 'dfsctl operation wait <id>' resumes an interrupted
wait.

Examples:
  # Find a directory user's SID
  dfsctl domain resolve 'CORP\bob' --wait

  # Join the domain as an administrator (prompts for the password)
  dfsctl domain join --user administrator@corp.example.com --wait

  # Join with a computer account an administrator created
  dfsctl domain join --offline --account FILESRV --password-file machine.txt`,
	}

	resolve := &cobra.Command{
		Use:   "resolve [DOMAIN\\name]",
		Short: "Resolve a directory principal by name or by SID",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var p apiv1.ResolvePrincipalParams
			p.SID, _ = cmd.Flags().GetString("sid")
			if len(args) == 1 {
				p.Name = args[0]
			}
			return run(cmd, env, "resolve-principal", p)
		},
	}
	resolve.Flags().String("sid", "", "The principal's SID, in place of its name")

	test := &cobra.Command{
		Use:   "test",
		Short: "Test the installation's directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, env, "test-directory", nil)
		},
	}

	join := &cobra.Command{
		Use:   "join",
		Short: "Join the installation to the domain",
		Long: `Join the installation to the directory's domain.

Online, the default, creates the computer account as the administrator --user
names, whose password is read from --password-file or a prompt. With --offline
the join takes a computer account an administrator created, and the password
read is that account's. The password is sent once, with no idempotency key, so
the server keeps no record of the request.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			offline, _ := cmd.Flags().GetBool("offline")
			var body apiv1.JoinDomain
			body.Spec.Mode = apiv1.JoinOnline
			if offline {
				body.Spec.Mode = apiv1.JoinOffline
			}
			body.Spec.Account, _ = cmd.Flags().GetString("account")
			body.Spec.OU, _ = cmd.Flags().GetString("ou")
			body.Spec.DNSHostName, _ = cmd.Flags().GetString("dns-host-name")
			body.Spec.Takeover, _ = cmd.Flags().GetBool("takeover")
			body.Credential.User, _ = cmd.Flags().GetString("user")
			// Before the password's prompt, which the server's own refusal
			// of the same would come after.
			switch {
			case offline && body.Credential.User != "":
				return fmt.Errorf("%s: --user names an online join's administrator; an offline join takes only the computer account's password", cmd.CommandPath())
			case !offline && body.Credential.User == "":
				return fmt.Errorf("%s: an online join takes the administrator's name; pass --user, or --offline", cmd.CommandPath())
			}
			label := "administrator's password"
			if offline {
				label = "computer account's password"
			}
			pw, err := cmdutil.NewSecrets(env).Read(cmd, "password-file", label)
			if err != nil {
				return err
			}
			body.Credential.Password = pw
			return run(cmd, env, "join-domain", body, client.WithoutIdempotencyKey())
		},
	}
	join.Flags().Bool("offline", false, "Take a computer account an administrator created")
	join.Flags().String("user", "", "The administrator an online join creates the computer account as")
	join.Flags().String("password-file", "", "File holding the password (prompts without it)")
	join.Flags().String("account", "", "The computer account's name (defaults to the installation's)")
	join.Flags().String("ou", "", "The organisational unit of an online join's computer account")
	join.Flags().String("dns-host-name", "", "The DNS host name of an online join's computer account")
	join.Flags().Bool("takeover", false, "Replace a join the installation holds, and online an account of that name it did not create")

	rotate := &cobra.Command{
		Use:   "rotate-machine-password",
		Short: "Rotate the computer account's password",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd, env, "rotate-machine-password", nil)
		},
	}

	for _, c := range []*cobra.Command{resolve, test, join, rotate} {
		cmdutil.AddWait(c)
	}
	cmd.AddCommand(resolve, test, join, rotate)
	return cmd
}

// run runs verb on the installation with body and follows the operation it
// starts.
func run(cmd *cobra.Command, env cmdutil.Env, verb string, body any, opts ...client.CallOption) error {
	c, _, err := cmdutil.NewClient(env)
	if err != nil {
		return err
	}
	op, err := c.InstallationAction(cmd.Context(), verb, body, opts...)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd.CommandPath(), err)
	}
	return cmdutil.Follow(cmd, c, op)
}
