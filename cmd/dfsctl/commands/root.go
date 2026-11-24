// Package commands implements the CLI commands for dfsctl client.
package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	ctxcmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/context"
	groupcmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/group"
	operationcmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/operation"
	quotacmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/quota"
	sharecmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/share"
	usercmd "github.com/cubbit/dittofs/cmd/dfsctl/commands/user"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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

	root.PersistentFlags().StringP("output", "o", "table", "Output format (table|json|yaml)")
	root.PersistentFlags().Bool("no-color", false, "Disable colored output")
	root.PersistentFlags().BoolP("verbose", "v", false, "Enable verbose output")

	root.AddCommand(
		newVersionCmd(),
		newLoginCmd(env),
		newLogoutCmd(env),
		newPasswordCmd(env),
		newStatusCmd(),
		ctxcmd.New(env),
		usercmd.New(),
		groupcmd.New(),
		sharecmd.New(),
		quotacmd.New(),
		operationcmd.New(env),
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

// Main runs one dfsctl process's invocation with args, over the process's
// streams and the credentials file at its default path, and returns its exit
// status. A first interrupt or SIGTERM ends the command's context: its call
// returns, its defers run and its lock is released, and a prompt restores
// the terminal. A second one kills the process, so a read that does not
// watch the context, such as a confirmation, still ends.
func Main(args []string) int {
	path, err := credentials.DefaultPath()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); stop() }()
	return Run(ctx, cmdutil.Env{
		In:          os.Stdin,
		Out:         os.Stdout,
		Err:         os.Stderr,
		ConfigPath:  path,
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
	}, args)
}

// Run executes one dfsctl invocation with args under ctx, printing any error
// to env.Err, and returns the process's exit status.
func Run(ctx context.Context, env cmdutil.Env, args []string) int {
	root := NewRoot(env)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(env.Err, "Error: %v\n", err)
		return 1
	}
	return 0
}
