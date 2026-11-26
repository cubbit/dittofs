package jdemo

import (
	"github.com/cubbit/dittofs/pkg/journal"
	"testing"
)

// registerTestStories seeds the registry for the package's own tests: the
// machinery package is linked without the scenarios package, whose init is
// what fills it in the binary. Only basic's shape is needed here.
func registerTestStories(t *testing.T) {
	t.Helper()
	stories["basic"] = Story{Beats: []Beat{
		{Title: "journal online", Caption: "the demo opened (or initialised) a real journal: one directory, a format file, a made spare pool; the version window is raised the way an engine records it",
			Run: func(d *Demo) {
				st := d.J.Stats()
				d.Printf("    dir:            %s\n", d.Dir)
				d.Printf("    state file:     %s\n", StatePath(d.Dir))
				d.Codef("      id, _ := journal.Init(ctx, dir, install, opts...)            // ran only on a fresh dir")
				d.Codef("      j, _ := journal.Open(ctx, dir, floor, id, nil, opts...)")
				d.Codef("      j.SetVersionLimit(journal.Version{Counter: %d})            // spares ready: %d", st.VersionLimit.Counter, st.Spares)
			}},
	}, Settings: demoSettings}
}

func TestWatchedEntriesAreCountedInGoodEnglish(t *testing.T) {
	w := Watching{Kind: WatchEntries, Hint: "entries"}
	for _, c := range []struct {
		delta int64
		want  string
	}{
		{1, "✓ index +1 entry"},
		{3, "✓ index +3 entries"},
		{-1, "✓ index -1 entry"},
		{-2, "✓ index -2 entries"},
	} {
		got := w.confirmed(journal.Stats{}, journal.Stats{ExtentCount: c.delta})
		if got != c.want {
			t.Errorf("entry delta %d: got %q, want %q", c.delta, got, c.want)
		}
	}
}
