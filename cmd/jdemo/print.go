package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// friendly prints err as one line a presenter can act on: what happened and,
// where it is on the user to fix, what to do.
func friendly(w io.Writer, err error) {
	var un *unreachableError
	if errors.As(err, &un) {
		fmt.Fprintf(w, "jdemo: daemon not reachable at %s — run 'jdemo start' first\n", un.path)
		return
	}
	fmt.Fprintf(w, "jdemo: %v\n", err)
}

// fail is friendly for a fatal CLI error: print and exit non-zero.
func fail(err error) {
	friendly(os.Stderr, err)
	os.Exit(1)
}
