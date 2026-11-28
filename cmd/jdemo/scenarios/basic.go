//go:build jdemo

package scenarios

import (
	"context"

	"github.com/cubbit/dittofs/pkg/journal"

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
				d.Codef("      id, _ := journal.Init(ctx, dir, installID, opts...)            // ran only on a fresh dir")
				d.Codef("      j, _ := journal.Open(ctx, dir, floor, expectedID, noMarks, opts...)")
				d.Codef("      j.SetVersionLimit(journal.Version{Counter: %d})            // spares ready: %d", st.VersionLimit.Counter, st.Spares)
			},
		},
		{
			Title:   "attach the share",
			Caption: "the engine works through one handle per share; every record it writes carries the share's tag",
			Run: func(d *jdemo.Demo) {
				h, err := d.J.Share(d.Tag, 0, journal.Version{})
				d.Codef("    h, err := j.Share(shareTag_%d, shareLimit_none, floor)      // 0: no limit below the journal's own", d.Tag)
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
				if h == nil {
					d.NoShare()
					return
				}
				v, err := h.WriteAt(context.Background(), jdemo.StoryFile, 0, []byte("hello world"), 0)
				d.Codef("    v, err := h.WriteAt(ctx, FILE, offset_0, []byte(\"hello world\"), mtimeNone)")
				d.Stash(v, err)
			},
		},
		{
			Title:   "overwrite half of it",
			Caption: "a newer version supersedes part of the old extent: the old entry splits around it, and the superseded range stays RETAINED — never served, kept until the new write is synced, so a crash that loses the new bytes still has the old ones",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchEntries, Hint: "the old entry splits in three: the new range held, the superseded range retained"},
				{Kind: jdemo.WatchVersions, Hint: "another version drawn"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				if h == nil {
					d.NoShare()
					return
				}
				v, err := h.WriteAt(context.Background(), jdemo.StoryFile, 2, []byte("XY"), 0)
				d.Codef("    v, err := h.WriteAt(ctx, FILE, offset_2, []byte(\"XY\"), mtimeNone)")
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
				if h == nil {
					d.NoShare()
					return
				}
				payload := make([]byte, 300_000_000)
				pattern := []byte("0123456789abcdef")
				for i := range payload {
					payload[i] = pattern[i%len(pattern)]
				}
				v, err := h.WriteAt(context.Background(), jdemo.StoryFile, 4096, payload, 0)
				d.Codef("    v, err := h.WriteAt(ctx, FILE, offset_4KiB, payload_300MB, mtimeNone)   // 300,000,000 bytes")
				d.Stash(v, err)
			},
		},
		{
			Title:   "make it durable",
			Caption: "Sync is durability's receipt: one mark per stream, naming where the durable records reached — the thing the caller records and hands to the next open",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				if h == nil {
					d.NoShare()
					return
				}
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
				if h == nil {
					d.NoShare()
					return
				}
				p := make([]byte, 16)
				n, missing, held, asOf, err := h.ReadAt(context.Background(), jdemo.StoryFile, 0, p)
				d.Codef("    n, missing, held, asOf, err := h.ReadAt(ctx, FILE, offset_0, readBuf_16B)")
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
				if h == nil {
					d.NoShare()
					return
				}
				d.Codef("    for ch := range h.Since(ctx, FILE, fromStart)       // journal.Version{} {")
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
				if h == nil {
					d.NoShare()
					return
				}
				v, err := h.Truncate(context.Background(), jdemo.StoryFile, 32)
				d.Codef("    v, err := h.Truncate(ctx, FILE, newSize_32B)")
				d.Stash(v, err)
			},
		},
		{
			Title:   "settle the removal",
			Caption: "Settle says the removal is accounted for (the demo holds no metadata; it calls it, as an engine that decided never to record would): the marker goes, the superseded ranges go with it — Since narrows to what is still owed",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchMarkers, Hint: "the marker goes"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				if h == nil {
					d.NoShare()
					return
				}
				d.Codef("    h.Settle(FILE, truncateVersion)      // journal.Version{Counter: %d}      // the truncate's version", d.Stashed.Counter)
				h.Settle(jdemo.StoryFile, d.Stashed)
				d.Codef("    for ch := range h.Since(ctx, FILE, fromStart)       // journal.Version{} {")
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
				if h == nil {
					d.NoShare()
					return
				}
				res, err := h.Reserve(8192, 2)
				if err != nil {
					d.Codef("    res, err := h.Reserve(reserveBytes_8KiB, writes_2)")
					return
				}
				v, werr := res.WriteAt(context.Background(), jdemo.StoryFile, 65536, []byte("reserved"), 0)
				d.Codef("    res, _ := h.Reserve(reserveBytes_8KiB, writes_2)")
				d.Codef("    v, err := res.WriteAt(ctx, FILE, offset_64KiB, []byte(\"reserved\"), mtimeNone)")
				d.Stash(v, werr)
				_ = res.Close()
			},
		},
		{
			Title:   "offload",
			Caption: "the engine offloads dirty extents to the remote tier, marking them in the index; the journal's own answer names what this build owes",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				if h == nil {
					d.NoShare()
					return
				}
				err := h.Offload(context.Background(), jdemo.StoryFile, d.Stashed, 0, 0,
					func(context.Context, journal.Offer, func([]journal.Extent) error) error {
						return nil
					})
				d.Codef("    err := h.Offload(ctx, FILE, throughVersion, limitBytes_none, widenBytes_none, sink)")
				d.Yellowf("    // → %v", err)
			},
		},
		{
			Title:   "verify",
			Caption: "the index checked against the segments, the report rather than the repair; the journal's own answer names what this build still owes",
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
