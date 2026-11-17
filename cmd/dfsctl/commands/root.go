// Package commands implements the CLI commands for dfsctl client.
package commands

import (
	"fmt"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	ctxcmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/context"
	groupcmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/group"
	quotacmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/quota"
	sharecmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/share"
	usercmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/user"
	"github.com/spf13/cobra"
)

// Version, Commit and Date describe the build; main sets them from the values
// -ldflags gives it, before building a root.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// NewRoot builds dfsctl's command tree for one invocation, reading and writing
// only through env.
func NewRoot(env cmdutil.Env) *cobra.Command {
	root := &cobra.Command{
		Use:   "dfsctl",
		Short: "DittoFS Control - Remote management client",
		Long: `dfsctl is the command-line client for managing DittoFS servers remotely.

Its commands for users, groups, shares and quotas return with the server's v2
API; until then each refuses. The context subcommand manages the server
contexts kept in ~/.config/dfsctl/config.json.

Use "dfsctl [command] --help" for more information about a command.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(env.In)
	root.SetOut(env.Out)
	root.SetErr(env.Err)

	root.PersistentFlags().String("server", "", "Server URL (overrides stored credential)")
	root.PersistentFlags().StringP("output", "o", "table", "Output format (table|json|yaml)")
	root.PersistentFlags().Bool("no-color", false, "Disable colored output")
	root.PersistentFlags().BoolP("verbose", "v", false, "Enable verbose output")
	root.PersistentFlags().String("cacert", "", "Path to a PEM CA bundle trusted for the server certificate (overrides stored)")

	root.AddCommand(
		newVersionCmd(),
		newLoginCmd(),
		newLogoutCmd(),
		newStatusCmd(),
		ctxcmd.New(env),
		usercmd.New(),
		groupcmd.New(),
		sharecmd.New(),
		quotacmd.New(),
		newCompletionCmd(),
	)

	// Hide the default completion command (we provide our own)
	root.CompletionOptions.DisableDefaultCmd = true
	runGroups(root)
	return root
}

// runGroups makes every command below root that only groups subcommands print
// its help when run bare and refuse anything else as an unknown command; Cobra
// checks for an unknown subcommand only at the root, and a group with no body
// would otherwise print its help and exit 0 for a verb it does not have.
func runGroups(c *cobra.Command) {
	for _, sub := range c.Commands() {
		if sub.HasSubCommands() && !sub.Runnable() {
			sub.Args = cobra.NoArgs
			sub.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }
		}
		runGroups(sub)
	}
}

// Run executes one dfsctl invocation with args, printing any error to env.Err,
// and returns the process's exit status.
func Run(env cmdutil.Env, args []string) int {
	root := NewRoot(env)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintf(env.Err, "Error: %v\n", err)
		return 1
	}
	return 0
}
