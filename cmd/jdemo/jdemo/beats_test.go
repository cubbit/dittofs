package jdemo

import (
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
