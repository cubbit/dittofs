package main

import (
	"os"

	"github.com/cubbit/dittofs/cmd/dfsctl/commands"
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
	os.Exit(commands.Main(os.Args[1:]))
}
