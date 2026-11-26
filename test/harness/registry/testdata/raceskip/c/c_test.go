package c

import "testing"

func helper(t *testing.T) {
	if !prof.RaceEnabled {
		t.Fatalf("allocated")
	}
}

func TestUsesHelper(t *testing.T) { helper(t) }
