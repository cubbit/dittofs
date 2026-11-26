//go:build slow

package b

import "testing"

func TestTagged(t *testing.T) {
	if prof.RaceEnabled {
		t.SkipNow()
	}
}
