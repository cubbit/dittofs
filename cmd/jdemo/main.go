//go:build jdemo

// Command jdemo is an interactive, scripted demo of the journal: it owns a
// real journal in its own process and walks a story of beats, one Enter at
// a time, with views the presenter picks between beats.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cubbit/dittofs/pkg/journal"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
	"github.com/cubbit/dittofs/cmd/jdemo/scenarios"
)

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// mainOptions are extra journal options runMain opens with. Production
// adds none — a real directory is judged by the real mount it lives on;
// a test running through runMain sets the mount probe its host cannot
// choose.
var mainOptions []journal.Option

// runMain is the binary: flags, the story, and the cleanup. The default
// journal directory is a fresh unpredictable temporary one, with the spare
// segments the journal preallocates as real blocks; it is removed on the way
// out, interrupt included, so rehearsals do not fill the disk. A directory
// the presenter named stays, it is theirs.
func runMain(args []string, in io.Reader, out, errw io.Writer) int {
	flags := flag.NewFlagSet("jdemo", flag.ContinueOnError)
	flags.SetOutput(errw)
	dir := flags.String("dir", "", "journal directory (default: a fresh temporary one, removed on exit)")
	story := flags.String("story", "basic", "which story to walk")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if _, ok := jdemo.Stories()[*story]; !ok {
		_, _ = fmt.Fprintf(errw, "jdemo: no such story %q (have: %s)\n", *story, strings.Join(scenarios.Names(), ", "))
		return 2
	}
	if *dir == "" {
		tmp, err := os.MkdirTemp("", "jdemo-")
		if err != nil {
			_, _ = fmt.Fprintf(errw, "jdemo: %v\n", err)
			return 1
		}
		*dir = filepath.Join(tmp, "journal")
		defer func() { _ = os.RemoveAll(tmp) }()
		// A blocked read of the presenter's Enter cannot be woken, so an
		// interrupt removes the temporary directory itself and exits.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sig)
		go func() {
			if _, ok := <-sig; ok {
				_ = os.RemoveAll(tmp)
				os.Exit(130)
			}
		}()
	}

	d := jdemo.NewDemo(*dir, in, out)
	d.Options = mainOptions
	d.StoryID = *story
	if err := d.Run(); err != nil {
		_, _ = fmt.Fprintf(errw, "jdemo: %v\n", err)
		return 1
	}
	return 0
}
