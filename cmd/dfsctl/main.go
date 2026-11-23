package main

import (
	"fmt"
	"os"

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
	os.Exit(commands.Run(cmdutil.Env{
		In:          os.Stdin,
		Out:         os.Stdout,
		Err:         os.Stderr,
		ConfigPath:  path,
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
	}, os.Args[1:]))
}
