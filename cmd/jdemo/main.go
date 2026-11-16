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
)

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

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
	if _, ok := stories[*story]; !ok {
		fmt.Fprintf(errw, "jdemo: no such story %q (have: basic)\n", *story)
		return 2
	}
	if flags.Lookup("dir").DefValue == *dir {
		defer os.RemoveAll(filepath.Dir(*dir))
	}

	d := newDemo(*dir, in, out)
	d.storyID = *story
	if err := d.run(); err != nil {
		fmt.Fprintf(errw, "jdemo: %v\n", err)
		return 1
	}
	return 0
}
