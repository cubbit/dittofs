package main

import (
	"encoding/hex"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// FILE is the one file the basic story works on, spelled once: the bytes
// "journal-demo-fil" (16), shown everywhere as their hex.
var FILE = func() journal.FileID {
	var raw [16]byte
	copy(raw[:], "journal-demo-fil")
	return journal.NewFileID(raw)
}()

// fileHex is FILE as the 32 hex chars every view prints. hexOf renders any
// file id the same way.
func hexOf(id journal.FileID) string {
	b := id.Bytes()
	return hex.EncodeToString(b[:])
}

var fileHex = hexOf(FILE)

// beat is one step of a story: a title, the caption the presenter narrates,
// and the work the beat does once the presenter's Enter lands.
type beat struct {
	title   string
	caption string
	// mutates marks a beat whose effect the views should re-diff.
	mutates bool
	run     func(*demo)
}

// stories holds a story by name; the basic one is the session sheet's.
// A second story is another entry in this map, not another binary.
var stories = map[string][]beat{
	"basic": basicStory(),
}

func basicStory() []beat {
	return []beat{
		{
			title:   "journal online",
			caption: "the demo opened (or initialised) a real journal: one directory, one format file, a spare pool; the version window is raised the way an engine records it",
			run: func(d *demo) {
				d.printf("    dir:            %s\n", d.dir)
				d.printf("    state file:     %s\n", statePath(d.dir))
				d.printf("    share tag:      %d\n", d.tag)
				d.printf("      id, _ := journal.Init(ctx, dir, install, opts...)           // ran only on a fresh dir\n")
				d.printf("      j, _ := journal.Open(ctx, dir, floor, id, nil, opts...)\n")
				st := d.j.Stats()
				d.printf("      j.SetVersionLimit(journal.Version{Counter: %d})               // spares ready: %d\n", st.VersionLimit.Counter, st.Spares)
			},
		},
	}
}
