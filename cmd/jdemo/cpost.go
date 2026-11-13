package main

import (
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal"
	"github.com/spf13/cobra"
)

// cPost is the one client call behind every CLI subcommand: dial the root's
// socket, POST the body, decode into resp when it carries one.
func cPost(cmd *cobra.Command, route string, body any, resp any) error {
	return Dial(socketFrom(cmd)).Do(cmd.Context(), "POST", route, body, resp)
}

// parseErr is the CLI's plainest refusal: a message that names what was
// wrong with the input, and nothing else.
func parseErr(msg string) error {
	return fmt.Errorf("%s", msg)
}

// wireChange is one change on the since route.
type wireChange struct {
	Kind    string      `json:"kind"`
	Off     int64       `json:"off"`
	Len     int64       `json:"len"`
	Version versionJSON `json:"version"`
	Mtime   int64       `json:"mtime"`
}

// toChange rebuilds a wire change for changeLine.
func (c wireChange) toChange() journal.Change {
	return journal.Change{
		Kind:    kindFromName(c.Kind),
		Extent:  journal.Extent{Off: c.Off, Len: c.Len},
		Version: versionFromJSON(c.Version),
		Mtime:   c.Mtime,
	}
}

// kindFromName is kindName's inverse: the daemon only ever sends kindName's
// own answers, so an unknown name never arrives; zero answers for one.
func kindFromName(name string) journal.ChangeKind {
	for k, word := range journalKindWords {
		if word == name {
			return k
		}
	}
	return 0
}

var journalKindWords = map[journal.ChangeKind]string{
	journal.ChangeWrite:       "write",
	journal.ChangeTruncate:    "truncate",
	journal.ChangeDeallocate:  "deallocate",
	journal.ChangeDelete:      "delete",
	journal.ChangeCloneTarget: "clone",
	journal.ChangeLoss:        "loss",
}
