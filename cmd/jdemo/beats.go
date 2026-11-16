package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/cubbit/dittofs/pkg/block/journal"
)

// FILE is the one file the basic story works on, spelled once: the bytes
// "journal-demo-fil" (16), shown everywhere as their hex.
var FILE = func() journal.FileID {
	var raw [16]byte
	copy(raw[:], "journal-demo-fil")
	return journal.NewFileID(raw)
}()

// fileHex is FILE as the 32 hex chars every view prints.
func hexOf(id journal.FileID) string {
	b := id.Bytes()
	return hex.EncodeToString(b[:])
}

var fileHex = hexOf(FILE)

// beat is one step of a story: a title, the caption the presenter narrates,
// the facts the room is told to look for before the beat runs, and the work
// the beat does once the presenter's Enter lands.
type beat struct {
	title   string
	caption string
	// mutates marks a beat whose effect the views should re-diff.
	mutates bool
	// watch names the facts this beat is expected to move — printed as
	// hints before the Enter, confirmed with measured deltas after.
	watch []watching
	run   func(*demo)
}

// watching is one fact a beat moves, with the prose the presenter narrates.
type watching struct {
	kind watchKind
	hint string
}

// watchKind is a Stats field a beat asks to be measured across its run.
type watchKind uint8

const (
	watchHeld watchKind = iota
	watchDirty
	watchFiles
	watchEntries
	watchSegments
	watchSealed
	watchSpares
	watchMarkers
	watchVersions
)

