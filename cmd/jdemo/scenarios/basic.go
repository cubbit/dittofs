package scenarios

import (
	"context"

	"github.com/cubbit/dittofs/pkg/block/journal"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
)

// BasicStory is the talk's spine: the journal's life from open to verify,
// one beat per thing the room should take home.
func BasicStory() ([]jdemo.Beat, journal.Settings) {
	return basicBeats(), journal.Settings{
		Streams:         2,
		SpareTarget:     2,
		HeadroomRecords: 1024,
		MaxBytes:        4 << 30,
	}
}

func basicBeats() []jdemo.Beat {
	return []jdemo.Beat{
		{
			Title:   "journal online",
			Caption: "the demo opened (or initialised) a real journal: one directory, a format file, a made spare pool; the version window is raised the way an engine records it",
			Run: func(d *jdemo.Demo) {
				st := d.J.Stats()
				d.Printf("    dir:            %s\n", d.Dir)
				d.Printf("    state file:     %s\n", jdemo.StatePath(d.Dir))
				d.Codef("      id, _ := journal.Init(ctx, dir, install, opts...)            // ran only on a fresh dir")
				d.Codef("      j, _ := journal.Open(ctx, dir, floor, id, nil, opts...)")
				d.Codef("      j.SetVersionLimit(journal.Version{Counter: %d})            // spares ready: %d", st.VersionLimit.Counter, st.Spares)
			},
		},
		{
			Title:   "attach the share",
			Caption: "the engine works through one handle per share; every record it writes carries the share's tag",
			Run: func(d *jdemo.Demo) {
				h, err := d.J.Share(d.Tag, 0, journal.Version{})
				d.Codef("    h, err := j.Share(%d, 0, journal.Version{})", d.Tag)
				if err != nil {
					d.Yellowf("    // → %v", err)
					return
				}
				d.H[d.Tag] = h
				d.Yellowf("    // → handle attached, records carry tag %d", d.Tag)
			},
		},
		{
			Title:   "a first write",
			Caption: "the journal takes the bytes as one record and the placement index gains a held entry; the version is drawn now, from the window",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchEntries, Hint: "one held entry at v1"},
				{Kind: jdemo.WatchDirty, Hint: "the 11 bytes go dirty"},
				{Kind: jdemo.WatchFiles, Hint: "the file appears in the share"},
				{Kind: jdemo.WatchVersions, Hint: "a version is drawn from the window"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				v, err := h.WriteAt(context.Background(), jdemo.StoryFile, 0, []byte("hello world"), 0)
				d.Codef("    v, err := h.WriteAt(ctx, FILE, 0, []byte(\"hello world\"), 0)")
				d.Stash(v, err)
			},
		},
		{
			Title:   "overwrite half of it",
			Caption: "a newer version supersedes part of the old extent: the index narrows the old entry and keeps the superseded range retained until its settle",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchEntries, Hint: "one more index entry, the old range narrowed"},
				{Kind: jdemo.WatchVersions, Hint: "another version drawn"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				v, err := h.WriteAt(context.Background(), jdemo.StoryFile, 2, []byte("XY"), 0)
				d.Codef("    v, err := h.WriteAt(ctx, FILE, 2, []byte(\"XY\"), 0)")
				d.Stash(v, err)
			},
		},
		{
			Title:   "a big write — the roll",
			Caption: "300 MB of pattern: the active segment crosses its cap and rolls onto a fresh one; the spare the open made becomes the next segment",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchSegments, Hint: "the pool's spare becomes a content segment"},
				{Kind: jdemo.WatchHeld, Hint: "300 MB goes held"},
				{Kind: jdemo.WatchVersions, Hint: "another version drawn"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				payload := make([]byte, 300_000_000)
				pattern := []byte("0123456789abcdef")
				for i := range payload {
					payload[i] = pattern[i%len(pattern)]
				}
				v, err := h.WriteAt(context.Background(), jdemo.StoryFile, 4096, payload, 0)
				d.Codef("    v, err := h.WriteAt(ctx, FILE, 4096, pattern300MB, 0)         // 300,000,000 bytes")
				d.Stash(v, err)
			},
		},
		{
			Title:   "make it durable",
			Caption: "Sync is durability's receipt: one mark per stream, naming where the durable records reached — the thing the caller records and hands to the next open",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				marks, err := h.Sync(context.Background(), jdemo.StoryFile)
				d.Codef("    marks, err := h.Sync(ctx, FILE)")
				if err != nil {
					d.Yellowf("    // → %v", err)
					return
				}
				for _, m := range marks {
					d.Yellowf("    // → {Stream:%d Segment:%d Through:%d Offset:%d Sequence:%d}",
						m.Stream, m.Segment, m.Through, m.Offset, m.Sequence)
				}
			},
		},
		{
			Title:   "read across the gap",
			Caption: "ReadAt serves what the index holds and reports the rest as missing extents — never zeros: the gap between hello and the pattern is right here",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				p := make([]byte, 16)
				n, missing, held, asOf, err := h.ReadAt(context.Background(), jdemo.StoryFile, 0, p)
				d.Codef("    n, missing, held, asOf, err := h.ReadAt(ctx, FILE, 0, p16)")
				if err != nil {
					d.Yellowf("    // → err %v", err)
				}
				d.Yellowf("    // → n=%d, asOf=v%d", n, asOf.Counter)
				d.Hexdump(p[:n], 16)
				for _, m := range missing {
					d.Yellowf("    // → missing %d–%d", m.Off, m.Off+m.Len)
				}
				for _, hv := range held {
					d.Yellowf("    // → held %d–%d at v%d", hv.Extent.Off, hv.Extent.Off+hv.Extent.Len, hv.Version.Counter)
				}
			},
		},
		{
			Title:   "the change feed",
			Caption: "Since walks the versions above the floor: every held extent and removal marker, version by version — the journal's own changelog",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				d.Codef("    for ch := range h.Since(ctx, FILE, journal.Version{}) {")
				for ch, err := range h.Since(context.Background(), jdemo.StoryFile, journal.Version{}) {
					if err != nil {
						d.Yellowf("    // → %v", err)
						return
					}
					d.Printf("        %s\n", jdemo.ChangeLine(ch))
				}
				d.Printf("    }\n")
			},
		},
		{
			Title:   "truncate",
			Caption: "everything past 32 goes: the index drops those extents and the removal marker waits, unsettled, for metadata to record it; the record is durable the moment Truncate returns",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchMarkers, Hint: "a removal marker waits, unsettled"},
				{Kind: jdemo.WatchHeld, Hint: "the held bytes past 32 go"},
				{Kind: jdemo.WatchVersions, Hint: "another version drawn"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				v, err := h.Truncate(context.Background(), jdemo.StoryFile, 32)
				d.Codef("    v, err := h.Truncate(ctx, FILE, 32)")
				d.Stash(v, err)
			},
		},
		{
			Title:   "settle the removal",
			Caption: "Settle says metadata has recorded the truncation: the marker goes, the superseded ranges go with it — Since narrows to what is still owed",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchMarkers, Hint: "the marker goes"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				d.Codef("    h.Settle(FILE, journal.Version{Counter: %d})      // the truncate's version", d.Stashed.Counter)
				h.Settle(jdemo.StoryFile, d.Stashed)
				d.Codef("    for ch := range h.Since(ctx, FILE, journal.Version{}) {")
				any := false
				for ch, err := range h.Since(context.Background(), jdemo.StoryFile, journal.Version{}) {
					if err != nil {
						d.Yellowf("    // → %v", err)
						return
					}
					any = true
					d.Printf("        %s\n", jdemo.ChangeLine(ch))
				}
				d.Printf("    }\n")
				if !any {
					d.Yellowf("    // → nothing owed")
				}
			},
		},
		{
			Title:   "a reserved write",
			Caption: "the engine reserves before a burst: the write's bytes, entries, versions and spares are held up front, so no limit refuses them",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchEntries, Hint: "one more index entry"},
				{Kind: jdemo.WatchDirty, Hint: "the 8 bytes go dirty"},
				{Kind: jdemo.WatchVersions, Hint: "another version drawn"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				res, err := h.Reserve(8192, 2)
				if err != nil {
					d.Codef("    res, err := h.Reserve(8192, 2)")
					return
				}
				v, werr := res.WriteAt(context.Background(), jdemo.StoryFile, 65536, []byte("reserved"), 0)
				d.Codef("    res, _ := h.Reserve(8192, 2)")
				d.Codef("    v, err := res.WriteAt(ctx, FILE, 65536, []byte(\"reserved\"), 0)")
				d.Stash(v, werr)
				_ = res.Close()
			},
		},
		{
			Title:   "offload",
			Caption: "the engine offloads dirty extents to the remote tier, marking them in the index; the journal's own answer names what this build owes",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				err := h.Offload(context.Background(), jdemo.StoryFile, d.Stashed, 0, 0,
					func(context.Context, journal.Offer, func([]journal.Extent) error) error {
						return nil
					})
				d.Codef("    err := h.Offload(ctx, FILE, through=d.Stashed, limit=0, widen=0, sink)")
				d.Yellowf("    // → %v", err)
			},
		},
		{
			Title:   "verify",
			Caption: "the index checked against the segments, the report rather than the repair; it lands with the same subtask as the per-segment accounting",
			Run: func(d *jdemo.Demo) {
				rep, err := d.J.Inspect().Verify(context.Background())
				d.Codef("    rep, err := j.Inspect().Verify(ctx)")
				if err != nil {
					d.Yellowf("    // → %v", err)
					return
				}
				d.Yellowf("    // → %+v", rep)
			},
		},
	}
}
