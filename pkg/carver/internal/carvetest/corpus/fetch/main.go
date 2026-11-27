// Command fetch fills a directory with the carver's pinned corpus, at
// <dir>/<kind>/<name>, downloading only the files missing there or changed
// since, and refusing one whose hashes do not match. Point
// DITTOFS_CORPUS_DIR at the directory to run the checks that read it.
//
// Usage:
//
//	go run ./pkg/carver/internal/carvetest/corpus/fetch <dir>
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"

	"github.com/cubbit/dittofs/pkg/carver/internal/carvetest/corpus"
)

func main() {
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, flag.Arg(0), corpus.Entries); err != nil {
		fmt.Fprintf(os.Stderr, "fetch: %v\n", err)
		stop()
		os.Exit(1)
	}
}

// run fetches entries into dir, refusing an empty dir.
func run(ctx context.Context, dir string, entries []corpus.Entry) error {
	if dir == "" {
		return errors.New("usage: fetch <dir>")
	}

	return corpus.Fetch(ctx, http.DefaultClient, dir, entries)
}
