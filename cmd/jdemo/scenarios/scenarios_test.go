package scenarios

import (
	"strings"
	"testing"

	"github.com/cubbit/dittofs/cmd/jdemo/jdemo"
	"github.com/cubbit/dittofs/pkg/block/journal"
)

// TestStoryShapes pins each story's own exported content: the beats a
// presenter would walk, through the constructor surface alone.
func TestStoryShapes(t *testing.T) {
	cases := []struct {
		name  string
		want  int    // beats
		title string // a beat title unique to the story
	}{
		{"basic", 13, "make it durable"},
		{"crash", 7, "close — and the restart that cannot happen yet"}, // online + attach borrowed + 5 own
		{"limits", 8, "fill the budget — until the share says no"},     // online borrowed + 7 declared
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctor, ok := map[string]func() ([]jdemo.Beat, journal.Settings){
				"basic":  BasicStory,
				"crash":  CrashStory,
				"limits": LimitsStory,
			}[c.name]
			if !ok {
				t.Fatalf("story %q has no constructor", c.name)
			}
			beats, _ := ctor()
			if len(beats) != c.want {
				t.Errorf("story %q: got %d beats, want %d", c.name, len(beats), c.want)
			}
			found := false
			for _, b := range beats {
				if strings.Contains(b.Title, c.title) {
					found = true
				}
				if b.Title == "" || b.Run == nil {
					t.Errorf("story %q: a beat carries no title or no work", c.name)
				}
			}
			if !found {
				t.Errorf("story %q: no beat titled like %q", c.name, c.title)
			}
		})
	}
}