func (k watchKind) of(st journal.Stats) int64 {
	switch k {
	case watchHeld:
		return st.HeldBytes
	case watchDirty:
		return st.DirtyBytes
	case watchFiles:
		return int64(st.Files)
	case watchEntries:
		return st.ExtentCount
	case watchSegments:
		return int64(st.Segments)
	case watchSealed:
		return int64(st.Sealed)
	case watchSpares:
		return int64(st.Spares)
	case watchMarkers:
		return int64(st.RemovalMarkers)
	case watchVersions:
		return int64(st.VersionsLeft)
	}
	return 0
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
			caption: "the demo opened (or initialised) a real journal: one directory, a format file, a made spare pool; the version window is raised the way an engine records it",
			run: func(d *demo) {
				st := d.j.Stats()
				d.printf("    dir:            %s\n", d.dir)
				d.printf("    state file:     %s\n", statePath(d.dir))
				d.codef("      id, _ := journal.Init(ctx, dir, install, opts...)            // ran only on a fresh dir")
				d.codef("      j, _ := journal.Open(ctx, dir, floor, id, nil, opts...)")
				d.codef("      j.SetVersionLimit(journal.Version{Counter: %d})            // spares ready: %d", st.VersionLimit.Counter, st.Spares)
			},
		},
		{
			title:   "attach the share",
			caption: "the engine works through one handle per share; every record it writes carries the share's tag",
			run: func(d *demo) {
				h, err := d.j.Share(d.tag, 0, journal.Version{})
				d.codef("    h, err := j.Share(%d, 0, journal.Version{})", d.tag)
				if err != nil {
					d.yellowf("    // → %v", err)
					return
				}
				d.h[d.tag] = h
				d.yellowf("    // → handle attached, records carry tag %d", d.tag)
			},
		},
		{
			title:   "a first write",
			caption: "the journal takes the bytes as one record and the placement index gains a held entry; the version is drawn now, from the window",
			mutates: true,
			watch: []watching{
				{watchEntries, "one held entry at v1"},
				{watchDirty, "the 11 bytes go dirty"},
				{watchFiles, "the file appears in the share"},
				{watchVersions, "a version is drawn from the window"},
			},
			run: func(d *demo) {
				h := d.h[d.tag]
				v, err := h.WriteAt(context.Background(), FILE, 0, []byte("hello world"), 0)
				d.codef("    v, err := h.WriteAt(ctx, FILE, 0, []byte(\"hello world\"), 0)")
				d.stash(v, err)
			},
		},
		{
			title:   "overwrite half of it",
			caption: "a newer version supersedes part of the old extent: the index narrows the old entry and keeps the superseded range retained until its settle",
			mutates: true,
			watch: []watching{
				{watchEntries, "one more index entry, the old range narrowed"},
				{watchVersions, "another version drawn"},
			},
			run: func(d *demo) {
				h := d.h[d.tag]
				v, err := h.WriteAt(context.Background(), FILE, 2, []byte("XY"), 0)
				d.codef("    v, err := h.WriteAt(ctx, FILE, 2, []byte(\"XY\"), 0)")
				d.stash(v, err)
			},
		},
		{
			title:   "a big write — the roll",
			caption: "300 MB of pattern: the active segment crosses its cap and rolls onto a fresh one; the spare the open made becomes the next segment",
			mutates: true,
			watch: []watching{
				{watchSegments, "the pool's spare becomes a content segment"},
				{watchHeld, "300 MB goes held"},
				{watchVersions, "another version drawn"},
			},
			run: func(d *demo) {
				h := d.h[d.tag]
				payload := make([]byte, 300_000_000)
				pattern := []byte("0123456789abcdef")
				for i := range payload {
					payload[i] = pattern[i%len(pattern)]
				}
				v, err := h.WriteAt(context.Background(), FILE, 4096, payload, 0)
				d.codef("    v, err := h.WriteAt(ctx, FILE, 4096, pattern300MB, 0)         // 300,000,000 bytes")
				d.stash(v, err)
			},
		},
		{
			title:   "read across the gap",
			caption: "ReadAt serves what the index holds and reports the rest as missing extents — never zeros: the gap between hello and the pattern is right here",
			run: func(d *demo) {
				h := d.h[d.tag]
				p := make([]byte, 16)
				n, missing, held, asOf, err := h.ReadAt(context.Background(), FILE, 0, p)
				d.codef("    n, missing, held, asOf, err := h.ReadAt(ctx, FILE, 0, p16)")
				if err != nil {
					d.yellowf("    // → err %v", err)
				}
				d.yellowf("    // → n=%d, asOf=v%d", n, asOf.Counter)
				d.hexdump(p[:n], 16)
				for _, m := range missing {
					d.yellowf("    // → missing %d–%d", m.Off, m.Off+m.Len)
				}
				for _, hv := range held {
					d.yellowf("    // → held %d–%d at v%d", hv.Extent.Off, hv.Extent.Off+hv.Extent.Len, hv.Version.Counter)
				}
			},
		},
		{
			title:   "the change feed",
			caption: "Since walks the versions above the floor: every held extent and removal marker, version by version — the journal's own changelog",
			run: func(d *demo) {
				h := d.h[d.tag]
				d.codef("    for ch := range h.Since(ctx, FILE, journal.Version{}) {")
				for ch, err := range h.Since(context.Background(), FILE, journal.Version{}) {
					if err != nil {
						d.yellowf("    // → %v", err)
						return
					}
					d.printf("        %s\n", changeLine(ch))
				}
				d.printf("    }\n")
			},
		},
		{
			title:   "truncate",
			caption: "everything past 32 goes: the index drops those extents and the removal marker waits, unsettled, for metadata to record it; the record is durable the moment Truncate returns",
			mutates: true,
			watch: []watching{
				{watchMarkers, "a removal marker waits, unsettled"},
				{watchHeld, "the held bytes past 32 go"},
				{watchVersions, "another version drawn"},
			},
			run: func(d *demo) {
				h := d.h[d.tag]
				v, err := h.Truncate(context.Background(), FILE, 32)
				d.codef("    v, err := h.Truncate(ctx, FILE, 32)")
				d.stash(v, err)
			},
		},
		{
			title:   "settle the removal",
			caption: "Settle says metadata has recorded the truncation: the marker goes, the superseded ranges go with it — Since narrows to what is still owed",
			mutates: true,
			watch: []watching{
				{watchMarkers, "the marker goes"},
			},
			run: func(d *demo) {
				h := d.h[d.tag]
				d.codef("    h.Settle(FILE, journal.Version{Counter: %d})      // the truncate's version", d.version.Counter)
				h.Settle(FILE, d.version)
				d.codef("    for ch := range h.Since(ctx, FILE, journal.Version{}) {")
				any := false
				for ch, err := range h.Since(context.Background(), FILE, journal.Version{}) {
					if err != nil {
						d.yellowf("    // → %v", err)
						return
					}
					any = true
					d.printf("        %s\n", changeLine(ch))
				}
				d.printf("    }\n")
				if !any {
					d.yellowf("    // → nothing owed")
				}
			},
		},
		{
			title:   "a reserved write",
			caption: "the engine reserves before a burst: the write's bytes, entries, versions and spares are held up front, so no limit refuses them",
			mutates: true,
			watch: []watching{
				{watchEntries, "one more index entry"},
				{watchDirty, "the 8 bytes go dirty"},
				{watchVersions, "another version drawn"},
			},
			run: func(d *demo) {
				h := d.h[d.tag]
				res, err := h.Reserve(8192, 2)
				if err != nil {
					d.codef("    res, err := h.Reserve(8192, 2)")
					return
				}
				v, werr := res.WriteAt(context.Background(), FILE, 65536, []byte("reserved"), 0)
				d.codef("    res, _ := h.Reserve(8192, 2)")
				d.codef("    v, err := res.WriteAt(ctx, FILE, 65536, []byte(\"reserved\"), 0)")
				d.stash(v, werr)
				_ = res.Close()
			},
		},
		{
			title:   "offload",
			caption: "the engine offloads dirty extents to the remote tier, marking them in the index; the journal's own answer names what this build owes",
			run: func(d *demo) {
				h := d.h[d.tag]
				err := h.Offload(context.Background(), FILE, d.version, 0, 0,
					func(context.Context, journal.Offer, func([]journal.Extent) error) error {
						return nil
					})
				d.codef("    err := h.Offload(ctx, FILE, through=d.version, limit=0, widen=0, sink)")
				d.yellowf("    // → %v", err)
			},
		},
		{
			title:   "verify",
			caption: "the index checked against the segments, the report rather than the repair; it lands with the same subtask as the per-segment accounting",
			run: func(d *demo) {
				rep, err := d.j.Inspect().Verify(context.Background())
				d.codef("    rep, err := j.Inspect().Verify(ctx)")
				if err != nil {
					d.yellowf("    // → %v", err)
					return
				}
				d.yellowf("    // → %+v", rep)
			},
		},
	}
}

