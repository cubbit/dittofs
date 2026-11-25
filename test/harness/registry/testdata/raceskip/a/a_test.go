package a

import "testing"

func TestLocalFlag(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts")
	}
}

func TestProfFlag(t *testing.T) {
	if prof.RaceEnabled {
		t.Skipf("allocation counts %d", 1)
	}
}

func TestRaceBranchWithoutSkip(t *testing.T) {
	if raceEnabled {
		n = 2
	}
}

func TestNestedSkip(t *testing.T) {
	t.Run("x", func(t *testing.T) {
		if raceEnabled {
			t.Skip("not at the top level")
		}
	})
}

func helper(t *testing.T) {
	if raceEnabled {
		t.Skip("not a test")
	}
}
