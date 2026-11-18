package main

import (
	"testing"
	"unicode"
)

// Each mapping sends a rune to a smaller rune of its own simple case-folding
// orbit, the least one, and never maps a rune that is already its orbit's
// least. It compares no Unicode version's output with the frozen table, which
// a newer Go may not reproduce and need not.
func TestPairs(t *testing.T) {
	ps := pairs()
	if len(ps) == 0 {
		t.Fatal("no mappings")
	}
	keys := make(map[rune]bool, len(ps))
	for _, p := range ps {
		keys[p[0]] = true
	}
	for _, p := range ps {
		r, least := p[0], p[1]
		if least >= r {
			t.Errorf("%U -> %U does not map to a smaller rune", r, least)
		}
		inOrbit := false
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < least {
				t.Errorf("%U -> %U, but %U in its orbit is smaller", r, least, f)
			}
			inOrbit = inOrbit || f == least
		}
		if !inOrbit {
			t.Errorf("%U -> %U is outside its simple case-folding orbit", r, least)
		}
		if keys[least] {
			t.Errorf("%U -> %U, but %U is mapped too: the least rune must fold to itself", r, least, least)
		}
	}
}