// stash records the version the last mutating beat drew, for the beats that
// name it back (settle names the truncate's version; offload a through).
func (d *demo) stash(v journal.Version, err error) {
	if err != nil {
		d.yellowf("    // → %v", err)
		return
	}
	d.version = v
	d.yellowf("    // → v%d", v.Counter)
}

// changeLine renders one change the way Since prints it: kind, extent,
// version, and a write's mtime when it carries one.
func changeLine(ch journal.Change) string {
	line := fmt.Sprintf("%s off=%d len=%d v%d", changeKindWord(ch.Kind), ch.Extent.Off, ch.Extent.Len, ch.Version.Counter)
	if ch.Mtime != 0 {
		line += fmt.Sprintf(" mtime=%d", ch.Mtime)
	}
	return line
}

func changeKindWord(k journal.ChangeKind) string {
	switch k {
	case journal.ChangeWrite:
		return "write"
	case journal.ChangeTruncate:
		return "truncate"
	case journal.ChangeDeallocate:
		return "deallocate"
	case journal.ChangeDelete:
		return "delete"
	case journal.ChangeCloneTarget:
		return "clone"
	case journal.ChangeLoss:
		return "loss"
	}
	return fmt.Sprintf("kind%d", uint8(k))
}

// confirmed renders one watched fact after the beat ran: the journal's own
// measurement of what moved, so the confirmation can never claim something
// that did not happen. A watched value that stayed put says so plainly.
func (w watching) confirmed(before, after journal.Stats) string {
	var delta int64
	if w.kind == watchVersions {
		// Versions left counts DOWN what the window still holds: one
		// drawn version shows as −1 in the line's terms.
		delta = int64(after.VersionsLeft) - int64(before.VersionsLeft)
	} else {
		delta = w.kind.of(after) - w.kind.of(before)
	}
	if delta == 0 {
		return "· " + w.hint + " — unchanged"
	}
	name, value := "", ""
	switch w.kind {
	case watchHeld:
		name, value = "held", signedBytes(delta)
	case watchDirty:
		name, value = "dirty", signedBytes(delta)
	case watchFiles:
		name, value = "files", signedCount(delta)
	case watchEntries:
		name, value = "index", signedCount(delta)+" entry"
		if delta != 1 {
			value += "ies"
		}
	case watchSegments:
		name, value = "segments", signedCount(delta)
	case watchSealed:
		name, value = "sealed", signedCount(delta)
	case watchSpares:
		name, value = "spares", signedCount(delta)
	case watchMarkers:
		name, value = "removal markers", signedCount(delta)
	case watchVersions:
		name, value = "versions", signedCount(-delta)
	}
	return "✓ " + name + " " + value
}

func signedCount(delta int64) string {
	if delta > 0 {
		return "+" + fmt.Sprint(delta)
	}
	return fmt.Sprint(delta)
}

func signedBytes(delta int64) string {
	if delta > 0 {
		return "+" + humanSize(delta)
	}
	return "-" + humanSize(-delta)
}
