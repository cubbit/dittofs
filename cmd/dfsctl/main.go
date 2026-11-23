package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cubbit/dittofs/cmd/dfsctl/cmdutil"
	"github.com/cubbit/dittofs/cmd/dfsctl/commands"
	"github.com/cubbit/dittofs/internal/cli/credentials"
	"golang.org/x/term"
)

// Build-time variables injected via ldflags
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	commands.Version = version
	commands.Commit = commit
	commands.Date = date

	path, err := credentials.DefaultPath()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	// An interrupt ends the command's context: its call returns, its
	// defers run and its lock is released, and a prompt restores the
	// terminal before the process exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	status := commands.Run(ctx, cmdutil.Env{
		In:          os.Stdin,
		Out:         os.Stdout,
		Err:         os.Stderr,
		ConfigPath:  path,
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
	}, os.Args[1:])
	stop()
	os.Exit(status)
}
