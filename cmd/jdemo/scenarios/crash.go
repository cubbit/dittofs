package scenarios

import (
	"context"
	"os"

	"github.com/cubbit/dittofs/pkg/block/journal"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
)

// CrashStory is the durability half of the paper's story: what the journal
// promises when things die — retention, the sync mark, and the honest
// refusal to reopen what recovery has not yet learned to rebuild.
func CrashStory() ([]jdemo.Beat, journal.Settings) {
	writes := []jdemo.Beat{
		{
			Title:   "write it, then name its durability",
			Caption: "the record lands; Sync hands back the mark naming where durability reached — the receipt the caller records and the next open receives",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchEntries, Hint: "one held entry at v1"},
				{Kind: jdemo.WatchDirty, Hint: "the bytes go dirty"},
				{Kind: jdemo.WatchVersions, Hint: "a version drawn"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				v, werr := h.WriteAt(context.Background(), jdemo.StoryFile, 0, []byte("hello world"), 0)
				d.Codef("    v, err := h.WriteAt(ctx, FILE, 0, []byte(\"hello world\"), 0)")
				d.Stash(v, werr)
				marks, serr := h.Sync(context.Background(), jdemo.StoryFile)
				d.Codef("    marks, err := h.Sync(ctx, FILE)")
				if serr != nil {
					d.Yellowf("    // → %v", serr)
					return
				}
				for _, m := range marks {
					d.Yellowf("    // → {Stream:%d Segment:%d Through:%d Offset:%d Sequence:%d}",
						m.Stream, m.Segment, m.Through, m.Offset, m.Sequence)
				}
			},
		},
		{
			Title:   "overwrite — retention is the crash story",
			Caption: "a newer version supersedes part of the old: the index narrows and the superseded range stays RETAINED — the old bytes are findable until settle says metadata has them",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchEntries, Hint: "one more index entry"},
				{Kind: jdemo.WatchVersions, Hint: "another version drawn"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				v, werr := h.WriteAt(context.Background(), jdemo.StoryFile, 2, []byte("XY"), 0)
				d.Codef("    v, err := h.WriteAt(ctx, FILE, 2, []byte(\"XY\"), 0)")
				d.Stash(v, werr)
			},
		},
		{
			Title:   "truncate — durable the moment it returns",
			Caption: "the removal marker is written and fsynced before Truncate answers; crash here and the truncate still happened",
			Mutates: true,
			Watch: []jdemo.Watching{
				{Kind: jdemo.WatchMarkers, Hint: "a removal marker waits, unsettled"},
			},
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				v, terr := h.Truncate(context.Background(), jdemo.StoryFile, 32)
				d.Codef("    v, err := h.Truncate(ctx, FILE, 32)")
				d.Stash(v, terr)
			},
		},
		{
			Title:   "the feed a recovery replays",
			Caption: "Since yields everything a recovering caller needs from the journal: held extents and unsettled markers, version by version",
			Run: func(d *jdemo.Demo) {
				h := d.H[d.Tag]
				d.Codef("    for ch := range h.Since(ctx, FILE, journal.Version{}) {")
				for ch, serr := range h.Since(context.Background(), jdemo.StoryFile, journal.Version{}) {
					if serr != nil {
						d.Yellowf("    // → %v", serr)
						return
					}
					d.Printf("        %s\n", jdemo.ChangeLine(ch))
				}
				d.Codef("    }")
			},
		},
		{
			Title:   "close — and the restart that cannot happen yet",
			Caption: "the journal closes; the recovery pass that reopens it is not built. Open names its own absence — the roadmap, verbatim — and the beats that unlock it are the marks in your hand",
			Run: func(d *jdemo.Demo) {
				d.Codef("    j.Close()")
				if err := d.J.Close(); err != nil {
					d.Yellowf("    // → %v", err)
				}
				d.J = nil
				d.Yellowf("    // → journal closed before the restart attempt")
				// The identity Open expects is the one the state file
				// carried all along — a fresh demo's open path reads the
				// same file.
				st, lerr := jdemo.LoadState(jdemo.StatePath(d.Dir))
				if lerr != nil {
					d.Yellowf("    // → %v", lerr)
					return
				}
				d.Codef("    j2, err := journal.Open(ctx, dir, floor, id, marks, opts...)")
				_, oerr := journal.Open(context.Background(), d.Dir, journal.Version{},
					st.ID, nil, d.OpenOptions()...)
				if oerr != nil {
					d.Yellowf("    // → %v", oerr)
				}
				// The story's last word: the identity it would reopen.
				d.Printf("    state file: %s\n", jdemo.StatePath(d.Dir))
				if _, serr := os.Stat(jdemo.StatePath(d.Dir)); serr == nil {
					d.Yellowf("    // → present; the identity outlives the process")
				}
			},
		},
	}
	// The story opens with basic's online and attach beats — every story
	// needs a handle — then walks its own.
	basic, settings := BasicStory()
	beats := append([]jdemo.Beat{basic[0], basic[1]}, writes...)
	return beats, settings
}
