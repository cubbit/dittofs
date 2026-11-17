// Command jdemo is an interactive, scripted demo of the journal: it owns a
// real journal in its own process and walks a story of beats, one Enter at
// a time, with views the presenter picks between beats.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cubbit/dittofs/pkg/block/journal"

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
// journal directory — six preallocated 256 MiB segments, about 1.5 GiB of
// real blocks per run — is removed on the way out so rehearsals do not
// fill the disk; a directory the presenter named stays, it is theirs.
func runMain(args []string, in io.Reader, out, errw io.Writer) int {
	flags := flag.NewFlagSet("jdemo", flag.ContinueOnError)
	flags.SetOutput(errw)
	dir := flags.String("dir", filepath.Join(os.TempDir(), fmt.Sprintf("jdemo-%d", os.Getpid()), "journal"), "journal directory")
	story := flags.String("story", "basic", "which story to walk")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if _, ok := jdemo.Stories()[*story]; !ok {
		_, _ = fmt.Fprintf(errw, "jdemo: no such story %q (have: %s)\n", *story, strings.Join(scenarios.Names(), ", "))
		return 2
	}
	if flags.Lookup("dir").DefValue == *dir {
		defer func() { _ = os.RemoveAll(filepath.Dir(*dir)) }()
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
