// Command jdemo is an interactive, scripted demo of the journal: it owns a
// real journal in its own process and walks a story of beats, one Enter at a
// time, with views the presenter picks between beats.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"flag"
)

func main() {
	dir := flag.String("dir", filepath.Join(os.TempDir(), fmt.Sprintf("jdemo-%d", os.Getpid()), "journal"), "journal directory")
	story := flag.String("story", "basic", "which story to walk")
	flag.Parse()

	d := newDemo(*dir, os.Stdin, os.Stdout)
	d.storyID = *story
	if _, ok := stories[*story]; !ok {
		fmt.Fprintf(os.Stderr, "jdemo: no such story %q (have: basic)\n", *story)
		os.Exit(2)
	}
	if err := d.run(); err != nil {
		fmt.Fprintf(os.Stderr, "jdemo: %v\n", err)
		os.Exit(1)
	}
}
